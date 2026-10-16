package status

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/sortedcord/codex-proxy/internal/oauth"
	"github.com/sortedcord/codex-proxy/internal/quota"
	"github.com/terastal-hq/proxylib/poller"
)

type PollInfo = poller.PollInfo

type Options struct {
	Interval time.Duration
	Account  func() (oauth.Profile, error)
	Fetch    func(context.Context) (quota.Snapshot, error)
}

type Service struct {
	mu        sync.Mutex
	options   Options
	snapshots map[string]quota.Snapshot
	available map[string]bool
	polling   map[string]PollInfo
	ctx       context.Context
	cancel    context.CancelFunc
	trigger   chan struct{}
	done      chan struct{}
	closeOnce sync.Once
	closeErr  error
	closed    bool
}

func NewService(ctx context.Context, options Options) (*Service, error) {
	if ctx == nil || options.Account == nil || options.Fetch == nil {
		return nil, errors.New("status lifecycle, account and fetch functions are required")
	}
	if options.Interval <= 0 {
		return nil, errors.New("polling interval must be positive")
	}

	owned, cancel := context.WithCancel(ctx)
	service := &Service{
		options:   options,
		snapshots: map[string]quota.Snapshot{},
		available: map[string]bool{},
		polling:   map[string]PollInfo{},
		ctx:       owned,
		cancel:    cancel,
		trigger:   make(chan struct{}, 1),
		done:      make(chan struct{}),
	}
	go service.run()
	return service, nil
}

// Prepare is a no-op when historical metrics storage is disabled.
func (s *Service) Prepare(accountID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("quota service is closed")
	}
	return nil
}

func (s *Service) run() {
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

func (s *Service) poll() {
	if s.ctx.Err() != nil {
		return
	}
	attempt := time.Now().UTC()
	ctx, cancel := context.WithTimeout(s.ctx, 30*time.Second)
	defer cancel()

	sample, err := s.options.Fetch(ctx)

	// The account callback may wait for OAuth. Do not nest that lock inside mu.
	active, accountErr := s.options.Account()

	// Callers look observations up by identity scope (status API) or by the
	// ChatGPT account ID (per-account adapters), so record under both.
	var keys []string
	addKey := func(key string) {
		if key == "" {
			return
		}
		for _, existing := range keys {
			if existing == key {
				return
			}
		}
		keys = append(keys, key)
	}
	if accountErr == nil {
		addKey(active.Scope())
		addKey(active.AccountID)
	}
	if err == nil {
		addKey(sample.Scope())
		addKey(sample.AccountID)
	}
	if len(keys) == 0 {
		keys = append(keys, "default")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	for _, key := range keys {
		info := s.polling[key]
		info.IntervalSeconds = int64(s.options.Interval / time.Second)
		info.LastAttemptAt = &attempt
		if err != nil {
			info.LastError = "quota observation failed; upstream unavailable"
		} else {
			s.snapshots[key] = sample
			s.available[key] = true
			info.LastSuccessAt = &sample.ObservedAt
			info.LastError = ""
		}
		s.polling[key] = info
	}
}

// TriggerPoll signals an immediate polling attempt.
func (s *Service) TriggerPoll() {
	select {
	case s.trigger <- struct{}{}:
	default:
	}
}

// Latest returns the most recent observation, availability flag, and polling info.
func (s *Service) Latest(accountID string) (quota.Snapshot, bool, PollInfo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	info := s.polling[accountID]
	if !s.available[accountID] {
		return quota.Snapshot{}, false, info
	}
	return s.snapshots[accountID], true, info
}

// Stale reports whether the latest snapshot is older than two polling intervals or has an error.
func (s *Service) Stale(sample quota.Snapshot, polling PollInfo) bool {
	return poller.IsStale(sample.ObservedAt, s.options.Interval, polling.LastError, time.Now())
}

// Query returns empty results because historical metrics storage is disabled.
func (s *Service) Query(accountID string, query Query) (Result, error) {
	return Result{Samples: []quota.Snapshot{}}, nil
}

// Close gracefully stops the worker.
func (s *Service) Close() error {
	s.closeOnce.Do(func() {
		s.cancel()
		<-s.done
		s.mu.Lock()
		defer s.mu.Unlock()
		s.closed = true
	})
	return s.closeErr
}
