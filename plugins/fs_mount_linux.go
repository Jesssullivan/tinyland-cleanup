//go:build linux

package plugins

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func platformMountID(path string) (string, error) {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return "", err
	}
	defer f.Close()
	clean := filepath.Clean(path)
	bestPoint, bestID := "", ""
	s := bufio.NewScanner(f)
	for s.Scan() {
		fields := strings.Fields(s.Text())
		if len(fields) < 6 {
			return "", fmt.Errorf("malformed /proc/self/mountinfo entry")
		}
		point, err := unescapeMountInfo(fields[4])
		if err != nil {
			return "", err
		}
		if pathWithin(point, clean) && len(point) > len(bestPoint) {
			bestPoint, bestID = point, fields[0]
		}
	}
	if err := s.Err(); err != nil {
		return "", err
	}
	if bestID == "" {
		return "", fmt.Errorf("no mount identity for %s", path)
	}
	return bestID, nil
}

func unescapeMountInfo(value string) (string, error) {
	for _, replacement := range []struct{ old, new string }{{`\040`, " "}, {`\011`, "\t"}, {`\012`, "\n"}, {`\134`, `\`}} {
		value = strings.ReplaceAll(value, replacement.old, replacement.new)
	}
	if strings.Contains(value, `\`) {
		for i := 0; i < len(value); i++ {
			if value[i] == '\\' {
				if i+3 >= len(value) {
					return "", fmt.Errorf("invalid mountinfo escape")
				}
				if _, err := strconv.ParseUint(value[i+1:i+4], 8, 8); err != nil {
					return "", err
				}
				i += 3
			}
		}
	}
	return value, nil
}
