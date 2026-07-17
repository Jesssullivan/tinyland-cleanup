//go:build linux

package plugins

import (
	"errors"
	"os"
	"os/exec"
	"reflect"
	"testing"
	"time"
)

func TestDecodeNixLinuxProcessArgvPreservesBoundaries(t *testing.T) {
	raw := []byte("nix\x00build\x00\x00\"literal quotes\"\x00argument with spaces\x00\x00")
	want := []string{"nix", "build", "", `"literal quotes"`, "argument with spaces", ""}

	if got := decodeNixLinuxProcessArgv(raw); !reflect.DeepEqual(got, want) {
		t.Fatalf("decodeNixLinuxProcessArgv() = %#v, want %#v", got, want)
	}
}

func TestNixProcessArgvLinux(t *testing.T) {
	const helperEnvironment = "TINYLAND_NIX_ARGV_LINUX_HELPER"
	if os.Getenv(helperEnvironment) == "1" {
		for {
			time.Sleep(time.Hour)
		}
	}

	arguments := []string{
		"-test.run=^TestNixProcessArgvLinux$",
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

func TestNixProcessArgvLinuxMapsMissingProcess(t *testing.T) {
	_, err := nixProcessArgv(int(^uint32(0) >> 1))
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("nixProcessArgv error = %v, want os.ErrNotExist", err)
	}
}
