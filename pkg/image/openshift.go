package image

import (
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
)

// openshiftComponent describes one OpenShift API layer component. Unlike
// the kube components they do not share an image: each ships in its own
// release component image, resolved by name.
type openshiftComponent struct {
	component      string
	path           string
	extraLibraries []string
}

// openshiftComponents lists the OpenShift API layer components and their
// binary paths inside their images, in extraction order.
//
// Most of these binaries link only against glibc, like the kube binaries.
// openshift-controller-manager also links against the gpgme stack used for
// image signature verification, so those libraries are copied alongside it
// and it must run with LD_LIBRARY_PATH pointing at the extraction
// directory.
var openshiftComponents = []openshiftComponent{
	{component: "openshift-apiserver", path: "/usr/bin/openshift-apiserver"},
	{component: "oauth-apiserver", path: "/usr/bin/oauth-apiserver"},
	{component: "oauth-server", path: "/usr/bin/oauth-server"},
	{
		component: "openshift-controller-manager",
		path:      "/usr/bin/openshift-controller-manager",
		extraLibraries: []string{
			"/usr/lib64/libgpgme.so.11",
			"/usr/lib64/libgpg-error.so.0",
			"/usr/lib64/libassuan.so.0",
		},
	},
	{component: "cluster-policy-controller", path: "/usr/bin/cluster-policy-controller"},
}

// ExtractOpenShiftComponents resolves each requested OpenShift API layer
// component's image from an unpacked release image, pulls it, and extracts
// the component binary into targetDirectory. It returns a map of component
// name to local binary path. With no names, every known component is
// extracted; otherwise only the named ones, so callers that run a subset do
// not pay for the other component image pulls.
func (c *Client) ExtractOpenShiftComponents(ctx context.Context, releaseImageRoot, targetDirectory string, names ...string) (map[string]string, error) {
	selected, err := selectOpenShiftComponents(names...)
	if err != nil {
		return nil, err
	}

	binaries := make(map[string]string, len(selected))
	for _, component := range selected {
		path, err := c.extractOpenShiftComponent(ctx, releaseImageRoot, component, targetDirectory)
		if err != nil {
			return nil, err
		}
		binaries[component.component] = path
	}
	return binaries, nil
}

// selectOpenShiftComponents filters the extraction table to the named
// components, in table order. With no names it returns the whole table.
func selectOpenShiftComponents(names ...string) ([]openshiftComponent, error) {
	if len(names) == 0 {
		return openshiftComponents, nil
	}
	wanted := make(map[string]bool, len(names))
	for _, name := range names {
		wanted[name] = true
	}
	var selected []openshiftComponent
	for _, component := range openshiftComponents {
		if wanted[component.component] {
			selected = append(selected, component)
			delete(wanted, component.component)
		}
	}
	if len(wanted) > 0 {
		return nil, fmt.Errorf("unknown OpenShift components: %v", slices.Sorted(maps.Keys(wanted)))
	}
	return selected, nil
}

// extractOpenShiftComponent resolves one component's image from an
// unpacked release image, pulls it, and extracts the binary into
// targetDirectory, returning the extracted binary path.
func (c *Client) extractOpenShiftComponent(ctx context.Context, releaseImageRoot string, component openshiftComponent, targetDirectory string) (string, error) {
	imageRef, err := ResolveComponentImage(releaseImageRoot, component.component)
	if err != nil {
		return "", fmt.Errorf("resolve %s image: %w", component.component, err)
	}

	filesystem, err := os.MkdirTemp("", "coreshift-"+component.component+"-")
	if err != nil {
		return "", fmt.Errorf("create %s image directory: %w", component.component, err)
	}
	defer os.RemoveAll(filesystem)
	if err := c.PullAndUnpack(ctx, imageRef, filesystem); err != nil {
		return "", fmt.Errorf("pull %s image: %w", component.component, err)
	}
	return extractOpenShiftBinary(component, filesystem, targetDirectory)
}

// extractOpenShiftBinary copies one component's binary, plus any shared
// libraries it links beyond glibc, from its unpacked image filesystem into
// targetDirectory and returns the extracted binary path.
func extractOpenShiftBinary(component openshiftComponent, filesystemRoot, targetDirectory string) (string, error) {
	if err := ExtractBinary(filesystemRoot, component.path, targetDirectory, component.extraLibraries); err != nil {
		return "", fmt.Errorf("extract %s: %w", component.component, err)
	}
	return filepath.Join(targetDirectory, filepath.Base(component.path)), nil
}
