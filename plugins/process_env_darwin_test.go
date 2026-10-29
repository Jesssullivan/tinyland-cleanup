//go:build darwin

package plugins

import (
	"encoding/binary"
	"os"
	"reflect"
	"testing"
)

func TestDecodeDarwinProcessEnv(t *testing.T) {
	raw := make([]byte, 4)
	binary.LittleEndian.PutUint32(raw, 2)
	raw = append(raw, []byte("/bin/bash\x00\x00\x00\x00bash\x00-l\x00HOME=/Users/x\x00TMPDIR=/private/tmp/nix-shell.q\x00\x00junk")...)
	env, err := decodeDarwinProcessEnv(raw)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"HOME=/Users/x", "TMPDIR=/private/tmp/nix-shell.q"}
	if !reflect.DeepEqual(env, want) {
		t.Fatalf("env = %q, want %q", env, want)
	}
	if _, err := decodeDarwinProcessEnv([]byte{1}); err == nil {
		t.Fatal("short buffer accepted")
	}
}

func TestDarwinProcessEnvReadsOwnProcess(t *testing.T) {
	env, err := darwinProcessEnv(os.Getppid())
	if err != nil {
		t.Skipf("parent environment unreadable here: %v", err)
	}
	if len(env) == 0 {
		t.Fatal("no environment decoded for the parent process")
	}
}
