package image

import (
	"context"
	"fmt"
	"path/filepath"
)

// ExtractEtcd resolves the etcd image from a cached release manifest,
// extracts the etcd binary into the cache, and returns its path.
func (c *Client) ExtractEtcd(ctx context.Context, releaseRoot string) (string, error) {
	imageRef, err := ResolveComponentImage(releaseRoot, "etcd")
	if err != nil {
		return "", fmt.Errorf("resolve etcd image: %w", err)
	}
	dir, err := c.extractCached(ctx, componentsKind, imageRef, func(filesystem, target string) error {
		return ExtractBinary(filesystem, "/usr/bin/etcd", target)
	})
	if err != nil {
		return "", fmt.Errorf("extract etcd: %w", err)
	}
	return filepath.Join(dir, "etcd"), nil
}
