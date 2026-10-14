package plugins

// agentsession.go is the compiled-in TIN-5268 agent-session deny set.
//
// TIN-5268 (preservation hold, ruling 605a2d1b): agent-session data is never
// deleted, aged out or lossily compressed. Lossless zstd archival compression
// stays allowed (R-C259) and lives in dev_artifacts, not here. The set below
// is compiled in: configuration (agent_session.extra_paths) can only add to
// it, and nothing can remove an entry.
//
// Every entry is resolved through symlinks, so a store that a host links
// elsewhere (sting links ~/.codex to /srv/fast-local/jess/state/codex) is
// denied at its physical path as well as its literal one. A path is denied
// when it, or any ancestor, is a denied tree; a removal of a directory that
// CONTAINS a denied tree is refused as well (ancestor refusal).

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// agentSessionHomeRelative are denied trees relative to $HOME. A "*" matches
// within one path element only.
var agentSessionHomeRelative = []string{
	".claude", ".codex", ".gstack", ".pi", ".kimi*", ".junie", ".opencode",
	".crush", ".fuzzybot", ".cmux*", ".tmux*",
	".config/claude", ".config/codex", ".config/opencode", ".config/junie",
	".config/pi", ".config/kimi*", ".config/cmux*", ".config/tmux", ".config/crush",
	".local/share/claude", ".local/share/codex", ".local/share/junie",
	".local/share/opencode", ".local/share/pi", ".local/share/kimi*",
	".local/share/cmux*", ".local/share/tmux", ".local/share/crush",
	".local/state/claude", ".local/state/codex", ".local/state/junie",
	".local/state/opencode", ".local/state/pi", ".local/state/kimi*",
	".local/state/cmux*", ".local/state/tmux",
	".cache/claude*", ".cache/codex*", ".cache/opencode", ".cache/pi",
	".cache/kimi*", ".cache/junie", ".cache/crush",
	"Library/Application Support/Claude", "Library/Application Support/Codex",
	"Library/Application Support/cmux", "Library/Application Support/com.cmuxterm.*",
	"Library/Caches/kimi-code", "Library/Caches/com.cmuxterm.*",
}

// agentSessionDirNames are per-repository session directories: a path element
// with one of these names is session data wherever it appears.
var agentSessionDirNames = []string{
	".claude", ".codex", ".crush", ".fuzzybot", ".gstack", ".pi", ".kimi*",
	".junie", ".opencode", ".cmux*", ".tmux*",
}

// agentSessionUIDPatterns are host-wide harness and multiplexer stores; %d is
// the user's uid.
var agentSessionUIDPatterns = []string{
	"/tmp/tmux-%d", "/private/tmp/tmux-%d",
	// tmux under TMUX_TMPDIR=$XDG_RUNTIME_DIR (sting's live sockets, 2026-10-13).
	"/run/user/%d/tmux-%d",
	"/tmp/claude-%d", "/private/tmp/claude-%d",
	"/srv/fast-local/tmp-agents/claude-%d",
}

// agentSessionAbsolute are host-wide stores with no uid in the name.
var agentSessionAbsolute = []string{
	"/srv/fast-local/tmp-agents/codex*",
}

// agentSessionTempNames are top-level entries of a temporary directory that
// hold agent-session data (R-W17 item 4, TIN-5694): Claude Code scratch under
// any suffix, Codex temp trees, the .tmp* trees Codex's tempfile crate makes,
// and Pi session trees. A "*" matches within the one path element.
var agentSessionTempNames = []string{"claude-*", "codex*", ".tmp*", "pi-*"}

// agentSessionTempRoots are the host temporary directories those names are
// denied in. $TMPDIR is added per cycle.
var agentSessionTempRoots = []string{"/tmp", "/private/tmp", "/var/tmp", "/private/var/tmp"}

// agentSessionTempProtectPrefixes returns the literal path prefixes of the
// agent-session trees directly under each root, in the form
// dev_artifacts.protect_paths matches (a string prefix). Each root is given
// literally and with its symlinks resolved.
func agentSessionTempProtectPrefixes(roots []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, root := range roots {
		if root == "" {
			continue
		}
		for _, r := range uniquePaths(filepath.Clean(root), resolvedOrSelf(root)) {
			for _, name := range agentSessionTempNames {
				prefix := r + string(os.PathSeparator) + strings.TrimSuffix(name, "*")
				if !seen[prefix] {
					seen[prefix] = true
					out = append(out, prefix)
				}
			}
		}
	}
	return out
}

// agentSessionDeny is one snapshot of the deny set: the literal patterns and
// the physical paths their existing matches resolve to.
type agentSessionDeny struct {
	patterns      []string // absolute, cleaned, one-element globs
	roots         []string // existing matches and their resolved targets
	carveouts     []string // expanded bazel_bases carve-out directories (literal and resolved)
	resolveErrors []string

	// Indexes for the walk's per-directory checks, built once per cycle.
	patternsByDir map[string][]string // pattern parent directory -> patterns
	slowPatterns  []string            // patterns with a glob above their last element
	rootSet       map[string]bool
	carveSet      map[string]bool
}

// newAgentSessionDeny builds the deny set for one cycle.
func newAgentSessionDeny(home string, uid int, tmpdir string, extra []string, carveouts []string) *agentSessionDeny {
	d := &agentSessionDeny{}
	add := func(pattern string) {
		if pattern == "" {
			return
		}
		d.patterns = append(d.patterns, filepath.Clean(pattern))
	}
	if home != "" {
		for _, rel := range agentSessionHomeRelative {
			add(filepath.Join(home, rel))
		}
	}
	for _, p := range agentSessionUIDPatterns {
		add(strings.ReplaceAll(p, "%d", strconv.Itoa(uid)))
	}
	if tmpdir != "" {
		add(filepath.Join(tmpdir, "tmux-"+strconv.Itoa(uid)))
		add(filepath.Join(tmpdir, "claude-"+strconv.Itoa(uid)))
	}
	tempRoots := agentSessionTempRoots
	if tmpdir != "" {
		tempRoots = append(append([]string{}, tempRoots...), tmpdir)
	}
	for _, root := range tempRoots {
		for _, name := range agentSessionTempNames {
			add(filepath.Join(root, name))
		}
	}
	for _, p := range agentSessionAbsolute {
		add(p)
	}
	for _, p := range extra {
		p = expandAgentSessionHome(p, home)
		if filepath.IsAbs(p) {
			add(p)
		}
	}
	seen := map[string]bool{}
	addRoot := func(p string) {
		p = filepath.Clean(p)
		if !seen[p] {
			seen[p] = true
			d.roots = append(d.roots, p)
		}
	}
	for _, pattern := range d.patterns {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			d.resolveErrors = append(d.resolveErrors, fmt.Sprintf("%s: %v", pattern, err))
			continue
		}
		for _, m := range matches {
			addRoot(m)
			if real, err := filepath.EvalSymlinks(m); err == nil {
				addRoot(real)
			} else if !os.IsNotExist(err) {
				d.resolveErrors = append(d.resolveErrors, fmt.Sprintf("%s: %v", m, err))
			}
		}
	}
	cseen := map[string]bool{}
	for _, pattern := range carveouts {
		pattern = expandAgentSessionHome(pattern, home)
		if !filepath.IsAbs(pattern) {
			continue
		}
		matches, _ := filepath.Glob(filepath.Clean(pattern))
		for _, m := range matches {
			for _, c := range []string{m, resolvedOrSelf(m)} {
				c = filepath.Clean(c)
				if !cseen[c] {
					cseen[c] = true
					d.carveouts = append(d.carveouts, c)
				}
			}
		}
	}
	d.index()
	return d
}

func (d *agentSessionDeny) index() {
	d.patternsByDir = map[string][]string{}
	d.rootSet = map[string]bool{}
	d.carveSet = map[string]bool{}
	for _, pattern := range d.patterns {
		dir := filepath.Dir(pattern)
		if strings.ContainsAny(dir, "*?[") {
			d.slowPatterns = append(d.slowPatterns, pattern)
			continue
		}
		d.patternsByDir[dir] = append(d.patternsByDir[dir], pattern)
	}
	for _, r := range d.roots {
		d.rootSet[r] = true
	}
	for _, c := range d.carveouts {
		d.carveSet[c] = true
	}
}

// bbUnder is a lexical "path is root or below it" for cleaned absolute paths.
func bbUnder(path, root string) bool {
	if path == root || root == "/" {
		return true
	}
	return strings.HasPrefix(path, root) && len(path) > len(root) && path[len(root)] == '/'
}

// defaultAgentSessionDeny is the compiled-in set for the current user with no
// configuration, used by shared deletion helpers that have no config.
func defaultAgentSessionDeny() *agentSessionDeny {
	home, _ := os.UserHomeDir()
	return newAgentSessionDeny(home, os.Getuid(), os.Getenv("TMPDIR"), nil, nil)
}

func expandAgentSessionHome(p, home string) string {
	if home != "" && (p == "~" || strings.HasPrefix(p, "~/")) {
		return filepath.Join(home, strings.TrimPrefix(p, "~"))
	}
	return p
}

func resolvedOrSelf(p string) string {
	p = filepath.Clean(p)
	if real, err := filepath.EvalSymlinks(p); err == nil {
		return real
	}
	// Resolve the nearest existing ancestor so a path that does not exist
	// (or no longer exists) is still compared at its physical location.
	rest := ""
	cur := p
	for {
		parent := filepath.Dir(cur)
		rest = filepath.Join(filepath.Base(cur), rest)
		if parent == cur {
			return p
		}
		if real, err := filepath.EvalSymlinks(parent); err == nil {
			return filepath.Join(real, rest)
		}
		cur = parent
	}
}

// denied reports why path is agent-session data: it or an ancestor matches a
// pattern or lies in a resolved store, or one of its elements is a
// per-repository session directory. Both the literal and the physical path
// are checked.
func (d *agentSessionDeny) denied(path string) (string, bool) {
	return d.deniedBelow(path, "")
}

// deniedBelow is denied restricted to the part of path strictly below
// boundary: ancestors at or above boundary, and stores containing boundary,
// do not count. With an empty boundary it is denied.
func (d *agentSessionDeny) deniedBelow(path, boundary string) (string, bool) {
	if d == nil {
		return "", false
	}
	for _, p := range uniquePaths(filepath.Clean(path), resolvedOrSelf(path)) {
		b := boundary
		if b != "" && !pathInsideRoot(p, b) {
			// The physical path may sit under the physical boundary.
			rb := resolvedOrSelf(boundary)
			if pathInsideRoot(p, rb) {
				b = rb
			} else {
				continue
			}
		}
		if why, ok := d.deniedPath(p, b); ok {
			return why, true
		}
	}
	return "", false
}

func (d *agentSessionDeny) deniedPath(p, boundary string) (string, bool) {
	if d.rootSet == nil {
		d.index()
	}
	p = filepath.Clean(p)
	for q := p; ; q = filepath.Dir(q) {
		if boundary != "" && (q == boundary || !bbUnder(q, boundary)) {
			break
		}
		for _, pattern := range d.patternsByDir[filepath.Dir(q)] {
			if ok, _ := filepath.Match(pattern, q); ok {
				return "agent-session store " + pattern + " (TIN-5268)", true
			}
		}
		for _, pattern := range d.slowPatterns {
			if ok, _ := filepath.Match(pattern, q); ok {
				return "agent-session store " + pattern + " (TIN-5268)", true
			}
		}
		if isAgentSessionDirName(filepath.Base(q)) {
			return "agent-session directory " + q + " (TIN-5268)", true
		}
		if d.rootSet[q] {
			return "inside agent-session store " + q + " (TIN-5268)", true
		}
		if q == "/" || q == "." || filepath.Dir(q) == q {
			break
		}
	}
	return "", false
}

// carveoutForLiteral is carveoutFor without symlink resolution, for physical
// walk paths.
func (d *agentSessionDeny) carveoutForLiteral(p string) string {
	if d == nil || len(d.carveouts) == 0 {
		return ""
	}
	if d.carveSet == nil {
		d.index()
	}
	for q := filepath.Clean(p); ; q = filepath.Dir(q) {
		if d.carveSet[q] {
			return q
		}
		if q == "/" || q == "." || filepath.Dir(q) == q {
			return ""
		}
	}
}

// leadsToCarveoutLiteral is leadsToCarveout without symlink resolution.
func (d *agentSessionDeny) leadsToCarveoutLiteral(p string) bool {
	if d == nil {
		return false
	}
	p = filepath.Clean(p)
	for _, c := range d.carveouts {
		if c != p && bbUnder(c, p) {
			return true
		}
	}
	return false
}

// containsDenied reports a denied store that lies inside path (or is path):
// removing path would remove it (ancestor refusal).
func (d *agentSessionDeny) containsDenied(path string) (string, bool) {
	if d == nil {
		return "", false
	}
	for _, p := range uniquePaths(filepath.Clean(path), resolvedOrSelf(path)) {
		for _, r := range d.roots {
			if pathInsideRoot(r, p) {
				return "contains agent-session store " + r + " (TIN-5268)", true
			}
		}
	}
	return "", false
}

// carveoutFor returns the carve-out directory path lies in, if any.
func (d *agentSessionDeny) carveoutFor(path string) string {
	if d == nil {
		return ""
	}
	for _, p := range uniquePaths(filepath.Clean(path), resolvedOrSelf(path)) {
		for _, c := range d.carveouts {
			if pathInsideRoot(p, c) {
				return c
			}
		}
	}
	return ""
}

// leadsToCarveout reports whether a carve-out directory lies strictly below
// path, so a walk may pass through a denied tree to reach it.
func (d *agentSessionDeny) leadsToCarveout(path string) bool {
	if d == nil {
		return false
	}
	for _, p := range uniquePaths(filepath.Clean(path), resolvedOrSelf(path)) {
		for _, c := range d.carveouts {
			if pathInsideRoot(c, p) && filepath.Clean(c) != p {
				return true
			}
		}
	}
	return false
}

// isAgentSessionDirName reports whether a directory name is a per-repository
// session directory.
func isAgentSessionDirName(name string) bool {
	for _, glob := range agentSessionDirNames {
		if ok, _ := filepath.Match(glob, name); ok {
			return true
		}
	}
	return false
}

func uniquePaths(paths ...string) []string {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		dup := false
		for _, q := range out {
			if q == p {
				dup = true
			}
		}
		if !dup {
			out = append(out, p)
		}
	}
	return out
}
