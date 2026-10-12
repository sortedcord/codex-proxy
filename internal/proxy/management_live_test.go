package proxy

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sortedcord/codex-proxy/internal/config"
)

func TestManagementLive(t *testing.T) {
	if os.Getenv("CODEX_PROXY_MANAGEMENT_LIVE_TEST") != "1" {
		t.Skip("set CODEX_PROXY_MANAGEMENT_LIVE_TEST=1 to verify real Codex account quotas")
	}
	cfg, err := config.Parse([]string{}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	cfg.APIKey, cfg.ManagementKey = "live-inference-test", "live-management-test"
	cfg.HistoryDir = filepath.Join(t.TempDir(), "history")
	runtime, err := NewRuntime(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	identity, err := runtime.auth.Profile()
	if err != nil {
		t.Fatal("installed Codex account is unavailable")
	}
	deadline := time.Now().Add(40 * time.Second)
	for time.Now().Before(deadline) {
		snapshot, available, polling := runtime.status.Latest(identity.Scope())
		if available {
			t.Logf("live quota source %s; %d quota families; persisted observation", snapshot.Source, len(snapshot.Pools))
			response := managementRequest(
				runtime,
				"GET",
				"/status/limit",
				"",
				"live-management-test",
			)
			if response.Code != 200 {
				t.Fatal("live limit endpoint failed")
			}
			return
		}
		if polling.LastError != "" {
			t.Fatal("live quota observation failed; provider access or schema requires investigation")
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("live quota observation timed out")
}
