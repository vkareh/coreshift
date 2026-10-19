package image

import (
	"context"
	"fmt"
	"path/filepath"
)

// hyperkubeComponent is the release manifest component carrying the core
// kube binaries. Unlike most components it ships several binaries in one
// image.
const hyperkubeComponent = "hyperkube"

// kubeComponents lists the core kube components and their paths inside
// the hyperkube image, in extraction order.
var kubeComponents = []struct{ component, path string }{
	{"kube-apiserver", "/usr/bin/kube-apiserver"},
	{"kube-controller-manager", "/usr/bin/kube-controller-manager"},
	{"kube-scheduler", "/usr/bin/kube-scheduler"},
}

// ExtractKubeComponents resolves the hyperkube image from a cached
// release manifest, extracts the core kube binaries into the cache, and
// returns a map of component name to local binary path.
func (c *Client) ExtractKubeComponents(ctx context.Context, releaseRoot string) (map[string]string, error) {
	imageRef, err := ResolveComponentImage(releaseRoot, hyperkubeComponent)
	if err != nil {
		return nil, fmt.Errorf("resolve hyperkube image: %w", err)
	}
	dir, err := c.extractCached(ctx, componentsKind, imageRef, func(filesystem, target string) error {
		_, err := extractKubeBinaries(filesystem, target)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("extract kube components: %w", err)
	}
	return kubeBinaryPaths(dir), nil
}

// extractKubeBinaries copies the core kube binaries from an unpacked
// hyperkube image filesystem into targetDirectory and returns a map of
// component name to extracted binary path.
func extractKubeBinaries(hyperkubeRoot, targetDirectory string) (map[string]string, error) {
	for _, component := range kubeComponents {
		if err := ExtractBinary(hyperkubeRoot, component.path, targetDirectory); err != nil {
			return nil, fmt.Errorf("extract %s: %w", component.component, err)
		}
	}
	return kubeBinaryPaths(targetDirectory), nil
}

// kubeBinaryPaths maps component names to their extracted binary paths
// in dir.
func kubeBinaryPaths(dir string) map[string]string {
	binaries := make(map[string]string, len(kubeComponents))
	for _, component := range kubeComponents {
		binaries[component.component] = filepath.Join(dir, filepath.Base(component.path))
	}
	return binaries
}
