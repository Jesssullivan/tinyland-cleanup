//go:build linux

package plugins

import (
	"errors"
	"os"
	"reflect"
	"testing"
)

func TestDecodeNixLinuxProcessArgvPreservesBoundaries(t *testing.T) {
	raw := []byte("nix\x00build\x00\x00\"literal quotes\"\x00argument with spaces\x00\x00")
	want := []string{"nix", "build", "", `"literal quotes"`, "argument with spaces", ""}

	if got := decodeNixLinuxProcessArgv(raw); !reflect.DeepEqual(got, want) {
		t.Fatalf("decodeNixLinuxProcessArgv() = %#v, want %#v", got, want)
	}
}

func TestNixProcessArgvLinux(t *testing.T) {
	got, err := nixProcessArgv(os.Getpid())
	if err != nil {
		t.Fatalf("nixProcessArgv returned error: %v", err)
	}
	if !reflect.DeepEqual(got, os.Args) {
		t.Fatalf("nixProcessArgv() = %#v, want %#v", got, os.Args)
	}
}

func TestNixProcessArgvLinuxMapsMissingProcess(t *testing.T) {
	_, err := nixProcessArgv(int(^uint32(0) >> 1))
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("nixProcessArgv error = %v, want os.ErrNotExist", err)
	}
}
