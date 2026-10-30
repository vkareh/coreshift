package image

import "fmt"

// verifiedLine is the newest OpenShift major.minor line CoreShift has
// verified its stand-ins against, written X.Y. An unversioned run floats
// to the newest release within this line instead of the registry's
// absolute newest, and a release beyond it runs only when asked for,
// with a warning. Raising it is deliberate work: the stand-ins for a new
// line come first.
const verifiedLine = "4.22"

// newestVersion is the version value that asks for the registry's
// absolute newest release, ignoring the verified line, so the nightly
// drift alarm can keep probing the frontier.
const newestVersion = "newest"

// BeyondLineWarning returns a plain warning for a release image
// reference whose version is from a line newer than the verified one,
// and "" for a reference within it or carrying no version at all.
func BeyondLineWarning(ref string) string {
	v, ok := ParseRefVersion(ref)
	if !ok || !beyondVerifiedLine(v) {
		return ""
	}
	return fmt.Sprintf("warning: %s is beyond the newest verified line, %s — it may not work", v, verifiedLine)
}

// beyondVerifiedLine reports whether v's major.minor is newer than the
// verified line. A malformed constant parses as zero and warns on
// everything, so a broken one cannot fail quietly.
func beyondVerifiedLine(v Version) bool {
	line, _ := parseVersionConstraint(verifiedLine)
	return v.Major > line.version.Major ||
		(v.Major == line.version.Major && v.Minor > line.version.Minor)
}
