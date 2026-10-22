package image

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// Release is a chosen release version together with the architecture
// it was filtered for, enough to rebuild its registry tag.
type Release struct {
	Version Version
	Arch    string
}

// Tag returns the release repository tag, e.g. "4.22.11-x86_64".
func (r Release) Tag() string {
	return r.Version.String() + "-" + r.Arch
}

// SelectRelease picks the release to use from the versions that
// ReleaseVersions filtered for arch. An empty constraint picks the
// highest version overall, "4.22" picks the highest patch release
// within 4.22, and "4.22.11" picks exactly that version. When
// nothing matches, the error names the constraint, the architecture,
// and what was available instead.
func SelectRelease(versions []Version, arch, constraint string) (Release, error) {
	c, err := parseVersionConstraint(constraint)
	if err != nil {
		return Release{}, err
	}
	var best Version
	found := false
	for _, v := range versions {
		if !c.matches(v) {
			continue
		}
		if !found || v.Compare(best) > 0 {
			best, found = v, true
		}
	}
	if !found {
		return Release{}, notFoundError(versions, arch, constraint)
	}
	return Release{Version: best, Arch: arch}, nil
}

// versionConstraint is a parsed version constraint: parts counts the
// numbers given, so 0 accepts any version, 2 pins major.minor, and 3
// pins the exact version.
type versionConstraint struct {
	version Version
	parts   int
}

func (c versionConstraint) matches(v Version) bool {
	switch c.parts {
	case 0:
		return true
	case 2:
		return v.Major == c.version.Major && v.Minor == c.version.Minor
	default:
		return v == c.version
	}
}

// parseVersionConstraint parses an optional X.Y or X.Y.Z constraint,
// rejecting anything that is not plain numbers in one of those forms.
func parseVersionConstraint(constraint string) (versionConstraint, error) {
	if constraint == "" {
		return versionConstraint{}, nil
	}
	parts := strings.Split(constraint, ".")
	if len(parts) < 2 || len(parts) > 3 {
		return versionConstraint{}, badConstraint(constraint)
	}
	var n [3]int
	for i, part := range parts {
		parsed, err := strconv.ParseUint(part, 10, 32)
		if err != nil {
			return versionConstraint{}, badConstraint(constraint)
		}
		n[i] = int(parsed)
	}
	return versionConstraint{
		version: Version{Major: n[0], Minor: n[1], Patch: n[2]},
		parts:   len(parts),
	}, nil
}

func badConstraint(constraint string) error {
	return fmt.Errorf("invalid version %q: must be X.Y or X.Y.Z", constraint)
}

// notFoundError reports a failed selection: what was wanted, for
// which architecture, and which versions were available instead.
func notFoundError(versions []Version, arch, constraint string) error {
	if len(versions) == 0 {
		return fmt.Errorf("no stable release tags found for %s", arch)
	}
	return fmt.Errorf("no %s release found for %s; available: %s",
		constraint, arch, formatVersions(versions))
}

// formatVersions lists versions lowest to highest, for error messages.
func formatVersions(versions []Version) string {
	sorted := slices.Clone(versions)
	slices.SortFunc(sorted, Version.Compare)
	parts := make([]string, len(sorted))
	for i, v := range sorted {
		parts[i] = v.String()
	}
	return strings.Join(parts, ", ")
}
