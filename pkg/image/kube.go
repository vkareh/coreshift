package image

import (
	"context"
	"fmt"
	"os"
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

// ExtractKubeComponents resolves the hyperkube image from an unpacked
// release image, pulls it, and extracts the core kube binaries into
// targetDirectory. It returns a map of component name to local binary
// path.
func (c *Client) ExtractKubeComponents(ctx context.Context, releaseImageRoot, targetDirectory string) (map[string]string, error) {
	imageRef, err := ResolveComponentImage(releaseImageRoot, hyperkubeComponent)
	if err != nil {
		return nil, fmt.Errorf("resolve hyperkube image: %w", err)
	}

	filesystem, err := os.MkdirTemp("", "coreshift-hyperkube-")
	if err != nil {
		return nil, fmt.Errorf("create hyperkube image directory: %w", err)
	}
	defer os.RemoveAll(filesystem)
	if err := c.PullAndUnpack(ctx, imageRef, filesystem); err != nil {
		return nil, fmt.Errorf("pull hyperkube image: %w", err)
	}
	return extractKubeBinaries(filesystem, targetDirectory)
}

// extractKubeBinaries copies the core kube binaries from an unpacked
// hyperkube image filesystem into targetDirectory and returns a map of
// component name to extracted binary path.
func extractKubeBinaries(hyperkubeRoot, targetDirectory string) (map[string]string, error) {
	binaries := make(map[string]string, len(kubeComponents))
	for _, component := range kubeComponents {
		if err := ExtractBinary(hyperkubeRoot, component.path, targetDirectory); err != nil {
			return nil, fmt.Errorf("extract %s: %w", component.component, err)
		}
		binaries[component.component] = filepath.Join(targetDirectory, filepath.Base(component.path))
	}
	return binaries, nil
}
