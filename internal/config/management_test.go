package config

import (
	"io"
	"testing"
)

func TestManagementKeysAndQuotaConfiguration(t *testing.T) {
	t.Setenv("CODEX_PROXY_API_KEY", "same-key")
	t.Setenv("CODEX_PROXY_MANAGEMENT_KEY", "same-key")
	if _, err := Parse([]string{}, io.Discard); err == nil {
		t.Fatal("shared inference and management key accepted")
	}
	t.Setenv("CODEX_PROXY_MANAGEMENT_KEY", "different-key")
	cfg, err := Parse([]string{}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.UsageURL != "https://chatgpt.com/backend-api/wham/usage" {
		t.Fatal("default quota URL is wrong")
	}
	for _, args := range [][]string{
		{"-quota-poll-interval", "1s"}, {"-quota-poll-interval", "25h"}, {"-history-max-samples", "0"},
		{"-history-max-samples", "10001"}, {"-usage-url", "http://example.com/usage"},
	} {
		if _, err := Parse(args, io.Discard); err == nil {
			t.Fatal("unsafe management settings accepted")
		}
	}
}
