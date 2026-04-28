package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"time"
)

const volumeProbeSkippedRemove = 99

type volumeProbeSummary struct {
	UID      int
	User     string
	PWD      string
	ListRC   int
	XattrRC  int
	TouchRC  int
	RemoveRC int
}

func runVolumeAccessProbe(volumePath, resultPath, probeName string, timeoutSeconds int) error {
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("volume probe mode only supports macOS")
	}
	if volumePath == "" {
		return fmt.Errorf("volume probe path is required")
	}
	if resultPath == "" {
		return fmt.Errorf("volume probe result path is required")
	}
	if probeName == "" {
		return fmt.Errorf("volume probe name is required")
	}
	if timeoutSeconds <= 0 {
		return fmt.Errorf("volume probe timeout must be positive")
	}

	probeFile := filepath.Join(volumePath, "."+probeName+"-write-test")
	listErr := resultPath + ".ls.err"
	xattrErr := resultPath + ".xattr.err"
	touchErr := resultPath + ".touch.err"
	removeErr := resultPath + ".rm.err"

	_ = os.Remove(listErr)
	_ = os.Remove(xattrErr)
	_ = os.Remove(touchErr)
	_ = os.Remove(removeErr)

	listRC := runVolumeProbeOperationWithTimeout("list", volumePath, probeFile, listErr, timeoutSeconds)
	xattrRC := runVolumeProbeOperationWithTimeout("xattr", volumePath, probeFile, xattrErr, timeoutSeconds)
	touchRC := runVolumeProbeOperationWithTimeout("touch", volumePath, probeFile, touchErr, timeoutSeconds)
	removeRC := volumeProbeSkippedRemove
	if touchRC == 0 {
		removeRC = runVolumeProbeOperationWithTimeout("rm", volumePath, probeFile, removeErr, timeoutSeconds)
	}

	currentUser := "unknown"
	if userInfo, err := user.Current(); err == nil && userInfo.Username != "" {
		currentUser = userInfo.Username
	} else if envUser := os.Getenv("USER"); envUser != "" {
		currentUser = envUser
	}

	currentWD := "unknown"
	if wd, err := os.Getwd(); err == nil {
		currentWD = wd
	}

	return writeVolumeProbeSummary(resultPath, volumeProbeSummary{
		UID:      os.Getuid(),
		User:     currentUser,
		PWD:      currentWD,
		ListRC:   listRC,
		XattrRC:  xattrRC,
		TouchRC:  touchRC,
		RemoveRC: removeRC,
	})
}

func runVolumeProbeOperationWithTimeout(op, volumePath, probeFile, errPath string, timeoutSeconds int) int {
	executablePath, err := os.Executable()
	if err != nil {
		writeVolumeProbeError(errPath, "os.Executable failed: %v", err)
		return 1
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeoutSeconds)*time.Second)
	defer cancel()

	cmd := exec.CommandContext(
		ctx,
		executablePath,
		"-probe-volume-op", op,
		"-probe-path", volumePath,
		"-probe-file", probeFile,
		"-probe-error-path", errPath,
	)
	cmd.Stdout = nil
	cmd.Stderr = nil

	err = cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		writeVolumeProbeError(errPath, "timed out after %d seconds", timeoutSeconds)
		return 124
	}
	if err == nil {
		return 0
	}

	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}

	writeVolumeProbeError(errPath, "exec failed: %v", err)
	return 1
}

func runVolumeProbeChildOperation(op, volumePath, probeFile, errPath string) int {
	switch op {
	case "list":
		handle, err := os.Open(volumePath)
		if err != nil {
			writeVolumeProbeError(errPath, "%v", err)
			return 1
		}
		if err := handle.Close(); err != nil {
			writeVolumeProbeError(errPath, "%v", err)
			return 1
		}
		return 0
	case "xattr":
		output, err := exec.Command("/usr/bin/xattr", "-l", volumePath).CombinedOutput()
		if err != nil {
			if len(output) > 0 {
				writeVolumeProbeError(errPath, "%s", output)
			} else {
				writeVolumeProbeError(errPath, "%v", err)
			}
			return 1
		}
		return 0
	case "touch":
		handle, err := os.OpenFile(probeFile, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
		if err != nil {
			writeVolumeProbeError(errPath, "%v", err)
			return 1
		}
		if err := handle.Close(); err != nil {
			writeVolumeProbeError(errPath, "%v", err)
			return 1
		}
		return 0
	case "rm":
		if err := os.Remove(probeFile); err != nil {
			writeVolumeProbeError(errPath, "%v", err)
			return 1
		}
		return 0
	default:
		writeVolumeProbeError(errPath, "unknown probe operation: %s", op)
		return 2
	}
}

func writeVolumeProbeSummary(resultPath string, summary volumeProbeSummary) error {
	content := "" +
		"uid=" + strconv.Itoa(summary.UID) + "\n" +
		"user=" + summary.User + "\n" +
		"pwd=" + summary.PWD + "\n" +
		"ls_rc=" + strconv.Itoa(summary.ListRC) + "\n" +
		"xattr_rc=" + strconv.Itoa(summary.XattrRC) + "\n" +
		"touch_rc=" + strconv.Itoa(summary.TouchRC) + "\n" +
		"rm_rc=" + strconv.Itoa(summary.RemoveRC) + "\n"

	return os.WriteFile(resultPath, []byte(content), 0o644)
}

func writeVolumeProbeError(errPath, format string, args ...any) {
	if errPath == "" {
		return
	}
	message := fmt.Sprintf(format, args...)
	_ = os.WriteFile(errPath, []byte(message+"\n"), 0o644)
}
