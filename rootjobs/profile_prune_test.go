package rootjobs

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type pruneFixture struct {
	t     *testing.T
	home  string
	store string
	log   string
	opts  PruneOptions
}

func newPruneFixture(t *testing.T) *pruneFixture {
	t.Helper()
	base := t.TempDir()
	f := &pruneFixture{t: t, home: filepath.Join(base, "home"), store: filepath.Join(base, "store"), log: filepath.Join(base, "calls")}
	bin := filepath.Join(base, "bin")
	for _, d := range []string{bin, f.store, filepath.Join(f.home, ".local/state/nix/profiles"), filepath.Join(f.home, ".local/state/home-manager/gcroots")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	nixEnv := filepath.Join(bin, "nix-env")
	if err := os.WriteFile(nixEnv, []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> "+f.log+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	runuser := filepath.Join(bin, "runuser")
	if err := os.WriteFile(runuser, []byte("#!/bin/sh\nprintf 'as %s\\n' \"$2\" >> "+f.log+"\nshift 3\nexec \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	f.opts = PruneOptions{
		KeepPrevious: 1,
		Users:        []string{"jess"},
		NixEnv:       nixEnv,
		Runuser:      runuser,
		RootProfile:  "",
		HomeOf:       func(string) (string, error) { return f.home, nil },
	}
	return f
}

func (f *pruneFixture) generation(profile string, n int, current bool) {
	f.t.Helper()
	target := filepath.Join(f.store, filepath.Base(profile)+"-gen")
	_ = os.MkdirAll(target, 0o755)
	link := profile + "-" + itoa(n) + "-link"
	if err := os.Symlink(target, link); err != nil {
		f.t.Fatal(err)
	}
	if current {
		if err := os.Symlink(filepath.Base(link), profile); err != nil {
			f.t.Fatal(err)
		}
	}
}

func itoa(n int) string { return string(rune('0' + n)) }

func (f *pruneFixture) calls() string {
	raw, _ := os.ReadFile(f.log)
	return string(raw)
}

// R-C795: keep 1 previous is the "+2" selector, run as the user for the
// user's own profiles.
func TestProfilePruneKeepsOnePreviousAsTheUser(t *testing.T) {
	f := newPruneFixture(t)
	hm := filepath.Join(f.home, ".local/state/nix/profiles/home-manager")
	f.generation(hm, 7, true)
	if err := os.Symlink(filepath.Join(f.store, "home-manager-gen"), filepath.Join(f.home, ".local/state/home-manager/gcroots/current-home")); err != nil {
		t.Fatal(err)
	}
	user := filepath.Join(f.home, ".local/state/nix/profiles/profile")
	f.generation(user, 3, true)

	var out bytes.Buffer
	f.opts.Out = &out
	if err := ProfilePrune(f.opts); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	calls := f.calls()
	if strings.Count(calls, "as jess") != 2 || !strings.Contains(calls, "--profile "+hm+" --delete-generations +2") || !strings.Contains(calls, "--profile "+user+" --delete-generations +2") {
		t.Fatalf("unexpected calls:\n%s", calls)
	}
}

func TestProfilePruneRefusesDisagreeingHomeManagerRoots(t *testing.T) {
	f := newPruneFixture(t)
	hm := filepath.Join(f.home, ".local/state/nix/profiles/home-manager")
	f.generation(hm, 7, true)
	other := filepath.Join(f.store, "other")
	_ = os.MkdirAll(other, 0o755)
	if err := os.Symlink(other, filepath.Join(f.home, ".local/state/home-manager/gcroots/current-home")); err != nil {
		t.Fatal(err)
	}
	if err := ProfilePrune(f.opts); err == nil || strings.Contains(f.calls(), "delete-generations") {
		t.Fatalf("disagreeing roots must prune nothing: err=%v calls=%q", err, f.calls())
	}
}

func TestProfilePruneReportOnlyPrunesNothing(t *testing.T) {
	f := newPruneFixture(t)
	user := filepath.Join(f.home, ".local/state/nix/profiles/profile")
	f.generation(user, 3, true)
	f.opts.ReportOnly = true
	var out bytes.Buffer
	f.opts.Out = &out
	if err := ProfilePrune(f.opts); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(f.calls(), "delete-generations") || !strings.Contains(out.String(), "would run") {
		t.Fatalf("report-only must not prune: calls=%q out=%q", f.calls(), out.String())
	}
}

func TestRunProfilePruneRefusesMoreThanOnePrevious(t *testing.T) {
	var out bytes.Buffer
	if rc := RunProfilePrune([]string{"--keep-previous", "2", "jess"}, &out); rc != 2 || !strings.Contains(out.String(), "R-C43") {
		t.Fatalf("keep-previous 2 must be refused: rc=%d %s", rc, out.String())
	}
}
