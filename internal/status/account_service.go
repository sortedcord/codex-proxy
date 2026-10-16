package status

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/sortedcord/codex-proxy/internal/quota"
)

// AccountOptions defines configuration for an account-scoped status service.
type AccountOptions struct {
	Dir        string
	Namespace  string
	MaxSamples int
	Interval   time.Duration
	Fetch      func(context.Context) (quota.Snapshot, error)
}

// AccountService manages background quota polling and status endpoints for one account.
type AccountService struct {
	mu        sync.Mutex
	options   AccountOptions
	snapshot  quota.Snapshot
	available bool
	polling   PollInfo
	ctx       context.Context
	cancel    context.CancelFunc
	trigger   chan struct{}
	done      chan struct{}
	closeOnce sync.Once
	closeErr  error
	closed    bool
}

// NewAccountService constructs and starts an account-scoped quota status service.
func NewAccountService(ctx context.Context, options AccountOptions) (*AccountService, error) {
	if ctx == nil || options.Fetch == nil {
		return nil, errors.New("status lifecycle and fetch function are required")
	}
	if options.Namespace == "" {
		return nil, errors.New("account namespace is required")
	}
	if options.Interval <= 0 {
		return nil, errors.New("polling interval must be positive")
	}

	owned, cancel := context.WithCancel(ctx)
	svc := &AccountService{
		options: options,
		ctx:     owned,
		cancel:  cancel,
		trigger: make(chan struct{}, 1),
		done:    make(chan struct{}),
	}
	svc.polling.IntervalSeconds = int64(options.Interval / time.Second)

	go svc.run()
	return svc, nil
}

// TriggerPoll signals an immediate polling attempt.
func (s *AccountService) TriggerPoll() {
	select {
	case s.trigger <- struct{}{}:
	default:
	}
}

func (s *AccountService) run() {
	defer close(s.done)
	ticker := time.NewTicker(s.options.Interval)
	defer ticker.Stop()

	s.poll()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			s.poll()
		case <-s.trigger:
			s.poll()
		}
	}
}

func (s *AccountService) poll() {
	if s.ctx.Err() != nil {
		return
	}
	attempt := time.Now().UTC()
	ctx, cancel := context.WithTimeout(s.ctx, 30*time.Second)
	defer cancel()

	sample, err := s.options.Fetch(ctx)

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}

	s.polling.IntervalSeconds = int64(s.options.Interval / time.Second)
	s.polling.LastAttemptAt = &attempt
	if err != nil {
		s.polling.LastError = "quota observation failed; upstream unavailable"
	} else {
		s.snapshot = sample
		s.available = true
		s.polling.LastSuccessAt = &sample.ObservedAt
		s.polling.LastError = ""
	}
}

// Latest returns the most recent observation, availability flag, and polling info.
func (s *AccountService) Latest() (quota.Snapshot, bool, PollInfo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	info := s.polling
	if !s.available {
		return quota.Snapshot{}, false, info
	}
	return s.snapshot, true, info
}

// Stale reports whether the latest snapshot is older than two polling intervals or has an error.
func (s *AccountService) Stale(sample quota.Snapshot, polling PollInfo) bool {
	return polling.LastError != "" || time.Now().After(sample.ObservedAt.Add(s.options.Interval).Add(s.options.Interval))
}

// Query returns empty results because historical metrics storage is disabled.
func (s *AccountService) Query(query Query) (Result, error) {
	return Result{Samples: []quota.Snapshot{}}, nil
}

// Close gracefully stops the worker.
func (s *AccountService) Close() error {
	s.closeOnce.Do(func() {
		s.cancel()
		<-s.done
		s.mu.Lock()
		defer s.mu.Unlock()
		s.closed = true
	})
	return s.closeErr
}

// ServeHTTP serves /status/limit and /status/usage for this account.
func (s *AccountService) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")

	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		w.WriteHeader(http.StatusMethodNotAllowed)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "status endpoints require GET"})
		return
	}

	path := r.URL.Path
	switch path {
	case "/status/limit":
		snapshot, available, polling := s.Latest()
		if !available {
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": map[string]string{
					"type":    "server_error",
					"code":    "quota_unavailable",
					"message": "quota has not been observed for the account",
				},
				"polling": polling,
			})
			return
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(struct {
			quota.Snapshot
			Polling PollInfo `json:"polling"`
			Stale   bool     `json:"stale"`
		}{
			Snapshot: snapshot,
			Polling:  polling,
			Stale:    s.Stale(snapshot, polling),
		})

	case "/status/usage":
		query, err := ParseQuery(r.URL.RawQuery)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"error": err.Error(),
			})
			return
		}
		result, _ := s.Query(query)
		_, _, polling := s.Latest()
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(struct {
			Result
			Polling PollInfo `json:"polling"`
		}{
			Result:  result,
			Polling: polling,
		})

	default:
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "unknown status endpoint"})
	}
}
