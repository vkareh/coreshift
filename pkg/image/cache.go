package image

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
)

// Logger receives image pull and cache messages. It discards them by
// default so runs stay quiet; callers wanting the messages, such as
// verbose runs, point it at a real writer.
var Logger = log.New(io.Discard, "", log.LstdFlags)

// componentsKind is the cache subdirectory holding files extracted from
// component images.
const componentsKind = "components"

// cachePath returns the cache location for imageRef below the given
// subdirectory. References keep their registry and repository structure,
// so the reference itself names the cache entry and different images
// never share one. Colons become dashes because they would break the
// library search path pointing extracted binaries at their bundled
// libraries; a collision would require the same digest, hence the same
// content, and is harmless.
func cachePath(kind, imageRef string) (string, error) {
	root, err := CacheRoot()
	if err != nil {
		return "", err
	}
	path := filepath.Join(root, kind, filepath.FromSlash(strings.ReplaceAll(imageRef, ":", "-")))
	if rel, err := filepath.Rel(root, path); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("image reference escapes cache: %q", imageRef)
	}
	return path, nil
}

// CacheRoot returns the XDG cache directory CoreShift keeps extracted
// image content in: $XDG_CACHE_HOME/coreshift, defaulting to
// ~/.cache/coreshift.
func CacheRoot() (string, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("determine cache directory: %w", err)
	}
	return filepath.Join(dir, "coreshift"), nil
}

// extractCached returns the cache directory holding the files extract
// copies out of imageRef, populating it on first use: the image is
// pulled and unpacked to a temporary directory, extract copies the
// wanted files into a temporary directory inside the cache, and that
// directory is renamed into place so a partially populated cache entry
// is never used. imageRef must be digest-pinned, so the cache directory
// identifies the image content and repeat runs skip all registry
// traffic and unpacking.
func (c *Client) extractCached(ctx context.Context, kind, imageRef string, extract func(filesystem, dir string) error) (string, error) {
	dir, err := cachePath(kind, imageRef)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(dir); err == nil {
		Logger.Printf("using cached image %s", imageRef)
		return dir, nil
	}
	Logger.Printf("pulling image %s", imageRef)

	filesystem, err := os.MkdirTemp("", "coreshift-image-")
	if err != nil {
		return "", fmt.Errorf("create image directory: %w", err)
	}
	defer os.RemoveAll(filesystem)
	if err := c.PullAndUnpack(ctx, imageRef, filesystem); err != nil {
		return "", fmt.Errorf("pull image: %w", err)
	}

	parent := filepath.Dir(dir)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return "", fmt.Errorf("create cache directory: %w", err)
	}
	tmp, err := os.MkdirTemp(parent, "."+filepath.Base(dir)+".")
	if err != nil {
		return "", fmt.Errorf("create cache directory: %w", err)
	}
	if err := extract(filesystem, tmp); err != nil {
		os.RemoveAll(tmp)
		return "", err
	}
	if err := os.Rename(tmp, dir); err != nil {
		os.RemoveAll(tmp)
		// Another run populated the same cache entry first; use its result.
		if _, err := os.Stat(dir); err == nil {
			return dir, nil
		}
		return "", fmt.Errorf("publish cache directory: %w", err)
	}
	return dir, nil
}
