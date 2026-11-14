package image

import (
	"context"
	"fmt"
	"path/filepath"
)

// ExtractTests resolves the tests image from a cached release manifest,
// extracts the openshift-tests binary into the cache, and returns its
// path. The binary is the release's own test suite: it is run against a
// control plane, never part of one.
func (c *Client) ExtractTests(ctx context.Context, releaseRoot string) (string, error) {
	imageRef, err := ResolveComponentImage(releaseRoot, "tests")
	if err != nil {
		return "", fmt.Errorf("resolve tests image: %w", err)
	}
	dir, err := c.extractCached(ctx, componentsKind, imageRef, func(filesystem, target string) error {
		return ExtractBinary(filesystem, "/usr/bin/openshift-tests", target)
	})
	if err != nil {
		return "", fmt.Errorf("extract tests: %w", err)
	}
	return filepath.Join(dir, "openshift-tests"), nil
}
