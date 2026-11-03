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

// The release payload's manifest directory inside the release image,
// and the same file `oc adm release info --image-for` reads.
const (
	releaseManifestDir  = "release-manifests"
	releaseManifestPath = releaseManifestDir + "/image-references"
)

// Cache subdirectories for release manifests, for the release payload's
// manifest sets, and for records mapping tag references to the digests
// they resolved to.
const (
	releaseKind   = "release"
	payloadKind   = "payload"
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

// releaseMetadataName is the file the cached payload's own version
// record lives under, beside the manifest directories.
const releaseMetadataName = releaseManifestDir + "/release-metadata"

// releaseMetadata is the subset of the payload's version record the
// image functions read.
type releaseMetadata struct {
	Version string `json:"version"`
}

// PayloadVersion returns the version the release payload records for
// itself, e.g. 4.22.21, from the directory CachedPayload returns. It is
// the version the release's own manifests name, whatever reference the
// run resolved the payload through.
func PayloadVersion(payloadDir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(payloadDir, releaseMetadataName))
	if err != nil {
		return "", fmt.Errorf("read release metadata: %w", err)
	}
	var metadata releaseMetadata
	if err := json.Unmarshal(data, &metadata); err != nil {
		return "", fmt.Errorf("parse release metadata: %w", err)
	}
	if metadata.Version == "" {
		return "", fmt.Errorf("release metadata records no version")
	}
	return metadata.Version, nil
}

// CachedRelease returns a directory holding the release image's
// manifest of component image references, pulling the release image
// only when that manifest is not already cached. The manifest is cached
// under the release image digest, and the digest a tag reference
// resolved to is recorded too, so repeat runs with the same release
// make no registry calls at all.
func (c *Client) CachedRelease(ctx context.Context, releaseImage string) (string, error) {
	imageRef, err := c.pinnedRelease(ctx, releaseImage)
	if err != nil {
		return "", err
	}
	return c.extractCached(ctx, releaseKind, imageRef, extractReleaseManifest)
}

// CachedPayload returns a directory holding the release image's
// manifest sets, the cluster-version operator's own manifests and the
// release payload's, pulling the release image only when they are not
// already cached. The entry sits beside the manifest cache under the
// same digest, so repeat runs make no registry calls at all.
func (c *Client) CachedPayload(ctx context.Context, releaseImage string) (string, error) {
	imageRef, err := c.pinnedRelease(ctx, releaseImage)
	if err != nil {
		return "", err
	}
	return c.extractCached(ctx, payloadKind, imageRef, extractPayload)
}

// pinnedRelease returns the digest-pinned form of a release reference:
// a tag is resolved through the registry once and the digest recorded,
// so cache entries are keyed by image content and later runs skip the
// registry call.
func (c *Client) pinnedRelease(ctx context.Context, releaseImage string) (string, error) {
	if strings.Contains(releaseImage, "@") {
		return releaseImage, nil
	}
	if recorded, ok := readTagRecord(releaseImage); ok {
		return recorded, nil
	}
	resolved, err := c.resolveDigestRef(ctx, releaseImage)
	if err != nil {
		return "", fmt.Errorf("resolve release image: %w", err)
	}
	if err := writeTagRecord(releaseImage, resolved); err != nil {
		return "", fmt.Errorf("record release image digest: %w", err)
	}
	return resolved, nil
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

// extractPayload copies the release image's manifest directories from an
// unpacked filesystem into dir: the operator's own manifests, which name
// its deployment and definitions, and the release payload's, which the
// operator applies to the cluster. The operator reads both from wherever
// it runs, not its image's filesystem, so the payload cache entry is
// what a run points it at.
func extractPayload(filesystem, dir string) error {
	for _, name := range []string{"manifests", releaseManifestDir} {
		if err := os.CopyFS(filepath.Join(dir, name), os.DirFS(filepath.Join(filesystem, name))); err != nil {
			return fmt.Errorf("copy %s: %w", name, err)
		}
	}
	return nil
}
