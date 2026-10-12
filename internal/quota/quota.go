// Package quota reads provider-reported Codex quotas, never estimates them from tokens.
package quota

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/sortedcord/codex-proxy/internal/httputil"
	"github.com/sortedcord/codex-proxy/internal/oauth"
)

const maxResponseBytes = 1 << 20

type Window struct {
	UsedPercent      float64    `json:"used_percent"`
	RemainingPercent float64    `json:"remaining_percent"`
	WindowSeconds    int64      `json:"window_seconds"`
	ResetAt          *time.Time `json:"reset_at"`
}

type Limit struct {
	ID           string  `json:"id"`
	DisplayName  string  `json:"display_name"`
	Allowed      *bool   `json:"allowed"`
	LimitReached *bool   `json:"limit_reached"`
	Primary      *Window `json:"primary"`
	Secondary    *Window `json:"secondary"`
}

type Credits struct {
	HasCredits *bool   `json:"has_credits"`
	Unlimited  *bool   `json:"unlimited"`
	Balance    *string `json:"balance"`
}

type Snapshot struct {
	ObservedAt  time.Time        `json:"observed_at"`
	AccountID   string           `json:"account_id"`
	IdentityKey string           `json:"identity_key,omitempty"`
	Source      string           `json:"source"`
	PlanType    string           `json:"plan_type,omitempty"`
	Pools       map[string]Limit `json:"pools"`
	Credits     *Credits         `json:"credits"`
}

func (s Snapshot) Scope() string {
	return oauth.AccountScope(s.AccountID, s.IdentityKey)
}

type Fetcher struct {
	Client        *http.Client
	Endpoint      string
	UserAgent     string
	ClientVersion string
}

func (f *Fetcher) Fetch(ctx context.Context, creds oauth.Credentials) (Snapshot, error) {
	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodGet,
		f.Endpoint,
		nil,
	)
	if err != nil {
		return Snapshot{}, errors.New("cannot construct quota request")
	}
	req.Header.Set("Authorization", "Bearer "+creds.AccessToken)
	req.Header.Set("Chatgpt-Account-Id", creds.AccountID)
	req.Header.Set("User-Agent", f.UserAgent)
	req.Header.Set("Version", f.ClientVersion)
	req.Header.Set("Originator", "codex_cli_rs")
	req.Header.Set("Accept", "application/json")
	resp, err := f.Client.Do(req)
	if err != nil {
		return Snapshot{}, errors.New("quota upstream transport failure")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return Snapshot{}, fmt.Errorf("quota upstream returned HTTP %d", resp.StatusCode)
	}
	body, err := httputil.ReadBounded(resp.Body, maxResponseBytes)
	if err != nil {
		return Snapshot{}, errors.New("cannot read bounded quota response")
	}
	snapshot, err := Parse(body, creds.AccountID, time.Now().UTC())
	if err == nil {
		snapshot.IdentityKey = creds.IdentityKey
	}
	return snapshot, err
}

type wireWindow struct {
	UsedPercent       *float64 `json:"used_percent"`
	WindowSeconds     *int64   `json:"limit_window_seconds"`
	ResetAt           *int64   `json:"reset_at"`
	ResetAfterSeconds *int64   `json:"reset_after_seconds"`
}

type wireLimit struct {
	Allowed      *bool       `json:"allowed"`
	LimitReached *bool       `json:"limit_reached"`
	Primary      *wireWindow `json:"primary_window"`
	Secondary    *wireWindow `json:"secondary_window"`
}

func Parse(body []byte, accountID string, observedAt time.Time) (Snapshot, error) {
	payload := struct {
		AccountID  string     `json:"account_id"`
		PlanType   string     `json:"plan_type"`
		RateLimit  *wireLimit `json:"rate_limit"`
		Credits    *Credits   `json:"credits"`
		Additional []struct {
			ID        string     `json:"metered_feature"`
			Name      string     `json:"limit_name"`
			RateLimit *wireLimit `json:"rate_limit"`
		} `json:"additional_rate_limits"`
	}{}
	if err := json.Unmarshal(body, &payload); err != nil {
		return Snapshot{}, errors.New("invalid quota JSON")
	}
	if payload.AccountID != "" && payload.AccountID != accountID {
		return Snapshot{}, errors.New("quota response account mismatch")
	}
	if accountID == "" || observedAt.IsZero() {
		return Snapshot{}, errors.New("quota observation identity and timestamp are required")
	}
	// A random successful JSON object is not evidence of available quota.
	hasData := payload.RateLimit != nil || payload.Credits != nil || len(payload.Additional) > 0
	if !hasData {
		return Snapshot{}, errors.New("quota response has no reported limits")
	}
	if len(payload.Additional) > 128 {
		return Snapshot{}, errors.New("too many quota families")
	}
	snapshot := Snapshot{
		ObservedAt: observedAt, AccountID: accountID, Source: "wham/usage", PlanType: payload.PlanType,
		Pools: map[string]Limit{}, Credits: payload.Credits,
	}
	if payload.RateLimit != nil {
		limit, err := normalizeLimit(
			"codex",
			"Codex",
			payload.RateLimit,
			observedAt,
		)
		if err != nil {
			return Snapshot{}, err
		}
		snapshot.Pools["codex"] = limit
	}
	for _, extra := range payload.Additional {
		if extra.ID == "" || len(extra.ID) > 128 || len(extra.Name) > 256 {
			return Snapshot{}, errors.New("invalid quota family identity")
		}
		if _, exists := snapshot.Pools[extra.ID]; exists {
			return Snapshot{}, errors.New("duplicate quota family identity")
		}
		limit, err := normalizeLimit(
			extra.ID,
			extra.Name,
			extra.RateLimit,
			observedAt,
		)
		if err != nil {
			return Snapshot{}, err
		}
		snapshot.Pools[extra.ID] = limit
	}
	return snapshot, nil
}

func normalizeLimit(id, name string, input *wireLimit, observedAt time.Time) (Limit, error) {
	result := Limit{ID: id, DisplayName: name}
	if input == nil {
		return result, nil
	}
	result.Allowed, result.LimitReached = input.Allowed, input.LimitReached
	var err error
	result.Primary, err = normalizeWindow(input.Primary, observedAt)
	if err != nil {
		return Limit{}, err
	}
	result.Secondary, err = normalizeWindow(input.Secondary, observedAt)
	return result, err
}

func normalizeWindow(input *wireWindow, observedAt time.Time) (*Window, error) {
	if input == nil {
		return nil, nil
	}
	missing := input.UsedPercent == nil || input.WindowSeconds == nil
	if missing {
		return nil, errors.New("quota window is missing required measurements")
	}
	invalidPercent := *input.UsedPercent < 0 || *input.UsedPercent > 100
	if invalidPercent || *input.WindowSeconds <= 0 {
		return nil, errors.New("invalid quota window measurements")
	}
	result := &Window{
		UsedPercent: *input.UsedPercent, RemainingPercent: 100 - *input.UsedPercent,
		WindowSeconds: *input.WindowSeconds,
	}
	if input.ResetAt != nil && *input.ResetAt > 0 {
		reset := time.Unix(*input.ResetAt, 0).UTC()
		result.ResetAt = &reset
	} else if input.ResetAfterSeconds != nil && *input.ResetAfterSeconds >= 0 {
		if *input.ResetAfterSeconds > 366*24*60*60 {
			return nil, errors.New("quota reset interval is too large")
		}
		reset := observedAt.Add(time.Duration(*input.ResetAfterSeconds) * time.Second)
		result.ResetAt = &reset
	}
	return result, nil
}
