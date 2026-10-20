package image

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// Same file `oc adm release info --image-for` reads.
const releaseManifestPath = "release-manifests/image-references"

// Cache subdirectories for release manifests and for records mapping
// tag references to the digests they resolved to.
const (
	releaseKind   = "release"
	tagRecordKind = "tags"
)

// imageStream is the subset of the image.openshift.io/v1 ImageStream schema
// needed to resolve component pullspecs.
type imageStream struct {
	Spec struct {
		Tags []struct {
			Name string `json:"name"`
			From struct {
				Name string `json:"name"`
			} `json:"from"`
		} `json:"tags"`
	} `json:"spec"`
}

// ResolveComponentImage returns the image pullspec for a named component
// (e.g. "etcd", "hyperkube") from the release manifest below releaseRoot,
// the directory CachedRelease returns. Performs no registry calls of its
// own.
func ResolveComponentImage(releaseRoot, componentName string) (string, error) {
	path, err := imagePath(releaseRoot, releaseManifestPath)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read release manifest: %w", err)
	}
	var stream imageStream
	if err := json.Unmarshal(data, &stream); err != nil {
		return "", fmt.Errorf("parse release manifest: %w", err)
	}
	for _, tag := range stream.Spec.Tags {
		if tag.Name == componentName {
			if tag.From.Name == "" {
				return "", fmt.Errorf("component %q has no image pullspec in release manifest", componentName)
			}
			return tag.From.Name, nil
		}
	}
	return "", fmt.Errorf("component %q not found in release manifest", componentName)
}

// CachedRelease returns a directory holding the release image's
// manifest of component image references, pulling the release image
// only when that manifest is not already cached. The manifest is cached
// under the release image digest, and the digest a tag reference
// resolved to is recorded too, so repeat runs with the same release
// make no registry calls at all.
func (c *Client) CachedRelease(ctx context.Context, releaseImage string) (string, error) {
	imageRef := releaseImage
	if !strings.Contains(imageRef, "@") {
		if recorded, ok := readTagRecord(releaseImage); ok {
			if dir, err := cachePath(releaseKind, recorded); err == nil {
				if _, err := os.Stat(dir); err == nil {
					Logger.Printf("using cached image %s", recorded)
					return dir, nil
				}
			}
		}
		resolved, err := c.resolveDigestRef(ctx, releaseImage)
		if err != nil {
			return "", fmt.Errorf("resolve release image: %w", err)
		}
		if err := writeTagRecord(releaseImage, resolved); err != nil {
			return "", fmt.Errorf("record release image digest: %w", err)
		}
		imageRef = resolved
	}
	return c.extractCached(ctx, releaseKind, imageRef, extractReleaseManifest)
}

// resolveDigestRef returns the digest-pinned form of a tag reference.
func (c *Client) resolveDigestRef(ctx context.Context, imageRef string) (string, error) {
	ref, err := name.ParseReference(imageRef)
	if err != nil {
		return "", fmt.Errorf("parse image reference: %w", err)
	}
	head, err := remote.Head(ref, remote.WithContext(ctx), remote.WithAuthFromKeychain(c.keychain))
	if err != nil {
		return "", fmt.Errorf("resolve digest: %w", err)
	}
	return ref.Context().Digest(head.Digest.String()).Name(), nil
}

// readTagRecord returns the digest-pinned reference a tag reference
// resolved to on an earlier run. Any problem reading the record counts
// as no record, so the tag is resolved again instead.
func readTagRecord(imageRef string) (string, bool) {
	path, err := cachePath(tagRecordKind, imageRef)
	if err != nil {
		return "", false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	recorded := strings.TrimSpace(string(data))
	return recorded, recorded != ""
}

// writeTagRecord records the digest-pinned reference a tag reference
// resolved to.
func writeTagRecord(imageRef, digestRef string) error {
	path, err := cachePath(tagRecordKind, imageRef)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create tag record directory: %w", err)
	}
	return os.WriteFile(path, []byte(digestRef), 0o644)
}

// extractReleaseManifest copies the release manifest, the only file
// CoreShift needs from the release image, from an unpacked filesystem
// into dir.
func extractReleaseManifest(filesystem, dir string) error {
	source, err := imagePath(filesystem, releaseManifestPath)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(source)
	if err != nil {
		return fmt.Errorf("read release manifest: %w", err)
	}
	target := filepath.Join(dir, releaseManifestPath)
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return fmt.Errorf("create manifest directory: %w", err)
	}
	if err := os.WriteFile(target, data, 0o644); err != nil {
		return fmt.Errorf("write release manifest: %w", err)
	}
	return nil
}
