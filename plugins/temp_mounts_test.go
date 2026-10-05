package plugins

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
)

func TestTemporaryRootMountCustody(t *testing.T) {
	cases := []string{"candidate root mount", "same-device nested bind", "inventory unavailable", "empty inventory", "invalid inventory", "prefix neighbor"}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			scan, root, cfg := releasedRootFixture(t)
			p := custodyTestPlugin()
			p.tempMounts = func(context.Context) ([]string, error) {
				switch name {
				case "candidate root mount":
					return []string{"/", root}, nil
				case "same-device nested bind":
					return []string{"/", filepath.Join(root, "nested")}, nil
				case "inventory unavailable":
					return nil, fmt.Errorf("inventory denied")
				case "empty inventory":
					return nil, nil
				case "invalid inventory":
					return []string{"relative"}, nil
				default:
					return []string{"/", root + "-neighbor"}, nil
				}
			}
			removed := false
			p.removeAll = func(string) error { removed = true; return nil }
			var targets []CleanupTarget
			p.planDisposableTemporaryRoots(context.Background(), scan, "", LevelCritical, cfg, nil, &targets, newDevArtifactScanBudget(cfg))
			if len(targets) != 1 {
				t.Fatalf("expected explicit target, got %#v", targets)
			}
			p.cleanDisposableTemporaryRoots(context.Background(), scan, "", LevelCritical, cfg, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), newDevArtifactScanBudget(cfg))
			if name == "prefix neighbor" {
				if targets[0].Action != "delete" || !removed {
					t.Fatalf("unrelated neighboring mount protected target: %#v", targets)
				}
			} else if targets[0].Action == "delete" || removed {
				t.Fatalf("mount or unknown inventory reached removal: %#v", targets)
			}
		})
	}
}

func TestTemporaryMountArrivingAfterPlanProtectsRemoval(t *testing.T) {
	scan, root, cfg := releasedRootFixture(t)
	p := custodyTestPlugin()
	calls := 0
	p.tempMounts = func(context.Context) ([]string, error) {
		calls++
		if calls > 2 {
			return []string{"/", root}, nil
		}
		return []string{"/"}, nil
	}
	p.removeAll = func(string) error { t.Fatal("new mount reached RemoveAll"); return nil }
	freed := p.cleanDisposableTemporaryRoots(context.Background(), scan, "", LevelCritical, cfg, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), newDevArtifactScanBudget(cfg))
	if freed != 0 || calls < 3 {
		t.Fatalf("fresh mount check missing: freed %d calls %d", freed, calls)
	}
}

func TestTemporaryMountInfoEscapesAndBindIdentity(t *testing.T) {
	data := `26 22 0:19 / / rw - ext4 /dev/test rw
27 26 0:19 /owned /tmp/owned\040job rw - ext4 /dev/test rw
28 27 0:19 /nested /tmp/owned\040job/nested\134name rw - ext4 /dev/test rw
`
	mounts, err := parseTemporaryMountInfo(strings.NewReader(data))
	if err != nil || len(mounts) != 3 || mounts[1] != "/tmp/owned job" || mounts[2] != "/tmp/owned job/nested\\name" {
		t.Fatalf("mountinfo lost identity/escapes: %#v %v", mounts, err)
	}
	for _, invalid := range []string{"", "malformed\n", `26 22 0:19 / /tmp/bad\099name rw - ext4 /dev/test rw`, strings.Repeat("26 22 0:19 / / rw - ext4 /dev/test rw\n", maxTemporaryMounts+1)} {
		if _, err := parseTemporaryMountInfo(strings.NewReader(invalid)); err == nil {
			t.Fatal("incomplete/malformed/bounded inventory accepted")
		}
	}
}
