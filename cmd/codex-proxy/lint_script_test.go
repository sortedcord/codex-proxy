package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestLintScriptResolvesPinnedToolWithoutReplacingPATH(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("lint.sh is a POSIX development script")
	}
	shell, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("POSIX shell unavailable")
	}
	root := filepath.Join(t.TempDir(), "fixture with spaces")
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	writeExecutable := func(path, body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	legacy := filepath.Join(bin, "golangci-lint")
	legacyBody := "#!/bin/sh\necho 'golangci-lint has version 1.64.8 built with go'\n"
	writeExecutable(legacy, legacyBody)
	pinned := filepath.Join(root, "pinned-lint")
	writeExecutable(pinned, `#!/bin/sh
if [ "$1" = version ]; then
  echo 'golangci-lint has version 2.14.0 built with go'
  exit 0
fi
printf 'lint:%s\n' "$*" >> "$LINT_TEST_LOG"
`)
	writeExecutable(filepath.Join(bin, "go"), `#!/bin/sh
if [ "$1" != install ] || [ "$2" != github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.14.0 ]; then
  echo 'unexpected Go invocation in lint resolution test' >&2
  exit 1
fi
printf 'install\n' >> "$LINT_TEST_LOG"
cp "$LINT_TEST_PINNED" "$GOBIN/golangci-lint"
`)
	script, err := filepath.Abs(filepath.Join("..", "..", "scripts", "lint.sh"))
	if err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(root, "commands.log")
	cache := filepath.Join(root, "cache")
	run := func(override string, extraEnv ...string) ([]byte, error) {
		command := exec.Command(shell, script)
		command.Env = append(os.Environ(),
			"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
			"HOME="+root, "XDG_CACHE_HOME="+cache, "GOLANGCI_LINT="+override,
			"LINT_TEST_LOG="+logPath, "LINT_TEST_PINNED="+pinned,
		)
		command.Env = append(command.Env, extraEnv...)
		return command.CombinedOutput()
	}
	for _, override := range []string{"", "", pinned} {
		if output, err := run(override); err != nil {
			t.Fatalf("lint resolution failed: %v\n%s", err, output)
		}
	}
	log, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(log), "install\n"); got != 1 {
		t.Fatalf("bootstrap installs=%d, want 1 (then cache reuse)", got)
	}
	if got := strings.Count(string(log), "lint:run ./...\n"); got != 3 {
		t.Fatalf("lint runs=%d, want 3", got)
	}
	unchanged, err := os.ReadFile(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if string(unchanged) != legacyBody {
		t.Fatal("bootstrap overwrote the legacy PATH linter")
	}
	output, err := run(legacy)
	if err == nil {
		t.Fatal("explicit incompatible linter accepted")
	}
	if !strings.Contains(string(output), "GOLANGCI_LINT must point to golangci-lint v2.14.0") {
		t.Fatalf("wrong-version diagnostic missing: %s", output)
	}
	output, err = run("", "XDG_CACHE_HOME=relative-cache")
	if err == nil {
		t.Fatal("relative cache directory accepted")
	}
	if !strings.Contains(string(output), "absolute writable directory") {
		t.Fatalf("relative-cache diagnostic missing: %s", output)
	}
}
