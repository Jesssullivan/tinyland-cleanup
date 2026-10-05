package plugins

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"
)

const maxTemporaryMounts = 16384
const maxTemporaryMountBytes = 8 * 1024 * 1024

type temporaryMountInventory func(context.Context) ([]string, error)

func temporaryMountProtectReason(ctx context.Context, root string, inventory temporaryMountInventory) string {
	if inventory == nil {
		inventory = nativeTemporaryMounts
	}
	mounts, err := inventory(ctx)
	if err != nil || len(mounts) == 0 || len(mounts) > maxTemporaryMounts {
		return "authoritative mount inventory unavailable, incomplete or over bound"
	}
	root = canonicalTempArtifactPath(root)
	for _, mount := range mounts {
		if !filepath.IsAbs(mount) || strings.IndexByte(mount, 0) >= 0 {
			return "authoritative mount inventory contains an invalid path"
		}
		mount = canonicalTempArtifactPath(mount)
		if pathWithin(mount, root) {
			return "temporary root is or contains a mounted filesystem"
		}
	}
	return ""
}

// Linux mountinfo lists bind mounts even when they have the same st.Dev as
// their parents. Reject malformed/truncated records rather than infer no mounts.
func parseTemporaryMountInfo(reader io.Reader) ([]string, error) {
	scanner := bufio.NewScanner(io.LimitReader(reader, maxTemporaryMountBytes+1))
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	bytesRead := 0
	var mounts []string
	for scanner.Scan() {
		line := scanner.Text()
		bytesRead += len(line) + 1
		if bytesRead > maxTemporaryMountBytes {
			return nil, fmt.Errorf("mount inventory byte bound exceeded")
		}
		fields := strings.Fields(line)
		separator := -1
		for i, field := range fields {
			if field == "-" {
				separator = i
				break
			}
		}
		if len(fields) < 10 || separator < 6 || len(fields) < separator+4 {
			return nil, fmt.Errorf("malformed mountinfo record")
		}
		if _, err := strconv.ParseUint(fields[0], 10, 64); err != nil {
			return nil, fmt.Errorf("invalid mount identity")
		}
		if _, err := strconv.ParseUint(fields[1], 10, 64); err != nil {
			return nil, fmt.Errorf("invalid parent mount identity")
		}
		path, err := decodeTemporaryMountPath(fields[4])
		if err != nil || !filepath.IsAbs(path) {
			return nil, fmt.Errorf("invalid mountpoint path")
		}
		mounts = append(mounts, path)
		if len(mounts) > maxTemporaryMounts {
			return nil, fmt.Errorf("mount inventory count bound exceeded")
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if len(mounts) == 0 {
		return nil, fmt.Errorf("empty mount inventory")
	}
	return mounts, nil
}

func decodeTemporaryMountPath(encoded string) (string, error) {
	var path strings.Builder
	for i := 0; i < len(encoded); i++ {
		if encoded[i] != '\\' {
			path.WriteByte(encoded[i])
			continue
		}
		if i+3 >= len(encoded) {
			return "", fmt.Errorf("truncated mountpoint escape")
		}
		switch encoded[i+1 : i+4] {
		case "040":
			path.WriteByte(' ')
		case "011":
			path.WriteByte('\t')
		case "012":
			path.WriteByte('\n')
		case "134":
			path.WriteByte('\\')
		default:
			return "", fmt.Errorf("unknown mountpoint escape")
		}
		i += 3
	}
	return path.String(), nil
}
