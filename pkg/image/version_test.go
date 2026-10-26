package image

import (
	"slices"
	"testing"
)

// releaseRegistryTags mirrors the tag shapes real release repositories
// such as quay.io/openshift-release-dev/ocp-release serve: stable
// releases per architecture, candidates, assembly tags, and noise.
var releaseRegistryTags = []string{
	"assembly.2cf84f371b1e75c22a37c0e4b0d0f5d5",
	"assembly.9a0e3b276652c268a95b6d0ee051dfe9",
	"4.10.0-rc.1-x86_64",
	"4.10.0-fc.2-x86_64",
	"4.23.0-rc.0-x86_64",
	"4.14.0",
	"4.22.8-x86_64",
	"4.22.8-aarch64",
	"4.22.8-ppc64le",
	"4.22.9-x86_64",
	"4.23.0-s390x",
	"4.22.11-x86_64",
	"4.22.11-aarch64",
	"not-a-version",
	"4.x.2-x86_64",
	"4.22-x86_64",
	"x86_64",
}

func TestReleaseVersionsFiltersTagShapes(t *testing.T) {
	versions := ReleaseVersions(releaseRegistryTags, "x86_64")

	want := []Version{
		{Major: 4, Minor: 22, Patch: 8},
		{Major: 4, Minor: 22, Patch: 9},
		{Major: 4, Minor: 22, Patch: 11},
	}
	if !slices.Equal(versions, want) {
		t.Fatalf("versions = %v, want %v", versions, want)
	}
}

func TestReleaseVersionsKeepsMatchingArchitecture(t *testing.T) {
	versions := ReleaseVersions(releaseRegistryTags, "aarch64")

	want := []Version{
		{Major: 4, Minor: 22, Patch: 8},
		{Major: 4, Minor: 22, Patch: 11},
	}
	if !slices.Equal(versions, want) {
		t.Fatalf("versions = %v, want %v", versions, want)
	}
}

func TestVersionCompareOrdersHighestLast(t *testing.T) {
	versions := ReleaseVersions(releaseRegistryTags, "x86_64")

	slices.SortFunc(versions, Version.Compare)
	highest := versions[len(versions)-1]
	if highest != (Version{Major: 4, Minor: 22, Patch: 11}) {
		t.Fatalf("highest = %v, want 4.22.11", highest)
	}
}

// TestParseRefVersion checks the version a release image reference
// carries in its tag: registry hosts with ports and both default and
// custom repositories parse, while stream tags, candidates, and digest
// references report none.
func TestParseRefVersion(t *testing.T) {
	for _, tc := range []struct {
		ref  string
		want Version
		ok   bool
	}{
		{"quay.io/openshift-release-dev/ocp-release:4.22.19-x86_64", Version{Major: 4, Minor: 22, Patch: 19}, true},
		{"localhost:5000/ocp-release:4.19.3-aarch64", Version{Major: 4, Minor: 19, Patch: 3}, true},
		{"quay.io/openshift-release-dev/ocp-release:4.22-x86_64", Version{}, false},
		{"quay.io/openshift-release-dev/ocp-release:4.23.0-rc.0-x86_64", Version{Major: 4, Minor: 23, Patch: 0}, true},
		{"quay.io/openshift-release-dev/ocp-release@sha256:0123abcd", Version{}, false},
		{"quay.io/openshift-release-dev/ocp-release", Version{}, false},
		{"assembly.2cf84f371b1e75c22a37c0e4b0d0f5d5", Version{}, false},
	} {
		v, ok := ParseRefVersion(tc.ref)
		if ok != tc.ok || v != tc.want {
			t.Errorf("ParseRefVersion(%q) = %v, %v, want %v, %v", tc.ref, v, ok, tc.want, tc.ok)
		}
	}
}
