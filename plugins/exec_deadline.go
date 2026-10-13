package plugins

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Every external command a plugin starts has a deadline. A child that never
// returns used to hold its plugin, and with it the whole daemon, for as long
// as the child lived: on one host `go env GOCACHE` sat in dyld for sixteen
// hours and nothing was reclaimed in that time.
//
// The constructors below replace exec.CommandContext and exec.Command
// everywhere in this package. They differ only in the deadline they apply when
// the caller did not set one:
//
//	commandContext         short probes and listings (DefaultExecTimeout)
//	reclaimCommandContext  commands that delete or prune (DefaultExecReclaimTimeout)
//	offlineCommandContext  VM stop and disk image conversion (execOfflineTimeout)
//	restartCommandContext  VM start after a stop by this daemon (execRestartTimeout)
//
// A caller that wants a specific deadline keeps using context.WithTimeout on
// the context it passes; that deadline is honored as given.

const (
	// DefaultExecTimeout bounds a probe command with no deadline of its own.
	DefaultExecTimeout = 60 * time.Second
	// DefaultExecReclaimTimeout bounds a reclaim command with no deadline of its own.
	DefaultExecReclaimTimeout = 15 * time.Minute
	// execOfflineTimeout bounds VM lifecycle and image conversion commands.
	execOfflineTimeout = 2 * time.Hour
	// execRestartTimeout bounds a VM start that follows a stop by this daemon.
	execRestartTimeout = 10 * time.Minute
)

// execKillGrace is how long a command gets between SIGTERM and SIGKILL, and
// how long Wait keeps reading output from a child that left grandchildren
// holding its pipes. Tests shorten it.
var execKillGrace = 5 * time.Second

type execClass int

const (
	execProbe execClass = iota
	execReclaim
	execOffline
	execRestart
)

// RunScope carries one plugin run's deadlines and records what its commands
// did. The daemon creates one per plugin run and attaches it to the context
// the plugin receives. A context with no scope uses the built-in defaults.
type RunScope struct {
	plugin   string
	deadline time.Time
	probe    time.Duration
	reclaim  time.Duration

	mu          sync.Mutex
	lastCommand string
	lastStarted time.Time
	wedged      []string
	killed      map[string]bool
}

// NewRunScope returns the scope for one run of plugin. deadline is the
// plugin's own watchdog deadline (zero for none). probe and reclaim are the
// default command deadlines; zero or negative selects the built-in default.
func NewRunScope(plugin string, deadline time.Time, probe, reclaim time.Duration) *RunScope {
	if probe <= 0 {
		probe = DefaultExecTimeout
	}
	if reclaim <= 0 {
		reclaim = DefaultExecReclaimTimeout
	}
	return &RunScope{plugin: plugin, deadline: deadline, probe: probe, reclaim: reclaim}
}

type runScopeKey struct{}

// WithRunScope attaches scope to ctx.
func WithRunScope(ctx context.Context, scope *RunScope) context.Context {
	return context.WithValue(ctx, runScopeKey{}, scope)
}

func runScopeFrom(ctx context.Context) *RunScope {
	if ctx == nil {
		return nil
	}
	scope, _ := ctx.Value(runScopeKey{}).(*RunScope)
	return scope
}

// LastCommand returns the most recently started command and when it started.
func (s *RunScope) LastCommand() (string, time.Time) {
	if s == nil {
		return "", time.Time{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastCommand, s.lastStarted
}

// Wedged returns one "wedged:<command>" entry for each command this run
// killed at its deadline, in the order they were killed.
func (s *RunScope) Wedged() []string {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.wedged...)
}

func (s *RunScope) timeoutFor(class execClass) time.Duration {
	switch class {
	case execOffline:
		return execOfflineTimeout
	case execRestart:
		return execRestartTimeout
	case execReclaim:
		if s != nil {
			return s.reclaim
		}
		return DefaultExecReclaimTimeout
	default:
		if s != nil {
			return s.probe
		}
		return DefaultExecTimeout
	}
}

func (s *RunScope) started(label string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.lastCommand = label
	s.lastStarted = time.Now()
	s.mu.Unlock()
}

func (s *RunScope) recordKill(binary, label string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.wedged = append(s.wedged, "wedged:"+label)
	if s.killed == nil {
		s.killed = make(map[string]bool)
	}
	s.killed[binary] = true
	s.mu.Unlock()
}

func (s *RunScope) binaryKilled(binary string) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.killed[binary]
}

// commandLabel is the command as an operator would type it, cut to a length
// that fits a log line and a report field.
func commandLabel(name string, args []string) string {
	label := strings.Join(append([]string{filepath.Base(name)}, args...), " ")
	const max = 160
	if len(label) > max {
		label = label[:max] + "..."
	}
	return label
}

// commandContext returns a command for a short probe or listing.
func commandContext(ctx context.Context, name string, args ...string) *exec.Cmd {
	return newCommand(ctx, execProbe, name, args...)
}

// reclaimCommandContext returns a command that deletes or prunes.
func reclaimCommandContext(ctx context.Context, name string, args ...string) *exec.Cmd {
	return newCommand(ctx, execReclaim, name, args...)
}

// offlineCommandContext returns a VM stop or disk image conversion command.
// These legitimately run for a long time.
func offlineCommandContext(ctx context.Context, name string, args ...string) *exec.Cmd {
	return newCommand(ctx, execOffline, name, args...)
}

func newCommand(ctx context.Context, class execClass, name string, args ...string) *exec.Cmd {
	if ctx == nil {
		ctx = context.Background()
	}
	scope := runScopeFrom(ctx)
	label := commandLabel(name, args)
	binary := filepath.Base(name)

	// A deadline that differs from the plugin's watchdog deadline was set by
	// the caller for this command. Keep it.
	callerDeadline := false
	if deadline, ok := ctx.Deadline(); ok {
		callerDeadline = scope == nil || scope.deadline.IsZero() || !deadline.Equal(scope.deadline)
	}

	var (
		cctx   context.Context
		cancel context.CancelFunc
	)
	if callerDeadline {
		cctx, cancel = context.WithCancel(ctx)
	} else {
		cctx, cancel = context.WithTimeout(ctx, scope.timeoutFor(class))
	}

	cmd := exec.CommandContext(cctx, name, args...)
	if class != execOffline && class != execRestart && scope.binaryKilled(binary) {
		// The same program already hung once in this plugin run. Do not wait
		// out another full deadline on it.
		cancel()
		cmd.Err = fmt.Errorf("%s: not started: %s was killed at its deadline earlier in this run", label, binary)
		return cmd
	}
	scope.started(label)
	cmd.Cancel = func() error {
		// os/exec calls this once, when cctx ends while the child is alive.
		// A deadline the caller chose (a 5s git probe, say) is ordinary
		// control flow and is not recorded. One this package imposed, or the
		// plugin's own watchdog deadline, means the command hung.
		if !callerDeadline && cctx.Err() == context.DeadlineExceeded {
			scope.recordKill(binary, label)
		}
		cancel()
		// SIGTERM first so wrappers such as sudo can pass it on; os/exec
		// follows with SIGKILL after WaitDelay.
		return cmd.Process.Signal(syscall.SIGTERM)
	}
	cmd.WaitDelay = execKillGrace
	return cmd
}

// goEnvSetting resolves one `go env` setting the way cmd/go does, without
// starting `go`: the process environment first, then the user's go/env file
// (written by `go env -w`, or named by GOENV). It returns "" when neither
// sets the key, so the caller applies the documented default.
func goEnvSetting(key string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	envFile := os.Getenv("GOENV")
	if envFile == "off" {
		return ""
	}
	if envFile == "" {
		dir, err := os.UserConfigDir()
		if err != nil {
			return ""
		}
		envFile = filepath.Join(dir, "go", "env")
	}
	data, err := os.ReadFile(envFile)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		name, value, ok := strings.Cut(line, "=")
		if ok && name == key {
			return value
		}
	}
	return ""
}

// goBuildCacheDir returns the Go build cache directory, or "" when the cache
// is off or cannot be located. It never starts `go`: GOCACHE from the
// environment or the go/env file, else the default go-build directory under
// os.UserCacheDir (~/Library/Caches on Darwin, $XDG_CACHE_HOME or ~/.cache on
// Linux), which is the rule cmd/go itself applies.
func goBuildCacheDir() string {
	if value := goEnvSetting("GOCACHE"); value != "" {
		if value == "off" || !filepath.IsAbs(value) {
			return ""
		}
		return filepath.Clean(value)
	}
	dir, err := os.UserCacheDir()
	if err != nil || !filepath.IsAbs(dir) {
		return ""
	}
	return filepath.Join(dir, "go-build")
}

// restartCommandContext returns a command that brings a VM back after this
// daemon stopped it. It must run even when the plugin's own deadline has
// already passed, or a compaction cut short would leave the VM down, so it is
// detached from the caller's cancellation and bounded only by its own
// deadline.
func restartCommandContext(ctx context.Context, name string, args ...string) *exec.Cmd {
	if ctx == nil {
		ctx = context.Background()
	}
	return newCommand(context.WithoutCancel(ctx), execRestart, name, args...)
}
