package main

import (
	"bytes"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

func TestBuiltExecutableVersion(t *testing.T) {
	if testing.Short() {
		t.Skip("builds an executable")
	}
	name := "codex-proxy"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	binary := filepath.Join(t.TempDir(), name)
	pkg := "github.com/sortedcord/codex-proxy/internal/buildinfo"
	flags := "-X " + pkg + ".Version=v0.5.0 -X " + pkg + ".Commit=testcommit -X " + pkg + ".Date=2026-10-13T00:00:00Z"
	build := exec.Command("go", "build", "-trimpath", "-ldflags", flags, "-o", binary, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, output)
	}
	command := exec.Command(binary, "version")
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := command.Run(); err != nil {
		t.Fatalf("version: %v\n%s", err, &stderr)
	}
	want := "codex-proxy v0.5.0 (commit=testcommit, date=2026-10-13T00:00:00Z)\n"
	if stdout.String() != want || stderr.Len() != 0 {
		t.Fatalf("stdout=%q, stderr=%q", stdout.String(), stderr.String())
	}
}
