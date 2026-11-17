package plugins

// home_lanes_git.go decides whether a Git checkout inside a stale work entry
// holds work that exists nowhere else (TIN-8526, operator ruling
// R-STING-HOME-20261117-c "Home budget rules").
//
// Each question to git is closed: an empty or non-empty listing, or one
// integer. A git that is missing, fails, times out or prints anything else
// keeps the entry.

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	// homeLanesGitTimeout bounds each git command of a checkout probe. A
	// probe that runs out of time keeps the entry.
	homeLanesGitTimeout = 20 * time.Second
	// homeLanesMaxCheckouts bounds the checkouts one entry may hold. An
	// entry with more is kept unprobed.
	homeLanesMaxCheckouts = 64
)

// homeLanesGitKeepReason returns why the checkout at dir must stay, or ""
// when git answered every question and the answer was "nothing unsaved".
// The questions, in order:
//
//   - the top level git reports for dir is dir itself, so a broken .git
//     cannot make git answer for a parent repository;
//   - another work tree does not depend on this repository (.git/worktrees);
//   - git status lists nothing, untracked files included;
//   - no stash exists;
//   - no commit reachable from HEAD, a local branch or a local tag is
//     missing from every remote-tracking ref. A repository with no remote
//     therefore always stays, and so does a HEAD on no remote.
//
// GIT_OPTIONAL_LOCKS=0 stops git status from rewriting the index, so a probe
// does not refresh the age it was asked about. core.fsmonitor is off so a
// repository's configuration starts no hook.
func (p *HomeLanesPlugin) homeLanesGitKeepReason(ctx context.Context, dir string) string {
	gitPath := p.gitPath
	if gitPath == "" {
		found, err := exec.LookPath("git")
		if err != nil {
			return "git checkout " + dir + " cannot be verified: git is unavailable"
		}
		gitPath = found
	}
	timeout := p.gitTimeout
	if timeout <= 0 {
		timeout = homeLanesGitTimeout
	}
	run := func(args ...string) ([]byte, error) {
		cctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		full := append([]string{"-c", "core.fsmonitor=false", "-C", dir}, args...)
		cmd := commandContext(cctx, gitPath, full...)
		cmd.Env = append(os.Environ(),
			"GIT_OPTIONAL_LOCKS=0",
			"GIT_TERMINAL_PROMPT=0",
			"GIT_CEILING_DIRECTORIES="+filepath.Dir(dir),
			"LC_ALL=C",
		)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err == nil && cctx.Err() != nil {
			err = cctx.Err()
		}
		return out, err
	}
	unknown := func(what string, err error) string {
		return "git checkout " + dir + " kept: " + what + " could not be verified: " + err.Error()
	}

	out, err := run("rev-parse", "--show-toplevel")
	if err != nil {
		return unknown("top level", err)
	}
	top := strings.TrimSpace(string(out))
	want, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return unknown("checkout path", err)
	}
	if got, err := filepath.EvalSymlinks(top); err != nil || got != want {
		return "git checkout " + dir + " kept: git answered for " + top
	}

	if gitDir := filepath.Join(dir, ".git"); isRealDir(gitDir) {
		entries, err := os.ReadDir(filepath.Join(gitDir, "worktrees"))
		switch {
		case err == nil && len(entries) > 0:
			return "git checkout " + dir + " kept: other work trees use this repository"
		case err != nil && !errors.Is(err, os.ErrNotExist):
			return unknown("linked work trees", err)
		}
	}

	out, err = run("status", "--porcelain=v1", "-z", "--untracked-files=all", "--ignore-submodules=none")
	if err != nil {
		return unknown("status", err)
	}
	if len(out) != 0 {
		return "git checkout " + dir + " has uncommitted changes"
	}

	out, err = run("for-each-ref", "--format=%(refname)", "refs/stash")
	if err != nil {
		return unknown("stash", err)
	}
	if len(bytes.TrimSpace(out)) != 0 {
		return "git checkout " + dir + " has a stash"
	}

	out, err = run("rev-list", "--count", "HEAD", "--branches", "--tags", "--not", "--remotes")
	if err != nil {
		return unknown("unpushed commits", err)
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return unknown("unpushed commit count", err)
	}
	if n != 0 {
		return "git checkout " + dir + " has " + strconv.Itoa(n) + " commit(s) on no remote"
	}
	return ""
}

func isRealDir(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.IsDir()
}
