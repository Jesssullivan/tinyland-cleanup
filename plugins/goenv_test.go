package plugins

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func fakeGo(t *testing.T, body string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell fake toolchain")
	}
	dir := t.TempDir()
	script := "#!/bin/sh\n" + body + "\n"
	if err := os.WriteFile(filepath.Join(dir, "go"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestGoEnvOutputReturnsTheProbeAnswer(t *testing.T) {
	fakeGo(t, `[ "$1 $2" = "env GOCACHE" ] && echo /fixture/go-build`)
	out, err := goEnvOutput(context.Background(), "GOCACHE")
	if err != nil {
		t.Fatalf("goEnvOutput: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != "/fixture/go-build" {
		t.Fatalf("got %q", got)
	}
}

func TestGoEnvOutputGivesUpOnAHungChild(t *testing.T) {
	fakeGo(t, "exec sleep 30")
	oldTimeout, oldDelay := goEnvProbeTimeout, goEnvProbeWaitDelay
	goEnvProbeTimeout, goEnvProbeWaitDelay = 200*time.Millisecond, 200*time.Millisecond
	t.Cleanup(func() { goEnvProbeTimeout, goEnvProbeWaitDelay = oldTimeout, oldDelay })

	start := time.Now()
	if _, err := goEnvOutput(context.Background(), "GOCACHE"); err == nil {
		t.Fatal("expected an error from a probe that never answers")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("probe was not bounded: took %s", elapsed)
	}
}

func TestDevArtifactsGoCacheDirIsEmptyWhenTheProbeHangs(t *testing.T) {
	fakeGo(t, "exec sleep 30")
	oldTimeout := goEnvProbeTimeout
	goEnvProbeTimeout = 200 * time.Millisecond
	t.Cleanup(func() { goEnvProbeTimeout = oldTimeout })

	p := &DevArtifactsPlugin{}
	if dir := p.getGoCacheDir(context.Background()); dir != "" {
		t.Fatalf("expected no Go cache dir from a hung probe, got %q", dir)
	}
}
