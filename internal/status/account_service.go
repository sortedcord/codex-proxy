package status

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/sortedcord/codex-proxy/internal/quota"
	"github.com/terastal-hq/proxylib/poller"
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
	options AccountOptions
	poller  *poller.Poller[quota.Snapshot]
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

	p, err := poller.New(options.Interval, options.Fetch, func(s quota.Snapshot) time.Time {
		return s.ObservedAt
	})
	if err != nil {
		return nil, err
	}
	if err := p.Start(ctx); err != nil {
		return nil, err
	}
	return &AccountService{
		options: options,
		poller:  p,
	}, nil
}

// TriggerPoll signals an immediate polling attempt.
func (s *AccountService) TriggerPoll() {
	s.poller.TriggerPoll()
}

// Latest returns the most recent observation, availability flag, and polling info.
func (s *AccountService) Latest() (quota.Snapshot, bool, PollInfo) {
	return s.poller.Latest()
}

// Stale reports whether the latest snapshot is older than two polling intervals or has an error.
func (s *AccountService) Stale(sample quota.Snapshot, polling PollInfo) bool {
	return s.poller.Stale(sample.ObservedAt, polling)
}

// Query returns empty results because historical metrics storage is disabled.
func (s *AccountService) Query(query Query) (Result, error) {
	return Result{Samples: []quota.Snapshot{}}, nil
}

// Close gracefully stops the worker.
func (s *AccountService) Close() error {
	return s.poller.Close()
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
