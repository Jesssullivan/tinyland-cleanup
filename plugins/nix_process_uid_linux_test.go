//go:build linux

package plugins

import (
	"errors"
	"os"
	"testing"
)

func TestNixProcessUIDLinuxReadsOwnStatus(t *testing.T) {
	got, err := nixProcessUID(os.Getpid())
	if err != nil {
		t.Fatalf("nixProcessUID: %v", err)
	}
	if want := uint32(os.Geteuid()); got != want {
		t.Fatalf("nixProcessUID = %d, want %d", got, want)
	}
}

func TestNixProcessUIDLinuxMapsMissingProcess(t *testing.T) {
	_, err := nixProcessUID(int(^uint32(0) >> 1))
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("nixProcessUID error = %v, want os.ErrNotExist", err)
	}
}
