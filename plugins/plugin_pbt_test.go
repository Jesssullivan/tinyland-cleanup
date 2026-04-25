// Package plugins provides cleanup plugin implementations.
package plugins

import (
	"runtime"
	"testing"

	"pgregory.net/rapid"
)

// TestCleanupLevelOrdering verifies that CleanupLevel ordering is total.
func TestCleanupLevelOrdering(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		l1 := CleanupLevel(rapid.IntRange(0, 4).Draw(t, "l1"))
		l2 := CleanupLevel(rapid.IntRange(0, 4).Draw(t, "l2"))

		// Totality: exactly one of l1 < l2, l1 == l2, or l1 > l2 must be true
		if !(l1 < l2 || l1 == l2 || l1 > l2) {
			t.Fatal("ordering not total")
		}

		// Antisymmetry: if l1 <= l2 and l2 <= l1, then l1 == l2
		if l1 <= l2 && l2 <= l1 && l1 != l2 {
			t.Fatal("antisymmetry violated")
		}
	})
}

// TestCleanupLevelTransitivity verifies transitivity of level ordering.
func TestCleanupLevelTransitivity(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		l1 := CleanupLevel(rapid.IntRange(0, 4).Draw(t, "l1"))
		l2 := CleanupLevel(rapid.IntRange(0, 4).Draw(t, "l2"))
		l3 := CleanupLevel(rapid.IntRange(0, 4).Draw(t, "l3"))

		// Transitivity: if l1 < l2 and l2 < l3, then l1 < l3
		if l1 < l2 && l2 < l3 && !(l1 < l3) {
			t.Fatalf("transitivity violated: %d < %d < %d but not %d < %d", l1, l2, l3, l1, l3)
		}
	})
}

// TestCleanupLevelBounds verifies level values are within expected bounds.
func TestCleanupLevelBounds(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		level := CleanupLevel(rapid.IntRange(0, 4).Draw(t, "level"))

		// All valid levels should have valid String() output
		str := level.String()
		validStrings := map[string]bool{
			"none":       true,
			"warning":    true,
			"moderate":   true,
			"aggressive": true,
			"critical":   true,
		}

		if !validStrings[str] {
			t.Fatalf("invalid level string: %s", str)
		}
	})
}

// TestCleanupResultBytesFreedNonNegative verifies BytesFreed is never negative.
func TestCleanupResultBytesFreedNonNegative(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		// Generate random output strings that might be parsed
		output := rapid.String().Draw(t, "output")

		// Create a podman plugin to test parsing
		p := NewPodmanPlugin()
		bytes := p.parseReclaimedSpace(output)

		if bytes < 0 {
			t.Fatalf("negative bytes freed: %d from output: %q", bytes, output)
		}
	})
}

// TestParseFstrimOutputNonNegative verifies fstrim output parsing returns non-negative.
func TestParseFstrimOutputNonNegative(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		output := rapid.String().Draw(t, "output")
		bytes := parseFstrimOutput(output)

		if bytes < 0 {
			t.Fatalf("negative bytes trimmed: %d from output: %q", bytes, output)
		}
	})
}

// TestParseReclaimedSpaceKnownFormats tests known output formats parse correctly.
func TestParseReclaimedSpaceKnownFormats(t *testing.T) {
	testCases := []struct {
		name     string
		output   string
		expected int64
	}{
		{
			name:     "docker_style_mb",
			output:   "Total reclaimed space: 100.5MB",
			expected: int64(100.5 * 1024 * 1024),
		},
		{
			name:     "docker_style_gb",
			output:   "Total reclaimed space: 1.5GB",
			expected: int64(1.5 * 1024 * 1024 * 1024),
		},
		{
			name:     "docker_style_kb",
			output:   "Total reclaimed space: 500KB",
			expected: int64(500 * 1024),
		},
		{
			name:     "no_match",
			output:   "No space reclaimed",
			expected: 0,
		},
		{
			name:     "empty",
			output:   "",
			expected: 0,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			p := NewPodmanPlugin()
			result := p.parseReclaimedSpace(tc.output)
			if result != tc.expected {
				t.Errorf("expected %d, got %d for output %q", tc.expected, result, tc.output)
			}
		})
	}
}

// TestFstrimOutputKnownFormats tests known fstrim output formats.
func TestFstrimOutputKnownFormats(t *testing.T) {
	testCases := []struct {
		name     string
		output   string
		expected int64
	}{
		{
			name:     "single_mount",
			output:   "/: 1610612736 bytes (1.5 GiB) trimmed",
			expected: 0, // This format doesn't match our regex
		},
		{
			name:     "parentheses_format",
			output:   "/: (1610612736 bytes) trimmed",
			expected: 1610612736,
		},
		{
			name:     "multiple_mounts",
			output:   "/: (1000000 bytes) trimmed\n/home: (2000000 bytes) trimmed",
			expected: 3000000,
		},
		{
			name:     "no_trim",
			output:   "Nothing to trim",
			expected: 0,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			result := parseFstrimOutput(tc.output)
			if result != tc.expected {
				t.Errorf("expected %d, got %d for output %q", tc.expected, result, tc.output)
			}
		})
	}
}

func TestPodmanFstrimHostAccounting(t *testing.T) {
	p := &PodmanPlugin{
		environment: &PodmanEnvironment{
			VMProvider: "applehv",
		},
	}

	if runtime.GOOS == "darwin" {
		if p.fstrimReclaimsHostSpace() {
			t.Fatal("applehv fstrim should not be counted as host-side reclaimed space on Darwin")
		}
		return
	}

	if !p.fstrimReclaimsHostSpace() {
		t.Fatal("non-Darwin platforms should keep fstrim host accounting enabled")
	}
}

func TestPodmanFstrimHostAccountingNilEnvironment(t *testing.T) {
	p := &PodmanPlugin{}
	if !p.fstrimReclaimsHostSpace() {
		t.Fatal("nil Podman environment should default to enabled accounting")
	}
}
