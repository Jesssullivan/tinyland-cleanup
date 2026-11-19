// Package plugins provides cleanup plugin implementations.
// nix_pin_expiry.go expires old named GC-root symlinks in configured lane
// directories before a Nix GC pass.
//
// Lane pins (nix build -o links, gcroots entries, result links) keep whole
// closures alive after their lane is finished, so a GC pass frees nothing
// while every path is rooted. With nix.pin_expiry_roots set, a symlink under
// those roots whose own mtime is older than nix.pin_expiry_after is removed.
// Only the link goes; the store path is collected by the GC pass that follows.
//
// A link is never removed when it is not a symlink, is owned by another
// user, does not point into the Nix store, lives under a profiles directory,
// is a profile generation link, sits in the hook-pin directory, or is covered
// by a keep file: a ".keep" file in its directory or any directory above it
// inside the root, or a "<name>.keep" file beside it. Each removal is written
// to the receipt file, one JSON line with link and target.
package plugins

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Jesssullivan/tinyland-cleanup/config"
)

const (
	nixPinExpiryDefaultAfter   = 14 * 24 * time.Hour
	nixPinExpiryDefaultDepth   = 6
	nixPinExpiryDefaultPerPass = 500
	nixPinExpiryKeepFile       = ".keep"
	nixPinExpiryTargetType     = "nix_pin_expiry"
	nixPinExpiryReceiptDefault = "~/.local/state/tinyland-cleanup/pin-expiry-receipts.jsonl"
)

var nixProfileGenerationLink = regexp.MustCompile(`-\d+-link$`)

type nixExpiredPin struct {
	Link   string
	Target string
	MTime  time.Time
}

type nixPinExpiryReceipt struct {
	Time   string `json:"time"`
	Link   string `json:"link"`
	Target string `json:"target"`
	MTime  string `json:"mtime"`
	Age    string `json:"age"`
	Dry    bool   `json:"dry_run,omitempty"`
}

func nixPinExpiryEnabled(cfg config.NixConfig) bool {
	return len(cfg.PinExpiryRoots) > 0
}

func nixPinExpiryAfter(cfg config.NixConfig) time.Duration {
	d := parseNixPolicyDuration(cfg.PinExpiryAfter, nixPinExpiryDefaultAfter)
	if d <= 0 {
		return nixPinExpiryDefaultAfter
	}
	return d
}

func nixPinExpiryReceiptPath(cfg config.NixConfig) string {
	path := strings.TrimSpace(cfg.PinExpiryReceiptFile)
	if path == "" {
		path = nixPinExpiryReceiptDefault
	}
	home, _ := os.UserHomeDir()
	return filepath.Clean(expandHome(path, home))
}

// nixPinExpiryRootDirs expands the configured roots: ~, then globs.
func nixPinExpiryRootDirs(cfg config.NixConfig) []string {
	home, _ := os.UserHomeDir()
	seen := map[string]bool{}
	var dirs []string
	for _, raw := range cfg.PinExpiryRoots {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		pattern := filepath.Clean(expandHome(raw, home))
		matches := []string{pattern}
		if strings.ContainsAny(pattern, "*?[") {
			matches, _ = filepath.Glob(pattern)
		}
		for _, m := range matches {
			info, err := os.Lstat(m)
			if err != nil || !info.IsDir() || seen[m] {
				continue
			}
			seen[m] = true
			dirs = append(dirs, m)
		}
	}
	sort.Strings(dirs)
	return dirs
}

// nixPinHasKeep reports whether a keep file covers link inside root.
func nixPinHasKeep(root, link string) bool {
	if _, err := os.Lstat(link + nixPinExpiryKeepFile); err == nil {
		return true
	}
	dir := filepath.Dir(link)
	for {
		if _, err := os.Lstat(filepath.Join(dir, nixPinExpiryKeepFile)); err == nil {
			return true
		}
		if dir == root || !strings.HasPrefix(dir, root) {
			return false
		}
		dir = filepath.Dir(dir)
	}
}

// nixPinExpiryScan lists the links that have expired at now. It reads only.
func (p *NixPlugin) nixPinExpiryScan(cfg config.NixConfig, now time.Time) []nixExpiredPin {
	if !nixPinExpiryEnabled(cfg) {
		return nil
	}
	age := nixPinExpiryAfter(cfg)
	maxDepth := cfg.PinExpiryMaxDepth
	if maxDepth <= 0 {
		maxDepth = nixPinExpiryDefaultDepth
	}
	limit := cfg.PinExpiryMaxPerPass
	if limit <= 0 {
		limit = nixPinExpiryDefaultPerPass
	}
	store := p.storeDir
	if store == "" {
		store = "/nix/store"
	}
	uid := os.Getuid()
	home, _ := os.UserHomeDir()
	hookDir := ""
	if cfg.HookPinDir != "" {
		hookDir = filepath.Clean(expandHome(cfg.HookPinDir, home))
	}
	var out []nixExpiredPin
	for _, root := range nixPinExpiryRootDirs(cfg) {
		rootDepth := strings.Count(root, string(filepath.Separator))
		_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if len(out) >= limit {
				return filepath.SkipAll
			}
			if d.IsDir() {
				if strings.Count(path, string(filepath.Separator))-rootDepth >= maxDepth {
					return filepath.SkipDir
				}
				if path == hookDir || strings.Contains(path, "/nix/var/nix/profiles") || filepath.Base(path) == "profiles" {
					return filepath.SkipDir
				}
				return nil
			}
			if d.Type()&fs.ModeSymlink == 0 {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return nil
			}
			if st, ok := info.Sys().(*syscall.Stat_t); !ok || int(st.Uid) != uid {
				return nil
			}
			if now.Sub(info.ModTime()) < age {
				return nil
			}
			if nixProfileGenerationLink.MatchString(filepath.Base(path)) {
				return nil
			}
			target, err := os.Readlink(path)
			if err != nil {
				return nil
			}
			if !filepath.IsAbs(target) {
				target = filepath.Join(filepath.Dir(path), target)
			}
			if !strings.HasPrefix(target, store+"/") || strings.HasPrefix(target, "/nix/var/nix/profiles") {
				return nil
			}
			if nixPinHasKeep(root, path) {
				return nil
			}
			out = append(out, nixExpiredPin{Link: path, Target: target, MTime: info.ModTime()})
			return nil
		})
		if len(out) >= limit {
			break
		}
	}
	return out
}

// planPinExpiry lists the links a pass would remove, for dry-run output.
func (p *NixPlugin) planPinExpiry(cfg *config.Config) ([]CleanupTarget, map[string]string, []string) {
	metadata := map[string]string{"pin_expiry": strconv.FormatBool(nixPinExpiryEnabled(cfg.Nix))}
	if !nixPinExpiryEnabled(cfg.Nix) {
		return nil, metadata, nil
	}
	now := p.clock()
	expired := p.nixPinExpiryScan(cfg.Nix, now)
	metadata["pin_expiry_after"] = nixPinExpiryAfter(cfg.Nix).String()
	metadata["pin_expiry_links"] = strconv.Itoa(len(expired))
	metadata["pin_expiry_receipt_file"] = nixPinExpiryReceiptPath(cfg.Nix)
	targets := make([]CleanupTarget, 0, len(expired))
	for _, e := range expired {
		targets = append(targets, CleanupTarget{
			Type:   nixPinExpiryTargetType,
			Tier:   "rebuildable",
			Name:   filepath.Base(e.Link),
			Path:   e.Link,
			Action: "remove_pin_link",
			Reason: fmt.Sprintf("lane pin older than %s points at %s", nixPinExpiryAfter(cfg.Nix), e.Target),
		})
	}
	return targets, metadata, nil
}

// expirePins removes the expired links and records each one. A receipt that
// cannot be written stops the pass before any removal.
func (p *NixPlugin) expirePins(cfg *config.Config, logger *slog.Logger) int {
	if !nixPinExpiryEnabled(cfg.Nix) {
		return 0
	}
	now := p.clock()
	expired := p.nixPinExpiryScan(cfg.Nix, now)
	if len(expired) == 0 {
		return 0
	}
	receiptPath := nixPinExpiryReceiptPath(cfg.Nix)
	if err := os.MkdirAll(filepath.Dir(receiptPath), 0o700); err != nil {
		logger.Warn("pin expiry skipped: receipt directory is unusable", "path", receiptPath, "error", err)
		return 0
	}
	receipt, err := os.OpenFile(receiptPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		logger.Warn("pin expiry skipped: receipt file is unusable", "path", receiptPath, "error", err)
		return 0
	}
	defer receipt.Close()
	removed := 0
	for _, e := range expired {
		line, _ := json.Marshal(nixPinExpiryReceipt{
			Time:   now.UTC().Format(time.RFC3339),
			Link:   e.Link,
			Target: e.Target,
			MTime:  e.MTime.UTC().Format(time.RFC3339),
			Age:    now.Sub(e.MTime).Round(time.Hour).String(),
		})
		if _, err := receipt.Write(append(line, '\n')); err != nil {
			logger.Warn("pin expiry stopped: receipt write failed", "error", err)
			break
		}
		if err := os.Remove(e.Link); err != nil {
			logger.Warn("pin expiry could not remove link", "link", e.Link, "error", err)
			continue
		}
		removed++
		logger.Info("expired lane pin", "link", e.Link, "target", e.Target, "mtime", e.MTime)
	}
	return removed
}
