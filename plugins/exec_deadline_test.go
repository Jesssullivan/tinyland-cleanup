package plugins

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"
)

func shortKillGrace(t *testing.T) {
	t.Helper()
	previous := execKillGrace
	execKillGrace = 300 * time.Millisecond
	t.Cleanup(func() { execKillGrace = previous })
}

// TestCommandContextKillsChildThatHangsForever is the PZM failure: a child
// that never returns, started by a plugin whose context has no deadline.
func TestCommandContextKillsChildThatHangsForever(t *testing.T) {
	shortKillGrace(t)
	scope := NewRunScope("cache", time.Time{}, 200*time.Millisecond, 0)
	ctx := WithRunScope(context.Background(), scope)

	started := time.Now()
	err := commandContext(ctx, "sleep", "3600").Run()
	elapsed := time.Since(started)

	if err == nil {
		t.Fatal("a child that hangs forever returned no error")
	}
	if elapsed > 5*time.Second {
		t.Fatalf("hung child held the caller for %s; the deadline was 200ms", elapsed)
	}
	if got := scope.Wedged(); len(got) != 1 || got[0] != "wedged:sleep 3600" {
		t.Fatalf("Wedged() = %v, want [wedged:sleep 3600]", got)
	}
	if last, _ := scope.LastCommand(); last != "sleep 3600" {
		t.Fatalf("LastCommand() = %q, want %q", last, "sleep 3600")
	}
}

func TestCommandContextDefaultDeadlineWithoutScope(t *testing.T) {
	cmd := commandContext(context.Background(), "true")
	if cmd.Cancel == nil || cmd.WaitDelay <= 0 {
		t.Fatal("a command built with no scope has no cancel hook or wait delay")
	}
	if err := cmd.Run(); err != nil {
		t.Fatalf("true failed: %v", err)
	}
	var scope *RunScope
	if got := scope.timeoutFor(execProbe); got != DefaultExecTimeout {
		t.Fatalf("probe default = %s, want %s", got, DefaultExecTimeout)
	}
	if got := scope.timeoutFor(execReclaim); got != DefaultExecReclaimTimeout {
		t.Fatalf("reclaim default = %s, want %s", got, DefaultExecReclaimTimeout)
	}
}

// A child that ignores SIGTERM is killed outright after the grace period.
func TestCommandContextKillsChildThatIgnoresSIGTERM(t *testing.T) {
	shortKillGrace(t)
	scope := NewRunScope("cache", time.Time{}, 200*time.Millisecond, 0)
	ctx := WithRunScope(context.Background(), scope)

	started := time.Now()
	err := commandContext(ctx, "sh", "-c", `trap "" TERM; while :; do sleep 1; done`).Run()
	if err == nil {
		t.Fatal("a child that ignores SIGTERM returned no error")
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("child ignoring SIGTERM held the caller for %s", elapsed)
	}
}

// A grandchild that inherits the output pipe must not hold Output() open
// after the child is gone.
func TestCommandContextReturnsWhenGrandchildHoldsThePipe(t *testing.T) {
	shortKillGrace(t)
	scope := NewRunScope("cache", time.Time{}, 200*time.Millisecond, 0)
	ctx := WithRunScope(context.Background(), scope)

	started := time.Now()
	_, err := commandContext(ctx, "sh", "-c", "sleep 30 & wait").Output()
	if err == nil {
		t.Fatal("expected an error from a command cut off at its deadline")
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("grandchild held Output() for %s", elapsed)
	}
}

// After one program hangs, the same plugin run does not wait out another
// deadline on it; a VM restart is still attempted.
func TestCommandContextRefusesBinaryThatAlreadyHungInThisRun(t *testing.T) {
	shortKillGrace(t)
	scope := NewRunScope("cache", time.Time{}, 150*time.Millisecond, 150*time.Millisecond)
	ctx := WithRunScope(context.Background(), scope)

	if err := commandContext(ctx, "sleep", "3600").Run(); err == nil {
		t.Fatal("first hung command returned no error")
	}
	started := time.Now()
	err := reclaimCommandContext(ctx, "sleep", "3600").Run()
	if err == nil || !strings.Contains(err.Error(), "not started") {
		t.Fatalf("second command on the hung binary: err = %v, want a not-started refusal", err)
	}
	if elapsed := time.Since(started); elapsed > 100*time.Millisecond {
		t.Fatalf("refusal took %s; it must not start the program", elapsed)
	}
	if err := commandContext(ctx, "true").Run(); err != nil {
		t.Fatalf("a different program was refused: %v", err)
	}
	if err := restartCommandContext(ctx, "sleep", "0").Run(); err != nil {
		t.Fatalf("a restart command on the hung binary was refused: %v", err)
	}
	// A new run of the same plugin starts clean.
	next := WithRunScope(context.Background(), NewRunScope("cache", time.Time{}, 0, 0))
	if err := commandContext(next, "sleep", "0").Run(); err != nil {
		t.Fatalf("the next run inherited the refusal: %v", err)
	}
}

// A deadline the caller set is used as given and is not treated as a hang.
func TestCommandContextHonorsCallerDeadline(t *testing.T) {
	shortKillGrace(t)
	scope := NewRunScope("dev-artifacts", time.Now().Add(time.Hour), 30*time.Second, 0)
	parent, cancelParent := context.WithDeadline(WithRunScope(context.Background(), scope), scope.deadline)
	defer cancelParent()

	ctx, cancel := context.WithTimeout(parent, 150*time.Millisecond)
	defer cancel()
	started := time.Now()
	if err := commandContext(ctx, "sleep", "3600").Run(); err == nil {
		t.Fatal("expected the caller's deadline to end the command")
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("caller deadline of 150ms took %s", elapsed)
	}
	if got := scope.Wedged(); len(got) != 0 {
		t.Fatalf("a caller-chosen deadline was recorded as a hang: %v", got)
	}
	if err := commandContext(parent, "sleep", "0").Run(); err != nil {
		t.Fatalf("sleep was refused after a caller-chosen deadline: %v", err)
	}
}

// The plugin's own watchdog deadline is not a caller deadline: the class
// default still applies under it.
func TestCommandContextAppliesClassDeadlineUnderWatchdogDeadline(t *testing.T) {
	shortKillGrace(t)
	scope := NewRunScope("cache", time.Now().Add(time.Hour), 150*time.Millisecond, 0)
	ctx, cancel := context.WithDeadline(WithRunScope(context.Background(), scope), scope.deadline)
	defer cancel()

	started := time.Now()
	if err := commandContext(ctx, "sleep", "3600").Run(); err == nil {
		t.Fatal("expected the probe deadline to end the command")
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("probe deadline of 150ms took %s under a one-hour watchdog deadline", elapsed)
	}
	if got := scope.Wedged(); len(got) != 1 {
		t.Fatalf("Wedged() = %v, want one entry", got)
	}
}

// restartCommandContext must run after the plugin's context has ended.
func TestRestartCommandRunsAfterPluginContextEnded(t *testing.T) {
	ctx, cancel := context.WithCancel(WithRunScope(context.Background(), NewRunScope("podman", time.Time{}, 0, 0)))
	cancel()
	if err := offlineCommandContext(ctx, "true").Run(); err == nil {
		t.Fatal("an offline command started on an ended context")
	}
	if err := restartCommandContext(ctx, "true").Run(); err != nil {
		t.Fatalf("restart command did not run on an ended context: %v", err)
	}
}

// installHangingGo puts a `go` on PATH that records every invocation and
// then never returns: it stops itself, so even SIGTERM stays pending, which
// is as stuck as a child in an uninterruptible wait.
func installHangingGo(t *testing.T) (marker string) {
	t.Helper()
	bin := t.TempDir()
	marker = filepath.Join(t.TempDir(), "go-was-started")
	script := "#!/bin/sh\necho \"$@\" >> " + marker + "\nkill -STOP $$\nwhile :; do sleep 1; done\n"
	if err := os.WriteFile(filepath.Join(bin, "go"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return marker
}

func TestGoBuildCacheDirNeverStartsGo(t *testing.T) {
	marker := installHangingGo(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("GOENV", "")

	t.Run("environment", func(t *testing.T) {
		t.Setenv("GOCACHE", "/srv/cache/go-build")
		if got := goBuildCacheDir(); got != "/srv/cache/go-build" {
			t.Fatalf("goBuildCacheDir() = %q", got)
		}
	})
	t.Run("off", func(t *testing.T) {
		t.Setenv("GOCACHE", "off")
		if got := goBuildCacheDir(); got != "" {
			t.Fatalf("goBuildCacheDir() = %q, want empty for GOCACHE=off", got)
		}
	})
	t.Run("relative is rejected", func(t *testing.T) {
		t.Setenv("GOCACHE", "relative/cache")
		if got := goBuildCacheDir(); got != "" {
			t.Fatalf("goBuildCacheDir() = %q, want empty for a relative GOCACHE", got)
		}
	})
	t.Run("go env file", func(t *testing.T) {
		t.Setenv("GOCACHE", "")
		envFile := filepath.Join(t.TempDir(), "env")
		if err := os.WriteFile(envFile, []byte("GOFLAGS=-mod=mod\nGOCACHE=/var/tmp/from-go-env-w\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Setenv("GOENV", envFile)
		if got := goBuildCacheDir(); got != "/var/tmp/from-go-env-w" {
			t.Fatalf("goBuildCacheDir() = %q", got)
		}
	})
	t.Run("default", func(t *testing.T) {
		t.Setenv("GOCACHE", "")
		t.Setenv("GOENV", "off")
		want := filepath.Join(home, ".cache", "go-build")
		if runtime.GOOS == "darwin" {
			want = filepath.Join(home, "Library", "Caches", "go-build")
		}
		if got := goBuildCacheDir(); got != want {
			t.Fatalf("goBuildCacheDir() = %q, want %q", got, want)
		}
	})
	t.Run("dev-artifacts lookup", func(t *testing.T) {
		t.Setenv("GOCACHE", "/srv/cache/go-build")
		if got := NewDevArtifactsPlugin().getGoCacheDir(context.Background()); got != "/srv/cache/go-build" {
			t.Fatalf("getGoCacheDir() = %q", got)
		}
	})

	if data, err := os.ReadFile(marker); err == nil {
		t.Fatalf("`go` was started to find the build cache: go %s", strings.TrimSpace(string(data)))
	}
}

// The Go build cache cleaner with a `go` that hangs on every invocation: the
// cache directory is found without starting it, `go clean` is cut off at its
// deadline, and the caller gets control back.
func TestGoBuildCacheCleanSurvivesHangingGo(t *testing.T) {
	shortKillGrace(t)
	marker := installHangingGo(t)
	cache := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cache, "ab"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cache, "ab", "abcdef-a"), make([]byte, 4096), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOCACHE", cache)

	// The stub must get far enough to record its arguments before the
	// deadline fires. On Darwin the first exec of a freshly written script
	// can spend well over 200ms in code-signing and XProtect checks; the
	// 0.4.3 Nix build on PZM failed here. Give it 2s; elapsed stays under 10s.
	scope := NewRunScope("dev-artifacts", time.Time{}, 2*time.Second, 2*time.Second)
	ctx := WithRunScope(context.Background(), scope)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	started := time.Now()
	freed := NewDevArtifactsPlugin().cleanGoBuildCache(ctx, LevelAggressive, logger)
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Fatalf("a hanging `go` held the cleaner for %s", elapsed)
	}
	if freed != 0 {
		t.Fatalf("freed = %d from a `go clean` that never ran to completion", freed)
	}
	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("`go clean` was never started: %v", err)
	}
	if got := strings.TrimSpace(string(data)); got != "clean -cache" {
		t.Fatalf("go invocations = %q, want only %q", got, "clean -cache")
	}
	if got := scope.Wedged(); len(got) != 1 || got[0] != "wedged:go clean -cache" {
		t.Fatalf("Wedged() = %v, want [wedged:go clean -cache]", got)
	}
}

// Every external command in this package goes through the deadline
// constructors. A bare exec.Command or exec.CommandContext has no deadline of
// its own and can hold the daemon forever.
func TestNoExternalCommandWithoutDeadline(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat("exec_deadline.go"); err != nil {
		t.Skip("package sources are not available in this test sandbox")
	}
	bare := regexp.MustCompile(`\bexec\.Command(Context)?\(`)
	goEnv := regexp.MustCompile(`(?i)command(context)?\(.*"go",\s*"env"`)
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			if file != "exec_deadline.go" && bare.MatchString(line) {
				t.Errorf("%s:%d starts a command without a deadline; use commandContext, reclaimCommandContext, offlineCommandContext or restartCommandContext", file, i+1)
			}
			if goEnv.MatchString(line) {
				t.Errorf("%s:%d runs `go env`; resolve the setting with goEnvSetting or goBuildCacheDir", file, i+1)
			}
		}
	}
}
