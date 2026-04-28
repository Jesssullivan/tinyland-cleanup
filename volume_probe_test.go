package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteVolumeProbeSummary(t *testing.T) {
	resultPath := filepath.Join(t.TempDir(), "probe.result")

	err := writeVolumeProbeSummary(resultPath, volumeProbeSummary{
		UID:      501,
		User:     "operator",
		PWD:      "/tmp/example",
		ListRC:   0,
		XattrRC:  0,
		TouchRC:  0,
		RemoveRC: volumeProbeSkippedRemove,
	})
	if err != nil {
		t.Fatalf("writeVolumeProbeSummary failed: %v", err)
	}

	contentBytes, err := os.ReadFile(resultPath)
	if err != nil {
		t.Fatalf("reading probe result failed: %v", err)
	}
	content := string(contentBytes)
	for _, want := range []string{
		"uid=501\n",
		"user=operator\n",
		"pwd=/tmp/example\n",
		"ls_rc=0\n",
		"xattr_rc=0\n",
		"touch_rc=0\n",
		"rm_rc=99\n",
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("probe summary missing %q:\n%s", want, content)
		}
	}
}

func TestRunVolumeProbeChildOperationFileOps(t *testing.T) {
	dir := t.TempDir()
	probeFile := filepath.Join(dir, ".probe-write-test")
	errPath := filepath.Join(dir, "probe.err")

	if rc := runVolumeProbeChildOperation("list", dir, probeFile, errPath); rc != 0 {
		t.Fatalf("list rc=%d, want 0", rc)
	}
	if rc := runVolumeProbeChildOperation("touch", dir, probeFile, errPath); rc != 0 {
		t.Fatalf("touch rc=%d, want 0", rc)
	}
	if _, err := os.Stat(probeFile); err != nil {
		t.Fatalf("expected probe file after touch: %v", err)
	}
	if rc := runVolumeProbeChildOperation("rm", dir, probeFile, errPath); rc != 0 {
		t.Fatalf("rm rc=%d, want 0", rc)
	}
	if _, err := os.Stat(probeFile); !os.IsNotExist(err) {
		t.Fatalf("probe file still exists after rm, err=%v", err)
	}
}

func TestRunVolumeProbeChildOperationUnknown(t *testing.T) {
	dir := t.TempDir()
	errPath := filepath.Join(dir, "probe.err")

	if rc := runVolumeProbeChildOperation("bogus", dir, filepath.Join(dir, "probe"), errPath); rc != 2 {
		t.Fatalf("unknown op rc=%d, want 2", rc)
	}
	content, err := os.ReadFile(errPath)
	if err != nil {
		t.Fatalf("expected error file: %v", err)
	}
	if !strings.Contains(string(content), "unknown probe operation: bogus") {
		t.Fatalf("unexpected error content: %s", content)
	}
}
