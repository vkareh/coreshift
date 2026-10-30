package image

import (
	"strings"
	"testing"
)

// TestBeyondLineWarningFiresBeyondLine checks that a release from a
// line newer than the verified one warns, naming both the release and
// the line.
func TestBeyondLineWarningFiresBeyondLine(t *testing.T) {
	warning := BeyondLineWarning("reg.example/ocp-release:5.0.0-x86_64")
	for _, want := range []string{"5.0.0", verifiedLine} {
		if !strings.Contains(warning, want) {
			t.Fatalf("warning = %q, want it to name %s", warning, want)
		}
	}
}

// TestBeyondLineWarningQuietWithinLine checks that releases within or
// below the verified line, and references carrying no version at all,
// stay quiet.
func TestBeyondLineWarningQuietWithinLine(t *testing.T) {
	for _, ref := range []string{
		"reg.example/ocp-release:4.22.21-x86_64",
		"reg.example/ocp-release:4.21.9-x86_64",
		"reg.example/ocp-release:4.22.21-aarch64",
		"reg.example/ocp-release@sha256:0123abcd",
	} {
		if warning := BeyondLineWarning(ref); warning != "" {
			t.Errorf("BeyondLineWarning(%q) = %q, want none", ref, warning)
		}
	}
}
