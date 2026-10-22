package image

import (
	"strings"
	"testing"
)

func TestSelectReleasePicksHighestVersion(t *testing.T) {
	release, err := SelectRelease(ReleaseVersions(releaseRegistryTags, "x86_64"), "x86_64", "")
	if err != nil {
		t.Fatal(err)
	}
	if release.Version != (Version{Major: 4, Minor: 22, Patch: 11}) {
		t.Fatalf("version = %v, want 4.22.11", release.Version)
	}
	if release.Tag() != "4.22.11-x86_64" {
		t.Fatalf("tag = %s, want 4.22.11-x86_64", release.Tag())
	}
}

func TestSelectReleaseMinorConstraintPicksHighestPatch(t *testing.T) {
	versions := []Version{
		{Major: 4, Minor: 22, Patch: 8},
		{Major: 4, Minor: 23, Patch: 0},
		{Major: 4, Minor: 22, Patch: 11},
	}
	release, err := SelectRelease(versions, "x86_64", "4.22")
	if err != nil {
		t.Fatal(err)
	}
	if release.Version != (Version{Major: 4, Minor: 22, Patch: 11}) {
		t.Fatalf("version = %v, want 4.22.11", release.Version)
	}
}

func TestSelectReleaseExactConstraint(t *testing.T) {
	versions := ReleaseVersions(releaseRegistryTags, "x86_64")

	release, err := SelectRelease(versions, "x86_64", "4.22.9")
	if err != nil {
		t.Fatal(err)
	}
	if release.Version != (Version{Major: 4, Minor: 22, Patch: 9}) {
		t.Fatalf("version = %v, want 4.22.9, not the newest", release.Version)
	}
}

func TestSelectReleaseEmptyList(t *testing.T) {
	_, err := SelectRelease(nil, "x86_64", "")
	if err == nil || !strings.Contains(err.Error(), "no stable release tags found for x86_64") {
		t.Fatalf("err = %v, want no stable release tags found for x86_64", err)
	}
}

func TestSelectReleaseConstraintWithoutMatch(t *testing.T) {
	versions := ReleaseVersions(releaseRegistryTags, "x86_64")

	_, err := SelectRelease(versions, "x86_64", "4.99")
	if err == nil || !strings.Contains(err.Error(), "no 4.99 release found for x86_64") {
		t.Fatalf("err = %v, want no 4.99 release found for x86_64", err)
	}
	if !strings.Contains(err.Error(), "available: 4.22.8, 4.22.9, 4.22.11") {
		t.Fatalf("err = %v, want available versions listed", err)
	}

	if _, err := SelectRelease(versions, "x86_64", "4.22.10"); err == nil ||
		!strings.Contains(err.Error(), "no 4.22.10 release found for x86_64") {
		t.Fatalf("err = %v, want no 4.22.10 release found for x86_64", err)
	}
}

func TestSelectReleaseRejectsMalformedConstraint(t *testing.T) {
	for _, constraint := range []string{"4.x", "four", "4", "4.22.11.1", "v4.22", "4.-1"} {
		_, err := SelectRelease(nil, "x86_64", constraint)
		if err == nil || !strings.Contains(err.Error(), `invalid version "`+constraint+`"`) {
			t.Fatalf("constraint %q: err = %v, want invalid version error", constraint, err)
		}
	}
}
