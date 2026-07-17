//go:build darwin

package plugins

import (
	"encoding/binary"
	"errors"
	"os"
	"os/exec"
	"reflect"
	"testing"
	"time"
)

func TestDecodeNixDarwinProcessArgvPreservesBoundaries(t *testing.T) {
	want := []string{
		"/nix/store/example/bin/nix",
		"build",
		"",
		`"literal double quotes"`,
		"'literal single quotes'",
		"argument with spaces",
		"",
	}
	raw := make([]byte, 4)
	binary.LittleEndian.PutUint32(raw, uint32(len(want)))
	raw = append(raw, "/nix/store/example/bin/nix"...)
	raw = append(raw, 0, 0, 0, 0)
	for _, argument := range want {
		raw = append(raw, argument...)
		raw = append(raw, 0)
	}
	raw = append(raw, "HOME=/Users/test"...)
	raw = append(raw, 0)

	got, err := decodeNixDarwinProcessArgv(raw)
	if err != nil {
		t.Fatalf("decodeNixDarwinProcessArgv returned error: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("decodeNixDarwinProcessArgv() = %#v, want %#v", got, want)
	}
}

func TestDecodeNixDarwinProcessArgvRejectsMalformedData(t *testing.T) {
	tests := []struct {
		name string
		raw  []byte
	}{
		{name: "missing argc", raw: []byte{1, 0, 0}},
		{name: "negative argc", raw: []byte{0xff, 0xff, 0xff, 0xff}},
		{name: "unterminated executable", raw: []byte{0, 0, 0, 0, 'n', 'i', 'x'}},
		{name: "truncated argv", raw: []byte{2, 0, 0, 0, 'n', 'i', 'x', 0, 0, 'n', 'i', 'x', 0}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := decodeNixDarwinProcessArgv(test.raw); err == nil {
				t.Fatal("decodeNixDarwinProcessArgv unexpectedly succeeded")
			}
		})
	}
}

func TestNixProcessArgvDarwin(t *testing.T) {
	const helperEnvironment = "TINYLAND_NIX_ARGV_DARWIN_HELPER"
	if os.Getenv(helperEnvironment) == "1" {
		for {
			time.Sleep(time.Hour)
		}
	}

	arguments := []string{
		"-test.run=^TestNixProcessArgvDarwin$",
		"--",
		"",
		`"literal quotes"`,
		"argument with spaces",
		"",
	}
	command := exec.Command(os.Args[0], arguments...)
	command.Env = append(os.Environ(), helperEnvironment+"=1")
	if err := command.Start(); err != nil {
		t.Fatalf("start helper process: %v", err)
	}
	t.Cleanup(func() {
		_ = command.Process.Kill()
		_ = command.Wait()
	})

	got, err := nixProcessArgv(command.Process.Pid)
	if err != nil {
		t.Fatalf("nixProcessArgv returned error: %v", err)
	}
	if !reflect.DeepEqual(got, command.Args) {
		t.Fatalf("nixProcessArgv() = %#v, want %#v", got, command.Args)
	}
}

func TestNixProcessArgvDarwinMapsMissingProcess(t *testing.T) {
	_, err := nixProcessArgv(int(^uint32(0) >> 1))
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("nixProcessArgv error = %v, want os.ErrNotExist", err)
	}
}
