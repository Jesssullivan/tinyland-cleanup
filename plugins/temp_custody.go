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
	"os/exec"
	"path/filepath"
	"runtime"
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

func temporaryRootActivityReason(roots map[string]string, path string) string {
	if reason := roots["*"]; reason != "" {
		return reason
	}
	return roots[canonicalTempArtifactPath(path)]
}

func (p *DevArtifactsPlugin) activeTemporaryRoots(ctx context.Context, paths []string, home string) map[string]string {
	if p.tempActivity == nil {
		return activeTempArtifactRoots(ctx, paths, home)
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
// Any incomplete inventory is unknown activity and protects every root.
func inspectTemporaryRootActivity(ctx context.Context, paths []string, home string) (map[string]string, error) {
	roots := map[string]string{}
	if len(paths) == 0 {
		return roots, nil
	}
	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	output, err := exec.CommandContext(probeCtx, "ps", "-axo", "comm=,args=").Output()
	if err != nil {
		return roots, fmt.Errorf("ps: %w", err)
	}
	roots = tempArtifactRootsFromProcessOutput(string(output), paths, home)
	// NUL-delimited fields preserve spaces and newlines in paths; cwd records
	// are included by default alongside regular file descriptors.
	if runtime.GOOS == "linux" {
		files, err := linuxTemporaryRootActivity(probeCtx, paths, home)
		for path, reason := range files {
			roots[path] = reason
		}
		return roots, err
	}
	var stderr bytes.Buffer
	cmd := exec.CommandContext(probeCtx, "lsof", "-nP", "-a", "-u", strconv.Itoa(os.Geteuid()), "-F0pn")
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
// same-user observation. Permission failures for an owned process remain unknown.
func linuxTemporaryRootActivity(ctx context.Context, paths []string, home string) (map[string]string, error) {
	return temporaryRootActivityFromProc(ctx, "/proc", paths, home)
}

func temporaryRootActivityFromProc(ctx context.Context, proc string, paths []string, home string) (map[string]string, error) {
	roots := map[string]string{}
	entries, err := os.ReadDir(proc)
	if err != nil {
		return roots, err
	}
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
		record := func(path string) {
			if root := tempArtifactRootForPath(strings.TrimSuffix(path, " (deleted)"), paths, home); root != "" {
				roots[root] = "cwd/open file held by pid " + entry.Name()
			}
		}
		cwd, err := os.Readlink(filepath.Join(process, "cwd"))
		if os.IsNotExist(err) {
			continue
		} // exited process or kernel thread
		if err != nil {
			return roots, fmt.Errorf("owned process cwd unavailable")
		}
		record(cwd)
		fds, err := os.ReadDir(filepath.Join(process, "fd"))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return roots, fmt.Errorf("owned process open files unavailable")
		}
		for _, fd := range fds {
			path, err := os.Readlink(filepath.Join(process, "fd", fd.Name()))
			if os.IsNotExist(err) {
				continue
			} // closed after the fd snapshot
			if err != nil {
				return roots, fmt.Errorf("owned process file identity unavailable")
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
func temporaryRootCustodyReason(ctx context.Context, root string) string {
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "temporary root identity is unavailable or not a real directory"
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
	if reason := temporaryRootCustodyReason(ctx, root); reason != "" {
		return reason
	}
	home, _ := os.UserHomeDir()
	if reason := temporaryRootActivityReason(p.activeTemporaryRoots(ctx, cfg.TempScanPaths, home), root); reason != "" {
		return reason
	}
	newest := staleModTime(current)
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
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
	entries, err := os.ReadDir(scanPath)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() || isNixTemporaryRootName(entry.Name()) || isHarnessScratchContainer(entry.Name(), cfg) {
			continue
		}
		root := filepath.Join(scanPath, entry.Name())
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
		target := CleanupTarget{Type: "disposable-temp-root", Name: entry.Name(), Path: root, Action: "protect", Protected: true, Reason: reason}
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
