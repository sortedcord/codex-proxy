package proxy

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/sortedcord/codex-proxy/internal/oauth"
)

func TestManagementDisabledAvoidsHistoryAndPolling(t *testing.T) {
	runtime := runtimeForTest(t, fakeAccountBackend(t, nil))
	cfg := runtime.cfg
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	cfg.ManagementKey = ""
	cfg.HistoryDir = filepath.Join(t.TempDir(), "should-not-exist")
	runtime, err := newRuntime(
		context.Background(),
		cfg,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		oauth.LoginOptions{},
	)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	if runtime.status != nil {
		t.Fatal("disabled management started quota polling")
	}
	if runtime.login != nil {
		t.Fatal("disabled management started login services")
	}
	if _, err := os.Stat(cfg.HistoryDir); !os.IsNotExist(err) {
		t.Fatal("disabled management created history")
	}
}

func TestLoginBodyValidationAndManagementIsolation(t *testing.T) {
	runtime := runtimeForTest(t, fakeAccountBackend(t, nil))
	for _, body := range []string{
		`null`, `{}`, `{"tokens":{}}`, `{"url":"x","code":"y"}`, `{} {}`, `{"code":"x","state":"wrong"}`,
	} {
		response := managementRequest(
			runtime,
			"POST",
			"/config/login",
			body,
			"management-test",
		)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("invalid login body status = %d", response.Code)
		}
	}
	response := managementRequest(
		runtime,
		"GET",
		"/v1/models",
		"",
		"management-test",
	)
	if response.Code != 401 {
		t.Fatal("management key was accepted as an inference key")
	}
	// A second runtime with the same auth path must fail before touching credentials.
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if _, err := NewRuntime(context.Background(), runtime.cfg, logger); err == nil {
		t.Fatal("two proxies acquired the same credential file")
	}
}
