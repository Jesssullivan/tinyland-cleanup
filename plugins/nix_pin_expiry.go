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
//
// A link whose target is in active use is never removed either (TIN-5770,
// "Spare pins in active use"). Each pass takes one process inventory, the
// bazel_bases one: a process that holds the target or a path under it (exe,
// cwd, root, open fd or mapping, from /proc on Linux and lsof on Darwin) or
// names the link or the target on its command line uses it. An incomplete
// inventory spares every expired link for that pass. A link seen in use is
// stamped in the state file, and a link seen in use within pin_expiry_after
// is spared as well, so a pin used in the last 14 days survives a pass that
// runs between two uses. Store atime is not used: /nix is mounted noatime on
// sting and on PZM, so it does not record use. The first time a link is
// spared for a reason, one receipt line records it. Until pin_expiry_after
// has passed since the state file began (observed_since), every expired link
// is spared. On Linux a /proc-only stamp runs every poll; lsof runs only on
// the removal pass. Each link is read again just before removal and kept if
// its target or mtime changed since the walk.
package plugins

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
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
	nixPinExpiryStateDefault   = "~/.local/state/tinyland-cleanup/pin-expiry-state.json"
	// nixPinExpiryWalkCap bounds the links one walk lists. Every link is
	// checked for use, not only the expired ones, so a pin used while it is
	// still young keeps its stamp.
	nixPinExpiryWalkCap = 20000

	nixPinSpareInUse      = "in_use"
	nixPinSpareSeenInUse  = "seen_in_use"
	nixPinSpareIncomplete = "inventory_incomplete"
	nixPinSpareObserving  = "observing"
)

var nixProfileGenerationLink = regexp.MustCompile(`-\d+-link$`)

type nixExpiredPin struct {
	Link   string
	Target string
	MTime  time.Time
}

// nixPinLink is one store-pointing link a walk found, expired or not.
type nixPinLink struct {
	nixExpiredPin
	Expired bool
}

// nixSparedPin is an expired link kept because its target is or was in use.
type nixSparedPin struct {
	nixExpiredPin
	Kind   string
	Reason string
}

type nixPinExpiryReceipt struct {
	Time   string `json:"time"`
	Action string `json:"action,omitempty"`
	Link   string `json:"link"`
	Target string `json:"target"`
	MTime  string `json:"mtime"`
	Age    string `json:"age"`
	Reason string `json:"reason,omitempty"`
	Dry    bool   `json:"dry_run,omitempty"`
}

// nixPinUseState is the pin expiry state file: per link, when its target
// was last seen in use and which spare reason was last receipted.
type nixPinUseState struct {
	// ObservedSince is when stamping began. Until pin_expiry_after has
	// passed since then, the stamps cannot show that a link went unused for
	// the whole window, so every expired link is spared.
	ObservedSince time.Time                 `json:"observed_since,omitempty"`
	Links         map[string]nixPinUseEntry `json:"links"`
}

type nixPinUseEntry struct {
	Target         string    `json:"target"`
	LastSeenInUse  time.Time `json:"last_seen_in_use,omitempty"`
	LastSeenBy     string    `json:"last_seen_by,omitempty"`
	SpareReceipted string    `json:"spare_receipted,omitempty"`
}

// nixPinDecision is one pass's verdict on the walked links.
type nixPinDecision struct {
	Expired    []nixExpiredPin
	Spared     []nixSparedPin
	Links      int
	Incomplete string
	State      nixPinUseState
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

func nixPinExpiryStatePath(cfg config.NixConfig) string {
	path := strings.TrimSpace(cfg.PinExpiryStateFile)
	if path == "" {
		path = nixPinExpiryStateDefault
	}
	home, _ := os.UserHomeDir()
	return filepath.Clean(expandHome(path, home))
}

// loadNixPinUseState reads the state file. A missing file is an empty state;
// any other failure is an error, and the pass then removes nothing, because
// it cannot know which links were seen in use.
func loadNixPinUseState(path string) (nixPinUseState, error) {
	st := nixPinUseState{Links: map[string]nixPinUseEntry{}}
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	if err := json.Unmarshal(raw, &st); err != nil {
		return nixPinUseState{Links: map[string]nixPinUseEntry{}}, fmt.Errorf("parse %s: %w", path, err)
	}
	if st.Links == nil {
		st.Links = map[string]nixPinUseEntry{}
	}
	return st, nil
}

func saveNixPinUseState(path string, st nixPinUseState) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// nixPinActivityOptions builds the process inventory options. The lsof
// binary is nix.pin_expiry_lsof, else bazel_bases.lsof, else on Darwin the
// one on PATH: Darwin has no other open-file inventory, and without one the
// inventory is incomplete and nothing expires.
func nixPinActivityOptions(cfg *config.Config, store string, roots []string) bbActivityOptions {
	lsof := strings.TrimSpace(cfg.Nix.PinExpiryLsof)
	if lsof == "" {
		lsof = strings.TrimSpace(cfg.BazelBases.Lsof)
	}
	if lsof == "" && runtime.GOOS == "darwin" {
		lsof, _ = exec.LookPath("lsof")
	}
	opts := bbActivityOptions{
		Lsof:        lsof,
		LsofTimeout: time.Duration(cfg.BazelBases.LsofTimeoutSeconds) * time.Second,
		UseLsof:     lsof != "",
		OKComms:     cfg.Nix.PinExpiryUninspectableOKComms,
		OKUnits:     cfg.Nix.PinExpiryUninspectableOKUnits,
		// An SSH session process runs no store path of a lane pin; its
		// parent chain proves what it is.
		SkipSSHDSessions: true,
		// Darwin: a process of this user that lsof cannot read is a hole.
		RequireLsofSameUser: true,
	}
	opts.Within = append(opts.Within, uniquePaths(store, resolvedOrSelf(store))...)
	for _, r := range roots {
		opts.Within = append(opts.Within, uniquePaths(r, resolvedOrSelf(r))...)
	}
	return opts
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

// nixPinExpiryWalk lists the store-pointing links under the roots that no
// keep file covers, each marked expired when its own mtime is older than
// pin_expiry_after at now. It reads only.
func (p *NixPlugin) nixPinExpiryWalk(cfg config.NixConfig, now time.Time) []nixPinLink {
	if !nixPinExpiryEnabled(cfg) {
		return nil
	}
	age := nixPinExpiryAfter(cfg)
	maxDepth := cfg.PinExpiryMaxDepth
	if maxDepth <= 0 {
		maxDepth = nixPinExpiryDefaultDepth
	}
	limit := nixPinExpiryWalkCap
	store := p.storePath()
	uid := os.Getuid()
	home, _ := os.UserHomeDir()
	hookDir := ""
	if cfg.HookPinDir != "" {
		hookDir = filepath.Clean(expandHome(cfg.HookPinDir, home))
	}
	var out []nixPinLink
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
			out = append(out, nixPinLink{
				nixExpiredPin: nixExpiredPin{Link: path, Target: target, MTime: info.ModTime()},
				Expired:       now.Sub(info.ModTime()) >= age,
			})
			return nil
		})
		if len(out) >= limit {
			break
		}
	}
	return out
}

func (p *NixPlugin) storePath() string {
	if p.storeDir == "" {
		return "/nix/store"
	}
	return p.storeDir
}

func (p *NixPlugin) pinActivityScan(ctx context.Context, opts bbActivityOptions) *bbActivity {
	if p.pinActivity != nil {
		return p.pinActivity(ctx, opts)
	}
	return bbScanActivity(ctx, opts)
}

// decidePinExpiry walks the roots, takes one process inventory, stamps the
// links whose target is in use and sorts the expired links into removals and
// spares. It writes nothing; st is updated in memory.
func (p *NixPlugin) decidePinExpiry(ctx context.Context, cfg *config.Config, now time.Time, st nixPinUseState) nixPinDecision {
	d := nixPinDecision{State: st}
	if d.State.ObservedSince.IsZero() {
		// No state file yet (or one from before observed_since): observing
		// starts now, and nothing expires for one window.
		d.State.ObservedSince = now.UTC()
	}
	links := p.nixPinExpiryWalk(cfg.Nix, now)
	d.Links = len(links)
	// Forget links that are gone.
	for link := range d.State.Links {
		if _, err := os.Lstat(link); errors.Is(err, fs.ErrNotExist) {
			delete(d.State.Links, link)
		}
	}
	if len(links) == 0 {
		return d
	}
	age := nixPinExpiryAfter(cfg.Nix)
	limit := cfg.Nix.PinExpiryMaxPerPass
	if limit <= 0 {
		limit = nixPinExpiryDefaultPerPass
	}
	activity := p.pinActivityScan(ctx, nixPinActivityOptions(cfg, p.storePath(), nixPinExpiryRootDirs(cfg.Nix)))
	d.Incomplete = activity.complete()
	for _, l := range links {
		users := stampPinLink(&d.State, l, activity, now)
		entry := d.State.Links[l.Link]
		if !l.Expired {
			continue
		}
		switch {
		case len(users) > 0:
			d.Spared = append(d.Spared, nixSparedPin{nixExpiredPin: l.nixExpiredPin, Kind: nixPinSpareInUse,
				Reason: "target in use: " + users[0]})
		case d.Incomplete != "":
			d.Spared = append(d.Spared, nixSparedPin{nixExpiredPin: l.nixExpiredPin, Kind: nixPinSpareIncomplete,
				Reason: d.Incomplete})
		case now.Sub(d.State.ObservedSince) < age:
			d.Spared = append(d.Spared, nixSparedPin{nixExpiredPin: l.nixExpiredPin, Kind: nixPinSpareObserving,
				Reason: fmt.Sprintf("use has been observed only since %s, less than %s: the state file cannot yet show the link unused for the whole window", d.State.ObservedSince.UTC().Format(time.RFC3339), age)})
		case !entry.LastSeenInUse.IsZero() && now.Sub(entry.LastSeenInUse) < age:
			d.Spared = append(d.Spared, nixSparedPin{nixExpiredPin: l.nixExpiredPin, Kind: nixPinSpareSeenInUse,
				Reason: fmt.Sprintf("target last seen in use %s (%s), within %s", entry.LastSeenInUse.UTC().Format(time.RFC3339), entry.LastSeenBy, age)})
		case len(d.Expired) < limit:
			d.Expired = append(d.Expired, l.nixExpiredPin)
		}
	}
	return d
}

// stampPinLink records in st whether a process in activity uses link l now,
// and returns the uses found. A replaced link drops its old stamps.
func stampPinLink(st *nixPinUseState, l nixPinLink, activity *bbActivity, now time.Time) []string {
	entry, ok := st.Links[l.Link]
	if ok && entry.Target != l.Target {
		delete(st.Links, l.Link)
		entry, ok = nixPinUseEntry{}, false
	}
	entry.Target = l.Target
	var users []string
	for _, path := range uniquePaths(l.Target, l.Link) {
		users = append(users, activity.blockers(path)...)
	}
	if len(users) > 0 {
		entry.LastSeenInUse = now.UTC()
		entry.LastSeenBy = users[0]
	}
	if len(users) > 0 || ok || entry.SpareReceipted != "" {
		st.Links[l.Link] = entry
	}
	return users
}

// stampPinsInUse is the cheap per-poll stamp: a /proc-only inventory (no
// lsof) on Linux, run every poll, so a lane that runs between two GC passes
// still leaves a stamp. It removes nothing. On Darwin the only inventory is
// a full lsof listing, so Darwin stamps on GC passes only. A partial
// inventory still stamps what it saw; it cannot clear a stamp.
func (p *NixPlugin) stampPinsInUse(ctx context.Context, cfg *config.Config, logger *slog.Logger) {
	if !nixPinExpiryEnabled(cfg.Nix) || !p.pinPollStamp() {
		return
	}
	now := p.clock()
	statePath := nixPinExpiryStatePath(cfg.Nix)
	st, err := loadNixPinUseState(statePath)
	if err != nil {
		logger.Warn("pin use stamp skipped: state file is unreadable", "path", statePath, "error", err)
		return
	}
	fresh := st.ObservedSince.IsZero()
	if fresh {
		st.ObservedSince = now.UTC()
	}
	links := p.nixPinExpiryWalk(cfg.Nix, now)
	stamped := 0
	if len(links) > 0 {
		opts := nixPinActivityOptions(cfg, p.storePath(), nixPinExpiryRootDirs(cfg.Nix))
		opts.UseLsof = false
		activity := p.pinActivityScan(ctx, opts)
		for _, l := range links {
			if len(stampPinLink(&st, l, activity, now)) > 0 {
				stamped++
			}
		}
	}
	if stamped == 0 && !fresh {
		return
	}
	if err := saveNixPinUseState(statePath, st); err != nil {
		logger.Warn("could not record pin use stamps", "path", statePath, "error", err)
	}
}

func (p *NixPlugin) pinPollStamp() bool {
	if p.pinPollStampOn != nil {
		return *p.pinPollStampOn
	}
	return runtime.GOOS == "linux"
}

// pinUnchanged re-reads a link just before removal and reports why it may
// not be removed: it is gone, no longer a symlink, has a new mtime, or
// points somewhere else than the walk saw.
func (p *NixPlugin) pinUnchanged(e nixExpiredPin) string {
	info, err := os.Lstat(e.Link)
	if err != nil {
		return "link unreadable: " + err.Error()
	}
	if info.Mode()&fs.ModeSymlink == 0 {
		return "no longer a symlink"
	}
	if !info.ModTime().Equal(e.MTime) {
		return fmt.Sprintf("mtime changed from %s to %s", e.MTime.UTC().Format(time.RFC3339Nano), info.ModTime().UTC().Format(time.RFC3339Nano))
	}
	target, err := os.Readlink(e.Link)
	if err != nil {
		return "link unreadable: " + err.Error()
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(e.Link), target)
	}
	if target != e.Target {
		return "target changed to " + target
	}
	return ""
}

// planPinExpiry lists the links a pass would remove, and the expired links
// it would spare, for dry-run output. It writes neither state nor receipts.
func (p *NixPlugin) planPinExpiry(ctx context.Context, cfg *config.Config) ([]CleanupTarget, map[string]string, []string) {
	metadata := map[string]string{"pin_expiry": strconv.FormatBool(nixPinExpiryEnabled(cfg.Nix))}
	if !nixPinExpiryEnabled(cfg.Nix) {
		return nil, metadata, nil
	}
	now := p.clock()
	metadata["pin_expiry_after"] = nixPinExpiryAfter(cfg.Nix).String()
	metadata["pin_expiry_receipt_file"] = nixPinExpiryReceiptPath(cfg.Nix)
	metadata["pin_expiry_state_file"] = nixPinExpiryStatePath(cfg.Nix)
	st, err := loadNixPinUseState(nixPinExpiryStatePath(cfg.Nix))
	if err != nil {
		metadata["pin_expiry_links"] = "0"
		return nil, metadata, []string{"pin expiry would remove nothing: its state file is unreadable: " + err.Error()}
	}
	d := p.decidePinExpiry(ctx, cfg, now, st)
	metadata["pin_expiry_links"] = strconv.Itoa(len(d.Expired))
	metadata["pin_expiry_spared"] = strconv.Itoa(len(d.Spared))
	var warnings []string
	if d.Incomplete != "" && len(d.Spared) > 0 {
		warnings = append(warnings, "pin expiry spares every expired link this pass: "+d.Incomplete)
	}
	targets := make([]CleanupTarget, 0, len(d.Expired)+len(d.Spared))
	for _, e := range d.Expired {
		targets = append(targets, CleanupTarget{
			Type:   nixPinExpiryTargetType,
			Tier:   "rebuildable",
			Name:   filepath.Base(e.Link),
			Path:   e.Link,
			Action: "remove_pin_link",
			Reason: fmt.Sprintf("lane pin older than %s points at %s", nixPinExpiryAfter(cfg.Nix), e.Target),
		})
	}
	for _, sp := range d.Spared {
		targets = append(targets, CleanupTarget{
			Type:      nixPinExpiryTargetType,
			Tier:      "rebuildable",
			Name:      filepath.Base(sp.Link),
			Path:      sp.Link,
			Active:    sp.Kind != nixPinSpareIncomplete,
			Protected: true,
			Action:    "keep",
			Reason:    fmt.Sprintf("lane pin older than %s spared (%s): %s", nixPinExpiryAfter(cfg.Nix), sp.Kind, sp.Reason),
		})
	}
	return targets, metadata, warnings
}

// expirePins removes the expired links that are not in use and records
// each removal, and the first spare of each link for each reason. The state
// file is read before and written before any removal; a state file or a
// receipt that cannot be read or written stops the pass before any removal.
func (p *NixPlugin) expirePins(ctx context.Context, cfg *config.Config, logger *slog.Logger) int {
	if !nixPinExpiryEnabled(cfg.Nix) {
		return 0
	}
	now := p.clock()
	statePath := nixPinExpiryStatePath(cfg.Nix)
	st, err := loadNixPinUseState(statePath)
	if err != nil {
		logger.Warn("pin expiry skipped: state file is unreadable", "path", statePath, "error", err)
		return 0
	}
	fresh := st.ObservedSince.IsZero()
	d := p.decidePinExpiry(ctx, cfg, now, st)
	if d.Links == 0 && len(st.Links) == 0 && !fresh {
		return 0
	}
	receiptPath := nixPinExpiryReceiptPath(cfg.Nix)
	var receipt *os.File
	openReceipt := func() bool {
		if receipt != nil {
			return true
		}
		if err := os.MkdirAll(filepath.Dir(receiptPath), 0o700); err != nil {
			logger.Warn("pin expiry skipped: receipt directory is unusable", "path", receiptPath, "error", err)
			return false
		}
		f, err := os.OpenFile(receiptPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			logger.Warn("pin expiry skipped: receipt file is unusable", "path", receiptPath, "error", err)
			return false
		}
		receipt = f
		return true
	}
	defer func() {
		if receipt != nil {
			receipt.Close()
		}
	}()
	write := func(action string, e nixExpiredPin, reason string) bool {
		line, _ := json.Marshal(nixPinExpiryReceipt{
			Time:   now.UTC().Format(time.RFC3339),
			Action: action,
			Link:   e.Link,
			Target: e.Target,
			MTime:  e.MTime.UTC().Format(time.RFC3339),
			Age:    now.Sub(e.MTime).Round(time.Hour).String(),
			Reason: reason,
		})
		if _, err := receipt.Write(append(line, '\n')); err != nil {
			logger.Warn("pin expiry stopped: receipt write failed", "error", err)
			return false
		}
		return true
	}
	receiptOK := true
	for _, sp := range d.Spared {
		entry := d.State.Links[sp.Link]
		if entry.SpareReceipted == sp.Kind {
			continue
		}
		if !openReceipt() || !write("spared", sp.nixExpiredPin, sp.Kind+": "+sp.Reason) {
			receiptOK = false
			break
		}
		entry.Target = sp.Target
		entry.SpareReceipted = sp.Kind
		d.State.Links[sp.Link] = entry
		logger.Info("spared expired lane pin", "link", sp.Link, "target", sp.Target, "kind", sp.Kind, "reason", sp.Reason)
	}
	if err := saveNixPinUseState(statePath, d.State); err != nil {
		logger.Warn("pin expiry skipped: state file could not be written", "path", statePath, "error", err)
		return 0
	}
	if !receiptOK || len(d.Expired) == 0 || !openReceipt() {
		return 0
	}
	removed := 0
	for _, e := range d.Expired {
		if p.pinPreRemove != nil {
			p.pinPreRemove(e.Link)
		}
		if why := p.pinUnchanged(e); why != "" {
			logger.Warn("pin expiry kept a link that changed since the walk", "link", e.Link, "reason", why)
			continue
		}
		if !write("removed", e, "") {
			break
		}
		if err := os.Remove(e.Link); err != nil {
			logger.Warn("pin expiry could not remove link", "link", e.Link, "error", err)
			continue
		}
		delete(d.State.Links, e.Link)
		removed++
		logger.Info("expired lane pin", "link", e.Link, "target", e.Target, "mtime", e.MTime)
	}
	if err := saveNixPinUseState(statePath, d.State); err != nil {
		logger.Warn("could not record the pin expiry state", "path", statePath, "error", err)
	}
	return removed
}
