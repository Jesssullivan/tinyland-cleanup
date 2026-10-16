package plugins

// nix_cadence.go: when the nix plugin may collect (lab R-C796). The plugin
// is level-independent, so the daemon calls it every poll. It collects
//   - under pressure (warning and above), and
//   - periodically, at least every nix.periodic_gc_interval, whatever the
//     pressure (a plain GC, no generation deletion),
// but never twice within nix.min_gc_interval, even at critical.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/Jesssullivan/tinyland-cleanup/config"
)

const nixDefaultMinGCInterval = 30 * time.Minute

type nixGCState struct {
	LastGCAttempt time.Time `json:"last_gc_attempt"`
}

// LevelIndependent makes the daemon call the nix plugin on every poll so the
// periodic trigger and the candidate list (rewritten every poll) work with no
// host pressure. The plugin applies its own cadence.
func (p *NixPlugin) LevelIndependent() bool { return true }

func (p *NixPlugin) clock() time.Time {
	if p.now != nil {
		return p.now()
	}
	return time.Now()
}

func nixGCStatePath(cfg config.NixConfig) string {
	path := cfg.GCStateFile
	if path == "" {
		return ""
	}
	home, _ := os.UserHomeDir()
	return filepath.Clean(expandHome(path, home))
}

func loadNixGCState(path string) nixGCState {
	var st nixGCState
	if path == "" {
		return st
	}
	if raw, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(raw, &st)
	}
	return st
}

func saveNixGCState(path string, st nixGCState) error {
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	raw, err := json.Marshal(st)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// nixGCDue returns the level to collect at now, or LevelNone, with a reason.
func nixGCDue(level CleanupLevel, cfg config.NixConfig, st nixGCState, now time.Time) (CleanupLevel, string) {
	minGap := parseNixPolicyDuration(cfg.MinGCInterval, nixDefaultMinGCInterval)
	periodic := parseNixPolicyDuration(cfg.PeriodicGCInterval, 0)
	since := now.Sub(st.LastGCAttempt)
	if !st.LastGCAttempt.IsZero() && minGap > 0 && since < minGap {
		return LevelNone, "min_gc_interval"
	}
	if level >= LevelWarning {
		return level, "pressure"
	}
	if periodic > 0 && (st.LastGCAttempt.IsZero() || since >= periodic) {
		return LevelWarning, "periodic"
	}
	return LevelNone, "not_due"
}
