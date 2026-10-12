package status

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/sortedcord/codex-proxy/internal/oauth"
	"github.com/sortedcord/codex-proxy/internal/quota"
	"github.com/sortedcord/codex-proxy/internal/storage"
)

type PollInfo struct {
	IntervalSeconds int64      `json:"interval_seconds"`
	LastAttemptAt   *time.Time `json:"last_attempt_at"`
	LastSuccessAt   *time.Time `json:"last_success_at"`
	LastError       string     `json:"last_error,omitempty"`
}

type Options struct {
	Dir        string
	MaxSamples int
	Interval   time.Duration
	Account    func() (oauth.Profile, error)
	Fetch      func(context.Context) (quota.Snapshot, error)
}

type Service struct {
	mu        sync.Mutex
	options   Options
	histories map[string]*History
	polling   map[string]PollInfo
	lock      *storage.Lock
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
	if err := storage.PrivateDir(options.Dir); err != nil {
		return nil, err
	}
	lock, err := storage.Acquire(options.Dir + "/status.lock")
	if err != nil {
		return nil, err
	}
	owned, cancel := context.WithCancel(ctx)
	service := &Service{
		options: options, histories: map[string]*History{}, polling: map[string]PollInfo{},
		lock: lock, ctx: owned, cancel: cancel, trigger: make(chan struct{}, 1), done: make(chan struct{}),
	}
	if account, err := options.Account(); err == nil {
		if _, err := service.history(account.Scope()); err != nil {
			cancel()
			_ = lock.Close()
			return nil, err
		}
	}
	go service.run()
	return service, nil
}

// Prepare validates the candidate's journal before committing login credentials.
func (s *Service) Prepare(accountID string) error {
	_, err := s.history(accountID)
	return err
}

func (s *Service) history(accountID string) (*History, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, errors.New("quota service is closed")
	}
	if history := s.histories[accountID]; history != nil {
		return history, nil
	}
	// Retain at most two accounts in memory. Old journals remain on disk and can
	// be reopened on a later login; never evict the currently active account.
	if len(s.histories) >= 2 {
		active, _ := s.options.Account()
		for id, history := range s.histories {
			if id == active.Scope() {
				continue
			}
			if err := history.Close(); err != nil {
				return nil, err
			}
			delete(s.histories, id)
			delete(s.polling, id)
			break
		}
	}
	history, err := OpenHistory(s.options.Dir, accountID, s.options.MaxSamples)
	if err != nil {
		return nil, err
	}
	s.histories[accountID] = history
	return history, nil
}

func (s *Service) TriggerPoll() {
	select {
	case s.trigger <- struct{}{}:
	default:
	}
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
	account, err := s.options.Account()
	if err != nil || s.ctx.Err() != nil {
		return
	}
	attempt := time.Now().UTC()
	ctx, cancel := context.WithTimeout(s.ctx, 30*time.Second)
	defer cancel()
	sample, err := s.options.Fetch(ctx)
	if err == nil {
		// Fetch may have started under another account after the initial identity
		// read. Attribute the observation to the credentials that actually fetched it.
		account.AccountID = sample.AccountID
		account.IdentityKey = sample.IdentityKey
		history, openErr := s.history(sample.Scope())
		if openErr != nil {
			err = openErr
		} else {
			err = history.Append(sample)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	info := s.polling[account.Scope()]
	info.IntervalSeconds = int64(s.options.Interval / time.Second)
	info.LastAttemptAt = &attempt
	if err != nil {
		// Filesystem paths, responses and tokens never enter status errors.
		info.LastError = "quota observation failed; upstream or history storage unavailable"
	} else {
		info.LastSuccessAt = &sample.ObservedAt
		info.LastError = ""
	}
	s.polling[account.Scope()] = info
}

func (s *Service) Latest(accountID string) (quota.Snapshot, bool, PollInfo) {
	s.mu.Lock()
	history := s.histories[accountID]
	info := s.polling[accountID]
	s.mu.Unlock()
	info.IntervalSeconds = int64(s.options.Interval / time.Second)
	if history == nil {
		return quota.Snapshot{}, false, info
	}
	latest, available := history.Latest()
	if !available {
		return quota.Snapshot{}, false, info
	}
	if info.LastSuccessAt == nil {
		info.LastSuccessAt = &latest.ObservedAt
	}
	return latest, true, info
}

func (s *Service) Stale(sample quota.Snapshot, polling PollInfo) bool {
	return polling.LastError != "" || time.Now().After(sample.ObservedAt.Add(s.options.Interval).Add(s.options.Interval))
}

func (s *Service) Query(accountID string, query Query) (Result, error) {
	history, err := s.history(accountID)
	if err != nil {
		return Result{}, err
	}
	return queryHistory(history.Samples(), query), nil
}

func (s *Service) Close() error {
	s.closeOnce.Do(func() {
		s.cancel()
		<-s.done
		s.mu.Lock()
		defer s.mu.Unlock()
		s.closed = true
		for _, history := range s.histories {
			s.closeErr = errors.Join(s.closeErr, history.Close())
		}
		s.closeErr = errors.Join(s.closeErr, s.lock.Close())
	})
	return s.closeErr
}
