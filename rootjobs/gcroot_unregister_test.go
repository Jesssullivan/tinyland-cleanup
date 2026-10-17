package rootjobs

import (
	"bytes"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

type unregFixture struct {
	t     *testing.T
	base  string
	auto  string
	list  string
	lines []string
	now   time.Time
	n     int
	user  string
}

func newUnregFixture(t *testing.T) *unregFixture {
	t.Helper()
	u, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	if u.Uid == "0" {
		t.Skip("the fixture needs a non-root user")
	}
	base := t.TempDir()
	f := &unregFixture{t: t, base: base, auto: filepath.Join(base, "auto"), list: filepath.Join(base, "home", "gcroot-candidates"), user: u.Username}
	for _, dir := range []string{f.auto, filepath.Dir(f.list)} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// Every link and entry is created "now"; the run clock sits 80 h later,
	// so out-links pass the 72 h min age.
	f.now = time.Now().Add(80 * time.Hour)
	return f
}

func (f *unregFixture) link(rel, dest string) string {
	f.t.Helper()
	path := filepath.Join(f.base, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.Symlink(dest, path); err != nil {
		f.t.Fatal(err)
	}
	return path
}

func (f *unregFixture) register(link, resolvesTo string) string {
	f.t.Helper()
	f.n++
	name := "entry" + strings.Repeat("x", f.n)
	if resolvesTo == "" {
		resolvesTo = link
	}
	if err := os.Symlink(resolvesTo, filepath.Join(f.auto, name)); err != nil {
		f.t.Fatal(err)
	}
	f.lines = append(f.lines, name+"\t"+link)
	return name
}

// writeList writes the list with mtime at (now - age).
func (f *unregFixture) writeList(age time.Duration) {
	f.t.Helper()
	body := "# header\n" + strings.Join(f.lines, "\n") + "\n"
	if err := os.WriteFile(f.list, []byte(body), 0o600); err != nil {
		f.t.Fatal(err)
	}
	mt := f.now.Add(-age)
	if err := os.Chtimes(f.list, mt, mt); err != nil {
		f.t.Fatal(err)
	}
}

func (f *unregFixture) run(reportOnly bool, user string) (UnregisterSummary, string) {
	var out bytes.Buffer
	if user == "" {
		user = f.user
	}
	sum := GCRootUnregister(UnregisterOptions{
		AutoDir:          f.auto,
		Receipts:         filepath.Join(f.base, "receipts"),
		MaxReceiptsBytes: 1 << 20,
		ReportOnly:       reportOnly,
		MaxListAge:       time.Hour,
		MinLinkAge:       72 * time.Hour,
		Lists:            []ListSpec{{User: user, File: f.list}},
		Now:              func() time.Time { return f.now },
		Out:              &out,
	})
	return sum, out.String()
}

func (f *unregFixture) registered(name string) bool {
	_, err := os.Lstat(filepath.Join(f.auto, name))
	return err == nil
}

// TIN-5268: the out-link and every file in an agent-session tree survive;
// only the auto entry goes.
func TestGCRootUnregisterRemovesOnlyTheAutoEntry(t *testing.T) {
	f := newUnregFixture(t)
	session := "tmp-agents/claude-1000/-srv-repo/session-1/scratchpad"
	link := f.link(session+"/res-default", "/nix/store/aaaaaaaa-test")
	notes := filepath.Join(f.base, session, "notes.md")
	if err := os.WriteFile(notes, []byte("session notes\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	name := f.register(link, "")
	f.writeList(time.Minute)

	sum, out := f.run(false, "")
	if sum.Removed != 1 || f.registered(name) {
		t.Fatalf("the auto entry should be removed: %+v\n%s", sum, out)
	}
	if dest, err := os.Readlink(link); err != nil || dest != "/nix/store/aaaaaaaa-test" {
		t.Fatalf("the out-link must survive untouched: %q %v", dest, err)
	}
	if body, err := os.ReadFile(notes); err != nil || string(body) != "session notes\n" {
		t.Fatalf("session files must survive: %q %v", body, err)
	}
	if !strings.Contains(out, "unregistered user="+f.user) {
		t.Fatalf("missing receipt:\n%s", out)
	}
}

// M4: report-only removes nothing.
func TestGCRootUnregisterReportOnlyRemovesNothing(t *testing.T) {
	f := newUnregFixture(t)
	name := f.register(f.link("tmp/session/result", "/nix/store/aaaaaaaa-test"), "")
	f.writeList(time.Minute)
	sum, out := f.run(true, "")
	if sum.Would != 1 || sum.Removed != 0 || !f.registered(name) {
		t.Fatalf("report-only must keep the entry: %+v\n%s", sum, out)
	}
	if !strings.Contains(out, "would-unregister") || !strings.Contains(out, "mode=report-only") {
		t.Fatalf("missing report-only receipts:\n%s", out)
	}
}

// H2: a list the daemon has not rewritten within an hour is replayed state.
func TestGCRootUnregisterRefusesAStaleList(t *testing.T) {
	f := newUnregFixture(t)
	name := f.register(f.link("tmp/session/result", "/nix/store/aaaaaaaa-test"), "")
	f.writeList(2 * time.Hour)
	sum, out := f.run(false, "")
	if sum.Removed != 0 || !f.registered(name) || !strings.Contains(out, "older than 1h0m0s") {
		t.Fatalf("a 2 h old list must be refused whole: %+v\n%s", sum, out)
	}
}

// H2: a root recreated at the same path after the list was written is
// registered by a newer auto entry; the list cannot speak for it.
func TestGCRootUnregisterRefusesAnAutoEntryNewerThanTheList(t *testing.T) {
	f := newUnregFixture(t)
	link := f.link("tmp/session/result", "/nix/store/aaaaaaaa-test")
	name := f.register(link, "")
	f.writeList(time.Minute)
	entry := filepath.Join(f.auto, name)
	newer := f.now
	if err := lchtimes(entry, newer); err != nil {
		t.Fatal(err)
	}
	sum, out := f.run(false, "")
	if sum.Removed != 0 || !f.registered(name) || !strings.Contains(out, "auto entry is newer than the list") {
		t.Fatalf("a newer auto entry must be refused: %+v\n%s", sum, out)
	}
}

// H2: the helper re-checks the out-link age itself.
func TestGCRootUnregisterRefusesARecentlyChangedOutLink(t *testing.T) {
	f := newUnregFixture(t)
	f.now = time.Now().Add(time.Hour)
	name := f.register(f.link("tmp/session/result", "/nix/store/aaaaaaaa-test"), "")
	if err := lchtimes(filepath.Join(f.auto, name), time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	f.writeList(time.Minute)
	sum, out := f.run(false, "")
	if sum.Removed != 0 || !f.registered(name) || !strings.Contains(out, "inside 72h0m0s") {
		t.Fatalf("an out-link changed an hour ago must be refused: %+v\n%s", sum, out)
	}
}

func TestGCRootUnregisterRefusesPinsProfilesNonStoreAndMismatchedEntries(t *testing.T) {
	cases := []struct {
		name, rel, dest, reason string
		elsewhere               bool
	}{
		{"named pin", "state/gcroots/tin-2447/postgresql", "/nix/store/aaaaaaaa-pg", "under gcroots/ or profiles/", false},
		{"profile", "state/nix/profiles/profile-3-link", "/nix/store/aaaaaaaa-prof", "under gcroots/ or profiles/", false},
		{"non-store", "tmp/session/result", "/etc/passwd", "does not point straight into /nix/store", false},
		{"deep store", "tmp/session/deep", "/nix/store/aaaaaaaa-x/bin/sh", "does not point straight into /nix/store", false},
		{"resolves elsewhere", "tmp/session/listed", "/nix/store/aaaaaaaa-x", "does not resolve to the listed out-link", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newUnregFixture(t)
			link := f.link(tc.rel, tc.dest)
			target := ""
			if tc.elsewhere {
				target = f.link("tmp/session/other", tc.dest)
			}
			name := f.register(link, target)
			f.writeList(time.Minute)
			sum, out := f.run(false, "")
			if sum.Removed != 0 || !f.registered(name) || !strings.Contains(out, tc.reason) {
				t.Fatalf("expected refusal %q: %+v\n%s", tc.reason, sum, out)
			}
		})
	}
}

// Another user's out-link is refused. Root's profile links stand in for the
// foreign link, and the test asserts the owner it reads from them, not uid 0.
// That owner depends on the host. A Nix install made by a non-root user owns
// all of /nix, root's profiles included: GloriousFlywheel runner images give
// them uid 1001, and their unsandboxed nix build runs this test as nixbld1.
// In a user namespace that does not map root, they read as the overflow uid.
func TestGCRootUnregisterRefusesAnotherUsersLink(t *testing.T) {
	matches, _ := filepath.Glob("/nix/var/nix/profiles/per-user/root/profile-*-link")
	var foreign string
	owner := -1
	for _, m := range matches {
		info, err := os.Lstat(m)
		if err != nil {
			continue
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		dest, _ := os.Readlink(m)
		if ok && int(st.Uid) != os.Geteuid() && strings.HasPrefix(dest, "/nix/store/") {
			foreign, owner = m, int(st.Uid)
			break
		}
	}
	if foreign == "" {
		t.Skip("no link owned by another user that points into /nix/store on this host")
	}
	f := newUnregFixture(t)
	name := f.register(foreign, "")
	f.writeList(time.Minute)
	sum, out := f.run(false, "")
	// The list is created by this process, so its owner is the effective uid.
	want := fmt.Sprintf("reason=out-link is owned by uid %d, not the list owner %d", owner, os.Geteuid())
	if sum.Refused != 1 || sum.Removed != 0 || !f.registered(name) || !strings.Contains(out, want) {
		t.Fatalf("another user's link must be refused with %q: %+v\n%s", want, sum, out)
	}
}

func TestGCRootUnregisterRefusesAListDeclaredForAnotherUser(t *testing.T) {
	for _, declared := range []string{"root", "nobody"} {
		t.Run(declared, func(t *testing.T) {
			f := newUnregFixture(t)
			name := f.register(f.link("tmp/session/result", "/nix/store/aaaaaaaa-test"), "")
			f.writeList(time.Minute)
			sum, out := f.run(false, declared)
			if sum.Removed != 0 || !f.registered(name) || !strings.Contains(out, "refused user="+declared) {
				t.Fatalf("a list declared for %s must be refused: %+v\n%s", declared, sum, out)
			}
		})
	}
}

// L1: a FIFO list must not block the run.
func TestGCRootUnregisterDoesNotBlockOnAFIFO(t *testing.T) {
	f := newUnregFixture(t)
	if err := syscall.Mkfifo(f.list, 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	done := make(chan string, 1)
	go func() {
		_, out := f.run(false, "")
		done <- out
	}()
	select {
	case out := <-done:
		if !strings.Contains(out, "not a regular file") {
			t.Fatalf("a FIFO must be refused:\n%s", out)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the run blocked on a FIFO list")
	}
}

func TestGCRootUnregisterNeverEchoesARefusedLine(t *testing.T) {
	f := newUnregFixture(t)
	f.lines = append(f.lines, "secret-looking-content-without-a-tab")
	f.writeList(time.Minute)
	_, out := f.run(false, "")
	if strings.Contains(out, "secret-looking") || !strings.Contains(out, "reason=no tab separator") {
		t.Fatalf("refusals give a line number, never the line:\n%s", out)
	}
}

// M3: the receipts file is bounded.
func TestGCRootUnregisterRotatesReceipts(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "receipts")
	r := &receiptWriter{path: path, max: 200, out: &bytes.Buffer{}, now: time.Now}
	for i := 0; i < 20; i++ {
		r.write(strings.Repeat("x", 40))
	}
	info, err := os.Stat(path)
	if err != nil || info.Size() > 200 {
		t.Fatalf("receipts must stay under the bound: %v %v", info, err)
	}
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatalf("the previous receipts are kept as .1: %v", err)
	}
}

func TestRunGCRootUnregisterRejectsMalformedArguments(t *testing.T) {
	var out bytes.Buffer
	if rc := RunGCRootUnregister([]string{"jess"}, &out); rc != 2 {
		t.Fatalf("a bare user is not a USER=FILE pair: rc=%d", rc)
	}
	if rc := RunGCRootUnregister([]string{"jess=relative/path"}, &out); rc != 2 {
		t.Fatalf("a relative list path is refused: rc=%d", rc)
	}
	if rc := RunGCRootUnregister(nil, &out); rc != 2 {
		t.Fatalf("no lists is refused: rc=%d", rc)
	}
}
