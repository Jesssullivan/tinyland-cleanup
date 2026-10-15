package plugins

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Jesssullivan/tinyland-cleanup/config"
)

const disposableTempReceipt = ".tinyland-cleanup-disposable.json"

type temporaryCustodyReceipt struct {
	Path     string `json:"path"`
	OwnerUID int    `json:"owner_uid"`
	State    string `json:"state"`
	Receipt  string `json:"receipt"`
}

// tempScanRoot is one top-level entry of a temp scan path.
type tempScanRoot struct {
	name string
	path string
	info os.FileInfo // Lstat: a symlink is not a directory
}

// tempScanRootsOldestFirst lists the top-level entries of a temp scan path,
// oldest first by the modification time the staleness checks use, then by
// name. temp_scan_max_roots bounds how many roots one cycle inspects per
// path; taking the oldest first spends that budget on the roots most likely
// to be stale, a reclaimed root leaves the list, and the next cycle reaches
// further. Before this, entries came in name order, so every cycle spent the
// budget on the same lexically first roots. Entries that vanish before they
// can be inspected are left out.
func tempScanRootsOldestFirst(scanPath string) ([]tempScanRoot, error) {
	entries, err := os.ReadDir(scanPath)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	roots := make([]tempScanRoot, 0, len(entries))
	times := make([]time.Time, 0, len(entries))
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			continue
		}
		roots = append(roots, tempScanRoot{name: entry.Name(), path: filepath.Join(scanPath, entry.Name()), info: info})
		times = append(times, effectiveModTime(info, now))
	}
	order := make([]int, len(roots))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool {
		ta, tb := times[order[a]], times[order[b]]
		if !ta.Equal(tb) {
			return ta.Before(tb)
		}
		return roots[order[a]].name < roots[order[b]].name
	})
	sorted := make([]tempScanRoot, len(roots))
	for i, j := range order {
		sorted[i] = roots[j]
	}
	return sorted, nil
}

func temporaryRootActivityReason(roots map[string]string, path string) string {
	if reason := roots["*"]; reason != "" {
		return reason
	}
	return roots[canonicalTempArtifactPath(path)]
}

func (p *DevArtifactsPlugin) activeTemporaryRoots(ctx context.Context, cfg config.DevArtifactsConfig, home string) map[string]string {
	paths := cfg.TempScanPaths
	if p.tempActivity == nil {
		return activeTempArtifactRoots(ctx, paths, home, cfg.TempUninspectableOKComms)
	}
	roots, err := p.tempActivity(ctx, paths, home)
	if roots == nil {
		roots = map[string]string{}
	}
	if err != nil {
		roots["*"] = "temporary process activity could not be verified: " + err.Error()
	}
	return roots
}

// Inspect every same-user process, including shell cwd and non-tool open files.
// Any incomplete inventory is unknown activity and protects every root. On
// Linux, okComms names same-user processes whose cwd or open files may be
// unreadable without making the inventory incomplete (see
// temporaryRootActivityFromProc).
func inspectTemporaryRootActivity(ctx context.Context, paths []string, home string, okComms []string) (map[string]string, error) {
	roots := map[string]string{}
	if len(paths) == 0 {
		return roots, nil
	}
	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	output, err := commandContext(probeCtx, "ps", "-axo", "comm=,args=").Output()
	if err != nil {
		return roots, fmt.Errorf("ps: %w", err)
	}
	roots = tempArtifactRootsFromProcessOutput(string(output), paths, home)
	// NUL-delimited fields preserve spaces and newlines in paths; cwd records
	// are included by default alongside regular file descriptors.
	if runtime.GOOS == "linux" {
		files, err := linuxTemporaryRootActivity(probeCtx, paths, home, okComms)
		for path, reason := range files {
			roots[path] = reason
		}
		return roots, err
	}
	var stderr bytes.Buffer
	cmd := commandContext(probeCtx, "lsof", "-nP", "-a", "-u", strconv.Itoa(os.Geteuid()), "-F0pn")
	cmd.Stderr = &stderr
	output, err = cmd.Output()
	if err != nil || stderr.Len() != 0 {
		return roots, fmt.Errorf("cwd/open-file inventory unavailable or incomplete")
	}
	for path, reason := range tempRootsFromOpenFiles(string(output), paths, home) {
		roots[path] = reason
	}
	return roots, nil
}

// Linux /proc keeps unrelated inaccessible cluster mounts outside this
// same-user observation. Permission failures for an owned process remain
// unknown unless the process is a zombie or its comm is in okComms.
func linuxTemporaryRootActivity(ctx context.Context, paths []string, home string, okComms []string) (map[string]string, error) {
	return temporaryRootActivityFromProc(ctx, "/proc", paths, home, okComms)
}

// tempProcReadlink and tempProcReadDir read a process's cwd and fd links.
// Tests replace them to simulate a process this user may not inspect.
var (
	tempProcReadlink = os.Readlink
	tempProcReadDir  = os.ReadDir
)

// temporaryRootActivityFromProc maps temp roots to the same-user process that
// holds them through its cwd or an open file. A same-user process whose cwd or
// open files cannot be read makes the inventory incomplete (the error), which
// protects every temp root, with two exceptions that leave it complete: a
// zombie (state Z), which holds no cwd or files, and a process whose comm is
// in okComms, a reviewed list of non-dumpable helpers that never work inside
// a temp root (dev_artifacts.temp_uninspectable_ok_comms; the bazel_bases
// lane has the same rule as uninspectable_ok_comms). What such a process
// does expose is still recorded.
func temporaryRootActivityFromProc(ctx context.Context, proc string, paths []string, home string, okComms []string) (map[string]string, error) {
	roots := map[string]string{}
	entries, err := os.ReadDir(proc)
	if err != nil {
		return roots, err
	}
	ok := procCommSet(okComms)
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return roots, err
		}
		if _, err := strconv.Atoi(entry.Name()); err != nil {
			continue
		}
		process := filepath.Join(proc, entry.Name())
		status, err := os.ReadFile(filepath.Join(process, "status"))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return roots, fmt.Errorf("process ownership unavailable")
		}
		uid := -1
		for _, line := range strings.Split(string(status), "\n") {
			fields := strings.Fields(line)
			if len(fields) >= 3 && fields[0] == "Uid:" {
				uid, err = strconv.Atoi(fields[2])
				break
			}
		}
		if err != nil || uid < 0 {
			return roots, fmt.Errorf("process ownership unknown")
		}
		if uid != os.Geteuid() {
			continue
		}
		// uninspectable decides an owned process whose cwd or open files
		// could not be read: nil leaves the inventory complete.
		uninspectable := func(what string) error {
			comm, state := procCommState(process)
			if state == "Z" || ok[comm] {
				return nil
			}
			return fmt.Errorf("owned process %s unavailable: pid %s (%s)", what, entry.Name(), comm)
		}
		record := func(path string) {
			if root := tempArtifactRootForPath(strings.TrimSuffix(path, " (deleted)"), paths, home); root != "" {
				roots[root] = "cwd/open file held by pid " + entry.Name()
			}
		}
		cwd, err := tempProcReadlink(filepath.Join(process, "cwd"))
		if os.IsNotExist(err) {
			continue
		} // exited process or kernel thread
		if err != nil {
			if err := uninspectable("cwd"); err != nil {
				return roots, err
			}
		} else {
			record(cwd)
		}
		fds, err := tempProcReadDir(filepath.Join(process, "fd"))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			if err := uninspectable("open files"); err != nil {
				return roots, err
			}
			continue
		}
		for _, fd := range fds {
			path, err := tempProcReadlink(filepath.Join(process, "fd", fd.Name()))
			if os.IsNotExist(err) {
				continue
			} // closed after the fd snapshot
			if err != nil {
				if err := uninspectable("file identity"); err != nil {
					return roots, err
				}
				break
			}
			if filepath.IsAbs(path) {
				record(path)
			}
		}
	}
	return roots, nil
}

func tempRootsFromOpenFiles(output string, paths []string, home string) map[string]string {
	roots := map[string]string{}
	pid := "unknown"
	for _, field := range strings.Split(output, "\x00") {
		field = strings.TrimLeft(field, "\n")
		if len(field) < 2 {
			continue
		}
		switch field[0] {
		case 'p':
			pid = field[1:]
		case 'n':
			path := strings.TrimSuffix(field[1:], " (deleted)")
			if root := tempArtifactRootForPath(path, paths, home); root != "" {
				roots[root] = "cwd/open file held by pid " + pid
			}
		}
	}
	return roots
}

// Never claim a Git tree is disposable, even when it is clean and pushed.
// Dirty/untracked/ignored content and nested worktrees remain Git-owner custody.
func temporaryRootCustodyReason(ctx context.Context, root string, inventories ...temporaryMountInventory) string {
	var inventory temporaryMountInventory
	if len(inventories) > 0 {
		inventory = inventories[0]
	}
	if reason := temporaryMountProtectReason(ctx, root, inventory); reason != "" {
		return reason
	}
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "temporary root identity is unavailable or not a real directory"
	}
	for ancestor := root; ; ancestor = filepath.Dir(ancestor) {
		if _, err := os.Lstat(filepath.Join(ancestor, ".git")); err == nil {
			return "temporary root is inside Git-owner custody"
		} else if !os.IsNotExist(err) {
			return "ancestor Git custody unavailable"
		}
		if filepath.Dir(ancestor) == ancestor {
			break
		}
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(st.Uid) != os.Geteuid() {
		return "temporary root belongs to a different or unknown user"
	}
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	count := 0
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := probeCtx.Err(); err != nil {
			return err
		}
		count++
		if count > 100000 {
			return fmt.Errorf("custody scan bound exceeded")
		}
		if entry.Name() == ".git" {
			return fmt.Errorf("Git worktree remains under Git-owner custody")
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		current, ok := info.Sys().(*syscall.Stat_t)
		if !ok || current.Dev != st.Dev || int(current.Uid) != os.Geteuid() {
			return fmt.Errorf("foreign owner, mount or unknown identity in temporary root")
		}
		return nil
	})
	if err != nil {
		return err.Error()
	}
	return ""
}

func releasedTemporaryRootReason(root string) string {
	path := filepath.Join(root, disposableTempReceipt)
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 4096 {
		return "disposable root custody receipt unavailable"
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "disposable root custody receipt unreadable"
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil || fields["owner_uid"] == nil {
		return "disposable root custody receipt has no explicit owner"
	}
	var receipt temporaryCustodyReceipt
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&receipt) != nil || decoder.Decode(new(any)) != io.EOF || receipt.State != "released" || receipt.OwnerUID != os.Geteuid() || receipt.Receipt == "" || filepath.Clean(receipt.Path) != canonicalTempArtifactPath(root) {
		return "disposable root custody receipt invalid or not released for this identity"
	}
	return ""
}

func (p *DevArtifactsPlugin) recheckTemporaryRoot(ctx context.Context, root string, previous os.FileInfo, age time.Duration, cfg config.DevArtifactsConfig) string {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	current, err := os.Lstat(root)
	if err != nil || !os.SameFile(previous, current) {
		return "temporary root identity changed"
	}
	if harnessSessionProtected(p, root, cfg.ProtectPaths) {
		return "root contains a protected path"
	}
	if reason := temporaryRootCustodyReason(ctx, root, p.tempMounts); reason != "" {
		return reason
	}
	home, _ := os.UserHomeDir()
	if reason := temporaryRootActivityReason(p.activeTemporaryRoots(ctx, cfg, home), root); reason != "" {
		return reason
	}
	if reason := temporaryRootIdleReason(ctx, root, age); reason != "" {
		return reason
	}
	// The walk/process checks do not hold a mount lease: observe again last.
	return temporaryMountProtectReason(ctx, root, p.tempMounts)
}

func temporaryRootIdleReason(ctx context.Context, root string, age time.Duration) string {
	newest := time.Time{}
	count := 0
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		count++
		if count > 100000 {
			return fmt.Errorf("idle scan bound exceeded")
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if staleModTime(info).After(newest) {
			newest = staleModTime(info)
		}
		return nil
	})
	if err != nil {
		return "temporary root idle age could not be verified"
	}
	if age <= 0 || newest.After(time.Now().Add(-age)) {
		return "temporary root contains recent activity"
	}

	return ""
}

func (p *DevArtifactsPlugin) planDisposableTemporaryRoots(ctx context.Context, scanPath, home string, level CleanupLevel, cfg config.DevArtifactsConfig, activity map[string]string, targets *[]CleanupTarget, budget *devArtifactScanBudget) {
	if !cfg.TempRootCleanup {
		return
	}
	age := parseNixPolicyDuration(cfg.TempArtifactStaleAfter, 7*24*time.Hour)
	entries, err := tempScanRootsOldestFirst(scanPath)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !entry.info.IsDir() || isNixTemporaryRootName(entry.name) || isHarnessScratchContainer(entry.name, cfg) {
			continue
		}
		root := entry.path
		if !pathExists(filepath.Join(root, disposableTempReceipt)) {
			continue
		}
		if err := budget.checkTempRoot(ctx, root); err != nil {
			return
		}
		info, err := os.Lstat(root)
		if err != nil {
			continue
		}
		reason := releasedTemporaryRootReason(root)
		if reason == "" {
			reason = temporaryRootActivityReason(activity, root)
		}
		if reason == "" {
			reason = p.recheckTemporaryRoot(ctx, root, info, age, cfg)
		}
		target := CleanupTarget{Type: "disposable-temp-root", Name: entry.name, Path: root, Action: "protect", Protected: true, Reason: reason}
		if reason == "" {
			target.Bytes, err = getDirAllocatedBytesContext(ctx, root)
			if err != nil {
				target.Reason = "temporary root size could not be verified"
			} else if level < LevelAggressive {
				target.Action = "report"
				target.Reason = "root reaping requires aggressive or critical cleanup level"
			} else {
				target.Action = "delete"
				target.Protected = false
				target.Reason = "released disposable root is idle past configured age"
			}
		}
		annotateCleanupTargetPolicy(&target, CleanupTierDestructive, hostReclaimForAction(target.Action))
		*targets = append(*targets, target)
	}
}

func (p *DevArtifactsPlugin) cleanDisposableTemporaryRoots(ctx context.Context, scanPath, home string, level CleanupLevel, cfg config.DevArtifactsConfig, activity map[string]string, logger *slog.Logger, budget *devArtifactScanBudget) int64 {
	identities := map[string]os.FileInfo{}
	entries, err := os.ReadDir(scanPath)
	if err != nil {
		return 0
	}
	for _, entry := range entries {
		root := filepath.Join(scanPath, entry.Name())
		if info, err := os.Lstat(root); err == nil {
			identities[root] = info
		}
	}
	var targets []CleanupTarget
	p.planDisposableTemporaryRoots(ctx, scanPath, home, level, cfg, activity, &targets, budget)
	var freed int64
	for _, target := range targets {
		if target.Action != "delete" {
			continue
		}
		info := identities[target.Path]
		if info == nil {
			continue
		}
		// Fresh check immediately before mutation; a planning decision is no lease.
		if releasedTemporaryRootReason(target.Path) != "" || p.recheckTemporaryRoot(ctx, target.Path, info, parseNixPolicyDuration(cfg.TempArtifactStaleAfter, 7*24*time.Hour), cfg) != "" {
			continue
		}
		if err := p.removeDevArtifactPath(target.Path, logger); err != nil {
			continue
		}
		freed += target.Bytes
	}
	return freed
}
