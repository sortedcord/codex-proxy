package quota

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sortedcord/codex-proxy/internal/oauth"
)

const sampleQuota = `{"account_id":"test-account","plan_type":"plus","rate_limit":{
	"allowed":true,"limit_reached":false,"primary_window":{"used_percent":12.5,
	"limit_window_seconds":18000,"reset_after_seconds":600}},"additional_rate_limits":[
	{"metered_feature":"codex_spark","limit_name":"Spark","rate_limit":{"allowed":false,
	"limit_reached":true,"secondary_window":{"used_percent":100,"limit_window_seconds":604800,"reset_at":2000000000}}}],
	"credits":{"has_credits":false,"unlimited":false,"balance":"0"}}`

func TestQuotaNativeWindowsAndFamilies(t *testing.T) {
	now := time.Now().UTC()
	snapshot, err := Parse([]byte(sampleQuota), "test-account", now)
	if err != nil {
		t.Fatal(err)
	}
	primary := snapshot.Pools["codex"].Primary
	if primary.RemainingPercent != 87.5 || primary.WindowSeconds != 18000 {
		t.Fatal("primary window was changed")
	}
	if primary.ResetAt == nil || !primary.ResetAt.Equal(now.Add(600*time.Second)) {
		t.Fatal("reset was lost")
	}
	if snapshot.Pools["codex"].Secondary != nil {
		t.Fatal("missing weekly quota was fabricated")
	}
	extra := snapshot.Pools["codex_spark"]
	if extra.Secondary.RemainingPercent != 0 || *extra.Allowed {
		t.Fatal("additional quota family was lost")
	}
	if snapshot.PlanType != "plus" || snapshot.Credits == nil || *snapshot.Credits.HasCredits {
		t.Fatal("plan/credits were lost")
	}
}

func TestQuotaRejectsUntrustedOrMissingData(t *testing.T) {
	for _, body := range []string{
		`{}`, `null`, `[]`, `{"rate_limit":{"primary_window":{"used_percent":10}}}`,
		strings.Replace(
			sampleQuota,
			"test-account",
			"another-account",
			1,
		),
		strings.Replace(
			sampleQuota,
			"12.5",
			"101",
			1,
		),
		strings.Replace(
			sampleQuota,
			"18000",
			"-1",
			1,
		),
	} {
		if _, err := Parse([]byte(body), "test-account", time.Now()); err == nil {
			t.Errorf("invalid quota accepted")
		}
	}
}

func TestQuotaHTTPBoundaryAndSanitizedErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		correctToken := request.Header.Get("Authorization") == "Bearer dummy-access"
		correctAccount := request.Header.Get("Chatgpt-Account-Id") == "test-account"
		if !correctToken || !correctAccount {
			t.Error("quota credential boundary failed")
		}
		_, _ = w.Write([]byte(sampleQuota))
	}))
	defer server.Close()
	fetcher := &Fetcher{Client: server.Client(), Endpoint: server.URL, UserAgent: "test", ClientVersion: "test"}
	creds := oauth.Credentials{AccessToken: "dummy-access", AccountID: "test-account"}
	if _, err := fetcher.Fetch(context.Background(), creds); err != nil {
		t.Fatal(err)
	}
	errorServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(403)
		_ = json.NewEncoder(w).Encode(map[string]string{"private": "token-must-not-appear"})
	}))
	defer errorServer.Close()
	fetcher.Endpoint = errorServer.URL
	_, err := fetcher.Fetch(context.Background(), creds)
	if err == nil || strings.Contains(err.Error(), "token-must-not-appear") {
		t.Fatal("quota error exposed upstream body")
	}
}

func FuzzQuotaJSON(f *testing.F) {
	f.Add(sampleQuota)
	f.Add(`{"rate_limit":null}`)
	f.Fuzz(func(t *testing.T, body string) { _, _ = Parse([]byte(body), "test-account", time.Now()) })
}
