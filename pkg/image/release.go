package image

import (
	"encoding/json"
	"fmt"
	"os"
)

// Same file `oc adm release info --image-for` reads.
const releaseManifestPath = "release-manifests/image-references"

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
// (e.g. "etcd", "hyperkube") from an already unpacked release image's
// filesystem at releaseImageRoot. Performs no registry calls of its own.
func ResolveComponentImage(releaseImageRoot, componentName string) (string, error) {
	path, err := imagePath(releaseImageRoot, releaseManifestPath)
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
