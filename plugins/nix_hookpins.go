package plugins

// nix_hookpins.go: hook-path safety for Nix GC (lab review H1; TIN-4405).
//
// Claude Code and Codex snapshot their hook commands at session start. A hook
// named by /nix/store path in settings.json keeps running from that path for
// the session's life, but no process holds it open between calls, so Nix's
// runtime roots do not protect it. After a Home Manager switch the old path
// is rooted by nothing, and a GC removes it from under the live session.
//
// Before every GC pass the plugin roots each store path its hook config
// files reference, under nix.hook_pin_dir (a gcroots/ directory, so the
// stale-root reaper never lists them). A pinned path that the config no
// longer references is released only once no session process (argv[0] or
// script basename in nix.hook_pin_session_comms) that started before the
// path stopped being referenced is still alive. When session inspection
// fails every pin is kept.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/Jesssullivan/tinyland-cleanup/config"
)

var nixStorePathRefRe = regexp.MustCompile(`/nix/store/[0-9a-z]{32}-[^/\s"'\\,;:)\]}]+`)

type nixHookPin struct {
	StorePath      string     `json:"store_path"`
	FirstSeen      time.Time  `json:"first_seen"`
	LastReferenced time.Time  `json:"last_referenced"`
	Unreferenced   *time.Time `json:"unreferenced,omitempty"`
}

type nixHookPinState struct {
	Pins map[string]*nixHookPin `json:"pins"`
}

// nixHookPinner holds the seams the pinning logic uses.
type nixHookPinner struct {
	dir           string
	now           time.Time
	valid         func(ctx context.Context, storePath string) bool
	addRoot       func(ctx context.Context, link, storePath string) error
	sessionStarts func(comms []string) ([]time.Time, error)
}

func (p *NixPlugin) hookPinner(cfg config.NixConfig) nixHookPinner {
	home, _ := os.UserHomeDir()
	pinner := nixHookPinner{
		dir:           filepath.Clean(expandHome(cfg.HookPinDir, home)),
		now:           p.clock(),
		valid:         nixStorePathValid,
		addRoot:       nixAddIndirectRoot,
		sessionStarts: nixSessionStartTimes,
	}
	if p.hookValid != nil {
		pinner.valid = p.hookValid
	}
	if p.hookAddRoot != nil {
		pinner.addRoot = p.hookAddRoot
	}
	if p.hookSessionStarts != nil {
		pinner.sessionStarts = p.hookSessionStarts
	}
	return pinner
}

// refreshHookPins pins every referenced hook path and releases pins no live
// session can still need. An error means the pins could not be established.
func (p *NixPlugin) refreshHookPins(ctx context.Context, cfg config.NixConfig, logger *slog.Logger) error {
	if len(cfg.HookPinFiles) == 0 {
		return nil
	}
	pinner := p.hookPinner(cfg)
	return pinner.refresh(ctx, cfg, logger)
}

func (h nixHookPinner) refresh(ctx context.Context, cfg config.NixConfig, logger *slog.Logger) error {
	if !nixPathHasComponent(h.dir, "gcroots") {
		return fmt.Errorf("hook_pin_dir %s has no gcroots path component", h.dir)
	}
	home, _ := os.UserHomeDir()
	refs := map[string]bool{}
	for _, file := range cfg.HookPinFiles {
		path := filepath.Clean(expandHome(file, home))
		raw, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("read hook config %s: %w", path, err)
		}
		for _, ref := range nixStorePathRefRe.FindAllString(string(raw), -1) {
			refs[ref] = true
		}
	}
	if err := os.MkdirAll(h.dir, 0o700); err != nil {
		return err
	}
	statePath := filepath.Join(h.dir, "state.json")
	st := nixHookPinState{Pins: map[string]*nixHookPin{}}
	if raw, err := os.ReadFile(statePath); err == nil {
		_ = json.Unmarshal(raw, &st)
		if st.Pins == nil {
			st.Pins = map[string]*nixHookPin{}
		}
	}

	refList := make([]string, 0, len(refs))
	for ref := range refs {
		refList = append(refList, ref)
	}
	sort.Strings(refList)
	for _, ref := range refList {
		pin := st.Pins[ref]
		if pin == nil {
			pin = &nixHookPin{StorePath: ref, FirstSeen: h.now}
			st.Pins[ref] = pin
		}
		pin.LastReferenced = h.now
		pin.Unreferenced = nil
		link := filepath.Join(h.dir, filepath.Base(ref))
		if dest, err := os.Readlink(link); err == nil && dest == ref {
			continue
		}
		if !h.valid(ctx, ref) {
			// Already gone; nothing to protect, and the session using it is
			// already broken. Keep tracking it so a later realisation pins it.
			logger.Warn("hook config references a store path that is not valid", "store_path", ref)
			continue
		}
		_ = os.Remove(link)
		if err := h.addRoot(ctx, link, ref); err != nil {
			return fmt.Errorf("pin %s: %w", ref, err)
		}
	}
	for ref, pin := range st.Pins {
		if refs[ref] || pin.Unreferenced != nil {
			continue
		}
		t := h.now
		pin.Unreferenced = &t
	}

	starts, err := h.sessionStarts(cfg.HookPinSessionComms)
	if err != nil {
		logger.Warn("keeping every hook pin; sessions cannot be inspected", "error", err)
	} else {
		for ref, pin := range st.Pins {
			if pin.Unreferenced == nil {
				continue
			}
			held := false
			for _, start := range starts {
				if start.Before(*pin.Unreferenced) {
					held = true
					break
				}
			}
			if held {
				continue
			}
			link := filepath.Join(h.dir, filepath.Base(ref))
			if dest, err := os.Readlink(link); err == nil && dest == ref {
				if err := os.Remove(link); err != nil {
					return err
				}
			}
			delete(st.Pins, ref)
			logger.Info("released hook pin; no session started while it was referenced is alive", "store_path", ref)
		}
	}
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := statePath + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, statePath)
}

func nixPathHasComponent(path, component string) bool {
	for _, part := range strings.Split(filepath.Clean(path), string(filepath.Separator)) {
		if part == component {
			return true
		}
	}
	return false
}

func nixStorePathValid(ctx context.Context, storePath string) bool {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	return commandContext(ctx, "nix-store", "--check-validity", storePath).Run() == nil
}

func nixAddIndirectRoot(ctx context.Context, link, storePath string) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	out, err := commandContext(ctx, "nix-store", "--realise", storePath, "--add-root", link).CombinedOutput()
	if err != nil {
		return fmt.Errorf("nix-store --add-root: %w: %s", err, strings.TrimSpace(string(out)))
	}
	if dest, err := os.Readlink(link); err != nil || dest != storePath {
		return fmt.Errorf("pin %s does not select %s", link, storePath)
	}
	return nil
}
