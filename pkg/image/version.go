package image

import (
	"cmp"
	"fmt"
	"strconv"
	"strings"
)

// Version is a released version, e.g. 4.22.11. The fields are ordered
// from most to least significant, and Compare orders versions by them.
type Version struct {
	Major, Minor, Patch int
}

// String returns the version as "major.minor.patch", e.g. "4.22.11".
func (v Version) String() string {
	return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
}

// Compare orders versions by major, then minor, then patch: negative
// when v sorts before other, zero when equal, positive when after.
func (v Version) Compare(other Version) int {
	return cmp.Or(
		cmp.Compare(v.Major, other.Major),
		cmp.Compare(v.Minor, other.Minor),
		cmp.Compare(v.Patch, other.Patch),
	)
}

// ReleaseVersions returns the versions of the tags that are plain
// stable releases for arch, i.e. tags shaped
// "<major>.<minor>.<patch>-<arch>" such as "4.22.11-x86_64". Release
// candidates, tags for other architectures, and assembly or otherwise
// differently named tags are left out.
func ReleaseVersions(tags []string, arch string) []Version {
	var versions []Version
	for _, tag := range tags {
		numbers, tagArch, found := strings.Cut(tag, "-")
		if !found || tagArch != arch {
			continue
		}
		if v, ok := parseVersion(numbers); ok {
			versions = append(versions, v)
		}
	}
	return versions
}

// ParseRefVersion returns the version a release image reference carries
// in its tag, e.g. 4.22.19 for
// "quay.io/openshift-release-dev/ocp-release:4.22.19-x86_64". Digest
// references and tags without a plain X.Y.Z version report false.
func ParseRefVersion(ref string) (Version, bool) {
	if i := strings.LastIndex(ref, ":"); i >= 0 {
		ref = ref[i+1:]
	}
	numbers, _, found := strings.Cut(ref, "-")
	if !found {
		return Version{}, false
	}
	return parseVersion(numbers)
}

// parseVersion parses "major.minor.patch" accepting plain digits only,
// so variants like "4.10.0-rc.1" fail instead of parsing as a version.
func parseVersion(numbers string) (Version, bool) {
	parts := strings.Split(numbers, ".")
	if len(parts) != 3 {
		return Version{}, false
	}
	var n [3]uint64
	for i, part := range parts {
		parsed, err := strconv.ParseUint(part, 10, 32)
		if err != nil {
			return Version{}, false
		}
		n[i] = parsed
	}
	return Version{Major: int(n[0]), Minor: int(n[1]), Patch: int(n[2])}, true
}
