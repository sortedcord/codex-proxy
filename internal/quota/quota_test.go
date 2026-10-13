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
	fiveHour := snapshot.Pools["codex"].FiveHour
	if fiveHour == nil {
		t.Fatal("five_hour window missing")
	}
	if fiveHour.RemainingPercent != 87.5 {
		t.Fatalf("remaining percentage=%v, want 87.5", fiveHour.RemainingPercent)
	}
	if fiveHour.WindowSeconds != 18000 {
		t.Fatalf("window duration=%d, want 18000", fiveHour.WindowSeconds)
	}
	if fiveHour.ResetAt == nil {
		t.Fatal("five_hour reset timestamp missing")
	}
	if want := now.Add(600 * time.Second); !fiveHour.ResetAt.Equal(want) {
		t.Fatalf("reset timestamp=%s, want %s", fiveHour.ResetAt, want)
	}
	if snapshot.Pools["codex"].Weekly != nil {
		t.Fatal("missing weekly quota was fabricated")
	}
	extra := snapshot.Pools["codex_spark"]
	if extra.Weekly == nil {
		t.Fatal("additional weekly window missing")
	}
	if extra.Weekly.RemainingPercent != 0 {
		t.Fatalf("additional remaining percentage=%v, want 0", extra.Weekly.RemainingPercent)
	}
	if extra.Allowed == nil {
		t.Fatal("additional allowed flag missing")
	}
	if *extra.Allowed {
		t.Fatal("exhausted additional family reported as allowed")
	}
	if snapshot.PlanType != "plus" {
		t.Fatalf("plan=%q, want plus", snapshot.PlanType)
	}
	if snapshot.Credits == nil {
		t.Fatal("credits metadata missing")
	}
	if snapshot.Credits.HasCredits == nil {
		t.Fatal("has_credits flag missing")
	}
	if *snapshot.Credits.HasCredits {
		t.Fatal("account incorrectly reported available credits")
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
		if !correctToken {
			t.Error("quota access token boundary failed")
		}
		if !correctAccount {
			t.Error("quota account boundary failed")
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
	if err == nil {
		t.Fatal("rejected quota request returned no error")
	}
	if strings.Contains(err.Error(), "token-must-not-appear") {
		t.Fatal("quota error exposed upstream body")
	}
}

func FuzzQuotaJSON(f *testing.F) {
	f.Add(sampleQuota)
	f.Add(`{"rate_limit":null}`)
	f.Fuzz(func(t *testing.T, body string) { _, _ = Parse([]byte(body), "test-account", time.Now()) })
}
