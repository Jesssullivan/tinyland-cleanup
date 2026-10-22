//go:build linux

package plugins

import (
	"testing"
	"time"
)

// The shm_scratch half of TestTempSweepAndShmScratchIgnorePackageCopiesOnly.
// newShmFixture and exists live in the linux-tagged shm_scratch_test.go, so
// this half is linux-only; the temp-sweep half stays portable (darwin test
// compile, TIN-5770).
func TestShmScratchIgnoresPackageCopiesOnly(t *testing.T) {
	f := newShmFixture(t)
	copies := f.tree("npm-scratch", 48*time.Hour, "node_modules/pkg/.claude/x", "node_modules/pkg/package.json")
	ws := f.tree("ws-with-deps", 48*time.Hour, ".claude/x", "node_modules/pkg/.claude/y", "node_modules/pkg/package.json")
	sessions := f.tree("pkg-sessions", 48*time.Hour, "node_modules/pkg/.codex/sessions/r.jsonl", "node_modules/pkg/package.json")
	f.run()
	if exists(copies) {
		t.Error("shm_scratch kept an idle tree whose only .claude is a package copy")
	}
	if !exists(ws) || !exists(sessions) {
		t.Fatal("shm_scratch removed a tree holding session data (TIN-5268)")
	}
}
