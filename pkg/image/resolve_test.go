package image

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
)

// TestResolveReleasePinnedSkipsRegistry checks that a pinned reference
// is returned unchanged without a single registry request.
func TestResolveReleasePinnedSkipsRegistry(t *testing.T) {
	// Any request to this server fails the test.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("pinned resolution made a registry request: %s %s", r.Method, r.URL)
	}))
	t.Cleanup(server.Close)
	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	// 127.0.0.1 would be reached over https; localhost keeps it http.
	repo := "localhost:" + parsed.Port() + "/repo"

	ref, err := (&Client{}).ResolveRelease(context.Background(), repo, "", "x86_64",
		"quay.io/openshift-release-dev/ocp-release:4.22.11-x86_64")
	if err != nil {
		t.Fatal(err)
	}
	if ref != "quay.io/openshift-release-dev/ocp-release:4.22.11-x86_64" {
		t.Fatalf("ref = %q, want the pinned reference unchanged", ref)
	}
}

// TestResolveReleasePinnedGolden resolves a pinned release image and
// compares the result against a checked-in golden file, so any change
// to the resolution output shows up as a reviewable fixture diff.
func TestResolveReleasePinnedGolden(t *testing.T) {
	// Any request to this server fails the test.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("pinned resolution made a registry request: %s %s", r.Method, r.URL)
	}))
	t.Cleanup(server.Close)
	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	// 127.0.0.1 would be reached over https; localhost keeps it http.
	repo := "localhost:" + parsed.Port() + "/repo"

	ref, err := (&Client{}).ResolveRelease(context.Background(), repo, "", "x86_64",
		"quay.io/openshift-release-dev/ocp-release:4.22.11-x86_64")
	if err != nil {
		t.Fatal(err)
	}
	golden, err := os.ReadFile("testdata/resolve-pinned.golden")
	if err != nil {
		t.Fatal(err)
	}
	if want := strings.TrimSpace(string(golden)); ref != want {
		t.Fatalf("ref = %q, want golden file %q", ref, want)
	}
}

// TestResolveReleaseDefaultPicksNewestInLine checks the unversioned
// default against the standard fixture: the newest release within the
// verified line for the machine's architecture.
func TestResolveReleaseDefaultPicksNewestInLine(t *testing.T) {
	repo := newTagRegistry(t, releaseRegistryTags, 100)

	ref, err := (&Client{}).ResolveRelease(context.Background(), repo, "", "aarch64", "")
	if err != nil {
		t.Fatal(err)
	}
	if want := repo + ":4.22.11-aarch64"; ref != want {
		t.Fatalf("ref = %q, want %q", ref, want)
	}
}

func TestResolveReleaseWithinMinorVersion(t *testing.T) {
	repo := newTagRegistry(t, []string{"4.22.11-x86_64", "4.23.0-x86_64"}, 100)

	ref, err := (&Client{}).ResolveRelease(context.Background(), repo, "4.22", "x86_64", "")
	if err != nil {
		t.Fatal(err)
	}
	if want := repo + ":4.22.11-x86_64"; ref != want {
		t.Fatalf("ref = %q, want %q", ref, want)
	}
}

// TestResolveReleaseDefaultStaysWithinVerifiedLine checks that an
// unversioned resolution floats to the newest release within the
// verified line, not the registry's absolute newest.
func TestResolveReleaseDefaultStaysWithinVerifiedLine(t *testing.T) {
	repo := newTagRegistry(t, []string{"4.22.21-x86_64", "5.0.0-x86_64"}, 100)

	ref, err := (&Client{}).ResolveRelease(context.Background(), repo, "", "x86_64", "")
	if err != nil {
		t.Fatal(err)
	}
	if want := repo + ":4.22.21-x86_64"; ref != want {
		t.Fatalf("ref = %q, want %q", ref, want)
	}
}

// TestResolveReleaseNewestIgnoresVerifiedLine checks that the explicit
// newest value resolves the registry's absolute newest release, so the
// drift alarm can keep probing the frontier.
func TestResolveReleaseNewestIgnoresVerifiedLine(t *testing.T) {
	repo := newTagRegistry(t, []string{"4.22.21-x86_64", "5.0.0-x86_64"}, 100)

	ref, err := (&Client{}).ResolveRelease(context.Background(), repo, "newest", "x86_64", "")
	if err != nil {
		t.Fatal(err)
	}
	if want := repo + ":5.0.0-x86_64"; ref != want {
		t.Fatalf("ref = %q, want %q", ref, want)
	}
}

// TestResolveReleaseSurfacesSelectionErrors checks that bad version
// constraints fail with the selection logic's own errors.
func TestResolveReleaseSurfacesSelectionErrors(t *testing.T) {
	repo := newTagRegistry(t, releaseRegistryTags, 100)

	_, err := (&Client{}).ResolveRelease(context.Background(), repo, "4.x", "x86_64", "")
	if err == nil || !strings.Contains(err.Error(), `invalid version "4.x"`) {
		t.Fatalf("err = %v, want the selection logic's invalid version error", err)
	}

	_, err = (&Client{}).ResolveRelease(context.Background(), repo, "4.99", "x86_64", "")
	if err == nil || !strings.Contains(err.Error(), "no 4.99 release found for x86_64") {
		t.Fatalf("err = %v, want the selection logic's no-match error", err)
	}
}
