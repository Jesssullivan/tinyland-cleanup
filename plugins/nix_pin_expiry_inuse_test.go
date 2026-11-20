package plugins

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The tests below drive the in-use check through a fake inventory: no real
// process is inspected and none is started.

func pinTarget(f *pinFixture, rel string) string {
	return filepath.Join(f.store, "abc-"+filepath.Base(rel))
}

func pinReceipts(t *testing.T, f *pinFixture) []nixPinExpiryReceipt {
	t.Helper()
	raw, err := os.ReadFile(f.receipt)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var out []nixPinExpiryReceipt
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if line == "" {
			continue
		}
		var r nixPinExpiryReceipt
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("receipt line %q: %v", line, err)
		}
		out = append(out, r)
	}
	return out
}

func TestPinExpirySparesTargetHeldUnderIt(t *testing.T) {
	f := newPinFixture(t)
	used := f.pin(t, "gcroots/kitpip-0.1.0-x86_64-linux", 22)
	idle := f.pin(t, "gcroots/idle", 22)
	f.activity.index(filepath.Join(pinTarget(f, used), "bin", "kitpip"), "pid 4242 maps "+pinTarget(f, used)+"/bin/kitpip", nil)

	if n := f.p.expirePins(context.Background(), f.cfg, pinLogger()); n != 1 {
		t.Fatalf("removed %d, want 1 (the idle pin)", n)
	}
	if !pinExists(used) || pinExists(idle) {
		t.Fatalf("used kept=%v idle removed=%v", pinExists(used), !pinExists(idle))
	}
	rs := pinReceipts(t, f)
	if len(rs) != 2 || rs[0].Action != "spared" || rs[0].Link != used || !strings.Contains(rs[0].Reason, "in_use") || !strings.Contains(rs[0].Reason, "pid 4242") {
		t.Fatalf("receipts = %+v", rs)
	}
	if rs[1].Action != "removed" || rs[1].Link != idle {
		t.Fatalf("removal receipt = %+v", rs[1])
	}
	// A second pass spares it again but writes no second receipt line.
	if n := f.p.expirePins(context.Background(), f.cfg, pinLogger()); n != 0 {
		t.Fatalf("second pass removed %d", n)
	}
	if rs := pinReceipts(t, f); len(rs) != 2 {
		t.Fatalf("second pass wrote another receipt: %+v", rs)
	}
}

func TestPinExpirySparesLinkNamedOnCommandLine(t *testing.T) {
	f := newPinFixture(t)
	viaLink := f.pin(t, "a/by-link", 30)
	viaTarget := f.pin(t, "a/by-target", 30)
	f.activity.procs = []bbProc{
		{pid: 10, cmdline: "python " + viaLink + "/bin/run --flag"},
		{pid: 11, cmdline: pinTarget(f, viaTarget) + "/bin/tool"},
	}
	if n := f.p.expirePins(context.Background(), f.cfg, pinLogger()); n != 0 {
		t.Fatalf("removed %d, want 0", n)
	}
	if !pinExists(viaLink) || !pinExists(viaTarget) {
		t.Fatal("a pin named on a command line was removed")
	}
}

func TestPinExpiryIncompleteInventorySparesAll(t *testing.T) {
	f := newPinFixture(t)
	a := f.pin(t, "a", 30)
	b := f.pin(t, "b", 30)
	f.activity.incomplete = []string{"pid 7 (agent) fd unreadable: permission denied"}
	if n := f.p.expirePins(context.Background(), f.cfg, pinLogger()); n != 0 {
		t.Fatalf("removed %d on an incomplete inventory", n)
	}
	if !pinExists(a) || !pinExists(b) {
		t.Fatal("a link was removed on an incomplete inventory")
	}
	for _, r := range pinReceipts(t, f) {
		if r.Action != "spared" || !strings.Contains(r.Reason, nixPinSpareIncomplete) {
			t.Fatalf("receipt = %+v", r)
		}
	}
	// A failed lsof listing counts the same way.
	f.activity = newBBActivity()
	f.activity.lsofError = "lsof listing failed: exit status 1"
	if n := f.p.expirePins(context.Background(), f.cfg, pinLogger()); n != 0 {
		t.Fatalf("removed %d on a failed lsof listing", n)
	}
	// The inventory complete again: both go.
	f.activity = newBBActivity()
	if n := f.p.expirePins(context.Background(), f.cfg, pinLogger()); n != 2 {
		t.Fatalf("removed %d after the inventory recovered, want 2", n)
	}
}

func TestPinExpirySeenInUseStampSparesWithinWindow(t *testing.T) {
	f := newPinFixture(t)
	start := f.now
	// The link is young when its lane uses it, so it is not yet expired.
	link := f.pin(t, "lane/result", 10)
	target := pinTarget(f, link)
	f.activity.index(target, "pid 99 cwd is "+target, nil)
	if n := f.p.expirePins(context.Background(), f.cfg, pinLogger()); n != 0 {
		t.Fatalf("removed %d", n)
	}
	st, err := loadNixPinUseState(f.state)
	if err != nil || st.Links[link].LastSeenInUse.IsZero() || !strings.Contains(st.Links[link].LastSeenBy, "pid 99") {
		t.Fatalf("no stamp: %+v %v", st, err)
	}

	// Ten days on the lane has stopped. The link is 20 days old, the stamp 10.
	f.activity = newBBActivity()
	f.now = start.Add(10 * 24 * time.Hour)
	if n := f.p.expirePins(context.Background(), f.cfg, pinLogger()); n != 0 || !pinExists(link) {
		t.Fatalf("a pin seen in use 10 days ago was removed (n=%d)", n)
	}
	rs := pinReceipts(t, f)
	if len(rs) != 1 || rs[0].Action != "spared" || !strings.Contains(rs[0].Reason, nixPinSpareSeenInUse) {
		t.Fatalf("receipts = %+v", rs)
	}

	// Fifteen days after the last use it expires.
	f.now = start.Add(15 * 24 * time.Hour)
	if n := f.p.expirePins(context.Background(), f.cfg, pinLogger()); n != 1 || pinExists(link) {
		t.Fatalf("a pin idle past the window survived (n=%d)", n)
	}
	st, _ = loadNixPinUseState(f.state)
	if _, ok := st.Links[link]; ok {
		t.Fatal("state keeps a removed link")
	}
}

func TestPinExpiryRetargetedLinkDropsOldStamp(t *testing.T) {
	f := newPinFixture(t)
	link := f.pin(t, "lane/out", 30)
	other := filepath.Join(f.store, "def-other")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	st := nixPinUseState{Links: map[string]nixPinUseEntry{
		link: {Target: other, LastSeenInUse: f.now.Add(-time.Hour), LastSeenBy: "pid 1 old"},
	}}
	if err := saveNixPinUseState(f.state, st); err != nil {
		t.Fatal(err)
	}
	if n := f.p.expirePins(context.Background(), f.cfg, pinLogger()); n != 1 {
		t.Fatalf("a stamp for another target spared the link (n=%d)", n)
	}
}

func TestPinExpiryUnreadableStateRemovesNothing(t *testing.T) {
	f := newPinFixture(t)
	link := f.pin(t, "x", 30)
	if err := os.WriteFile(f.state, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if n := f.p.expirePins(context.Background(), f.cfg, pinLogger()); n != 0 || !pinExists(link) {
		t.Fatalf("removed %d with a corrupt state file", n)
	}
	targets, _, warnings := f.p.planPinExpiry(context.Background(), f.cfg)
	if len(targets) != 0 || len(warnings) != 1 || !strings.Contains(warnings[0], "state file") {
		t.Fatalf("plan = %v %v", targets, warnings)
	}
	if f.scans != 0 {
		t.Fatalf("inventory ran %d times with an unreadable state", f.scans)
	}
}

func TestPinExpiryPlanShowsSparedAndWritesNothing(t *testing.T) {
	f := newPinFixture(t)
	used := f.pin(t, "u", 30)
	idle := f.pin(t, "i", 30)
	f.activity.index(pinTarget(f, used), "lsof lists "+pinTarget(f, used), nil)
	targets, md, _ := f.p.planPinExpiry(context.Background(), f.cfg)
	if md["pin_expiry_links"] != "1" || md["pin_expiry_spared"] != "1" || len(targets) != 2 {
		t.Fatalf("plan = %v %v", targets, md)
	}
	for _, tg := range targets {
		switch tg.Path {
		case idle:
			if tg.Action != "remove_pin_link" {
				t.Fatalf("idle target = %+v", tg)
			}
		case used:
			if tg.Action != "keep" || !tg.Protected || !tg.Active || !strings.Contains(tg.Reason, "in_use") {
				t.Fatalf("used target = %+v", tg)
			}
		default:
			t.Fatalf("unexpected target %+v", tg)
		}
	}
	if pinExists(f.receipt) || pinExists(f.state) {
		t.Fatal("planning wrote a receipt or the state file")
	}
}

func TestPinExpiryNoInventoryWithoutLinks(t *testing.T) {
	f := newPinFixture(t)
	if n := f.p.expirePins(context.Background(), f.cfg, pinLogger()); n != 0 || f.scans != 0 {
		t.Fatalf("n=%d scans=%d with no links", n, f.scans)
	}
}

func TestPinActivityOptionsWithinStoreAndRoots(t *testing.T) {
	f := newPinFixture(t)
	f.cfg.BazelBases.Lsof = "/fake/lsof"
	opts := nixPinActivityOptions(f.cfg, f.store, []string{f.root})
	if opts.Lsof != "/fake/lsof" || !opts.UseLsof || !opts.SkipSSHDSessions {
		t.Fatalf("opts = %+v", opts)
	}
	f.cfg.Nix.PinExpiryLsof = "/other/lsof"
	f.cfg.Nix.PinExpiryUninspectableOKComms = []string{"ok"}
	opts = nixPinActivityOptions(f.cfg, f.store, []string{f.root})
	if opts.Lsof != "/other/lsof" || len(opts.OKComms) != 1 || len(opts.OKUnits) != 0 {
		t.Fatalf("opts = %+v", opts)
	}
	in := func(p string) bool {
		for _, w := range opts.Within {
			if w == p {
				return true
			}
		}
		return false
	}
	if !in(f.store) || !in(f.root) {
		t.Fatalf("within = %v", opts.Within)
	}
}
