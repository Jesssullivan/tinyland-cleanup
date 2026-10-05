package plugins

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Jesssullivan/tinyland-cleanup/config"
)

func releasedRootFixture(t *testing.T) (string, string, config.DevArtifactsConfig) {
	t.Helper()
	scan := t.TempDir()
	root := filepath.Join(scan, "owned-candidate")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	receipt, _ := json.Marshal(temporaryCustodyReceipt{Path: canonicalTempArtifactPath(root), OwnerUID: os.Geteuid(), State: "released", Receipt: "TIN-5128 owned fixture release"})
	if err := os.WriteFile(filepath.Join(root, disposableTempReceipt), receipt, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "artifact"), []byte("rebuildable fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	ageFixture(t, root)
	cfg := config.DefaultConfig().DevArtifacts
	cfg.TempRootCleanup = true
	cfg.TempScanPaths = []string{scan}
	cfg.TempArtifactStaleAfter = "7d"
	return scan, root, cfg
}
func ageFixture(t *testing.T, root string) {
	t.Helper()
	old := time.Now().Add(-8 * 24 * time.Hour)
	if err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		return os.Chtimes(path, old, old)
	}); err != nil {
		t.Fatal(err)
	}
}
func custodyTestPlugin() *DevArtifactsPlugin {
	return &DevArtifactsPlugin{tempActivity: func(context.Context, []string, string) (map[string]string, error) { return map[string]string{}, nil }}
}
func TestConfiguredTempActivityPaths(t *testing.T) {
	scan := t.TempDir()
	root := filepath.Join(scan, "job")
	output := "bash bash -c tool --root=" + filepath.Join(root, "output") + "\n"
	roots := tempArtifactRootsFromProcessOutput(output, []string{scan}, "")
	if temporaryRootActivityReason(roots, root) == "" {
		t.Fatal("configured root reference missed")
	}
	files := "p123\x00\nn" + filepath.Join(root, "has spaces", "output") + "\x00\n"
	if temporaryRootActivityReason(tempRootsFromOpenFiles(files, []string{scan}, ""), root) == "" {
		t.Fatal("open file in configured root missed")
	}
	if temporaryRootActivityReason(tempRootsFromOpenFiles("p123\x00n"+scan+"-neighbor/job\x00", []string{scan}, ""), root) != "" {
		t.Fatal("prefix collision protected unrelated root")
	}
}
func TestDisposableTempRootSafeguards(t *testing.T) {
	cases := []string{"released", "missing receipt", "unreleased", "foreign receipt", "missing uid", "git clean", "git ignored", "nested git", "recent child", "protected child", "unknown activity", "active cwd", "replaced identity", "disabled"}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			scan, root, cfg := releasedRootFixture(t)
			p := custodyTestPlugin()
			oldInfo, _ := os.Lstat(root)
			switch name {
			case "missing receipt":
				os.Remove(filepath.Join(root, disposableTempReceipt))
			case "unreleased":
				os.WriteFile(filepath.Join(root, disposableTempReceipt), []byte(fmt.Sprintf(`{"path":%q,"owner_uid":%d,"state":"active","receipt":"fixture"}`, canonicalTempArtifactPath(root), os.Geteuid())), 0600)
			case "missing uid":
				os.WriteFile(filepath.Join(root, disposableTempReceipt), []byte(fmt.Sprintf(`{"path":%q,"state":"released","receipt":"fixture"}`, canonicalTempArtifactPath(root))), 0600)
			case "foreign receipt":
				os.WriteFile(filepath.Join(root, disposableTempReceipt), []byte(fmt.Sprintf(`{"path":%q,"owner_uid":%d,"state":"released","receipt":"fixture"}`, canonicalTempArtifactPath(root), os.Geteuid()+1)), 0600)
			case "git clean", "git ignored":
				os.Mkdir(filepath.Join(root, ".git"), 0700)
				ageFixture(t, root)
			case "nested git":
				os.MkdirAll(filepath.Join(root, "nested", ".git"), 0700)
				ageFixture(t, root)
			case "recent child":
				os.WriteFile(filepath.Join(root, "artifact"), []byte("new activity"), 0600)
				old := time.Now().Add(-8 * 24 * time.Hour)
				os.Chtimes(root, old, old)
			case "protected child":
				cfg.ProtectPaths = []string{filepath.Join(root, "artifact")}
			case "unknown activity":
				p.tempActivity = func(context.Context, []string, string) (map[string]string, error) {
					return nil, fmt.Errorf("inventory denied")
				}
			case "active cwd":
				p.tempActivity = func(context.Context, []string, string) (map[string]string, error) {
					return map[string]string{canonicalTempArtifactPath(root): "cwd held"}, nil
				}
			case "replaced identity":
				os.Rename(root, root+"-preserved")
				os.Mkdir(root, 0700)
				if p.recheckTemporaryRoot(context.Background(), root, oldInfo, 7*24*time.Hour, cfg) == "" {
					t.Fatal("replacement identity allowed")
				}
				return
			case "disabled":
				cfg.TempRootCleanup = false
			}
			var targets []CleanupTarget
			p.planDisposableTemporaryRoots(context.Background(), scan, "", LevelCritical, cfg, nil, &targets, newDevArtifactScanBudget(cfg))
			if name == "released" {
				if len(targets) != 1 || targets[0].Action != "delete" {
					t.Fatalf("released fixture not eligible: %#v", targets)
				}
			} else {
				for _, target := range targets {
					if target.Action == "delete" {
						t.Fatalf("protected fixture eligible: %#v", target)
					}
				}
			}
		})
	}
}
func TestDisposableTempRootRechecksAfterPlan(t *testing.T) {
	scan, root, cfg := releasedRootFixture(t)
	p := custodyTestPlugin()
	calls := 0
	p.tempActivity = func(context.Context, []string, string) (map[string]string, error) {
		calls++
		if calls > 1 {
			return map[string]string{canonicalTempArtifactPath(root): "new open file"}, nil
		}
		return map[string]string{}, nil
	}
	p.removeAll = func(string) error { t.Fatal("deleted root after it became active"); return nil }
	freed := p.cleanDisposableTemporaryRoots(context.Background(), scan, "", LevelCritical, cfg, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), newDevArtifactScanBudget(cfg))
	if freed != 0 || !pathExists(root) || calls < 2 {
		t.Fatalf("fresh protection failed: freed %d calls %d", freed, calls)
	}
}
func TestDisposableTempRootDeletesOwnedIdleFixture(t *testing.T) {
	scan, root, cfg := releasedRootFixture(t)
	p := custodyTestPlugin()
	freed := p.cleanDisposableTemporaryRoots(context.Background(), scan, "", LevelCritical, cfg, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), newDevArtifactScanBudget(cfg))
	if pathExists(root) || freed <= 0 {
		t.Fatalf("owned fixture not reaped: freed %d", freed)
	}
}

func TestTemporaryProcActivityCustody(t *testing.T) {
	proc := t.TempDir()
	scan := t.TempDir()
	root := filepath.Join(scan, "job")
	process := filepath.Join(proc, "123")
	if err := os.MkdirAll(filepath.Join(process, "fd"), 0700); err != nil {
		t.Fatal(err)
	}
	status := fmt.Sprintf("Uid: %d %d %d %d\n", os.Geteuid(), os.Geteuid(), os.Geteuid(), os.Geteuid())
	if err := os.WriteFile(filepath.Join(process, "status"), []byte(status), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(root, filepath.Join(process, "cwd")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "has spaces"), filepath.Join(process, "fd", "7")); err != nil {
		t.Fatal(err)
	}
	roots, err := temporaryRootActivityFromProc(context.Background(), proc, []string{scan}, "")
	if err != nil || temporaryRootActivityReason(roots, root) == "" {
		t.Fatalf("own cwd/fd reference missed: %#v %v", roots, err)
	}
	os.WriteFile(filepath.Join(process, "status"), []byte("ownership unreadable"), 0600)
	if _, err := temporaryRootActivityFromProc(context.Background(), proc, []string{scan}, ""); err == nil {
		t.Fatal("unknown process ownership accepted")
	}
	os.WriteFile(filepath.Join(process, "status"), []byte(fmt.Sprintf("Uid: %d %d %d %d\n", os.Geteuid()+1, os.Geteuid()+1, os.Geteuid()+1, os.Geteuid()+1)), 0600)
	roots, err = temporaryRootActivityFromProc(context.Background(), proc, []string{scan}, "")
	if err != nil || len(roots) != 0 {
		t.Fatalf("foreign process treated as own: %#v %v", roots, err)
	}
}
