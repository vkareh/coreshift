package image

import "context"

// ResolveRelease returns the release image reference to use, e.g.
// "quay.io/openshift-release-dev/ocp-release:4.22.11-x86_64". A
// non-empty pinned reference is returned unchanged, without any
// registry calls. Otherwise the tags of repository are listed and the
// release best matching the version constraint and arch is selected;
// an empty version picks the newest release within the verified line,
// and the value "newest" the registry's absolute newest release.
func (c *Client) ResolveRelease(ctx context.Context, repository, version, arch, pinned string) (string, error) {
	if pinned != "" {
		return pinned, nil
	}
	tags, err := c.ListTags(ctx, repository)
	if err != nil {
		return "", err
	}
	switch version {
	case newestVersion:
		version = "" // the frontier: the verified line does not apply
	case "":
		version = verifiedLine
	}
	release, err := SelectRelease(ReleaseVersions(tags, arch), arch, version)
	if err != nil {
		return "", err
	}
	return repository + ":" + release.Tag(), nil
}
