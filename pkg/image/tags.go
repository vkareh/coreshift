package image

import (
	"context"
	"fmt"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// ListTags returns every tag in repository, e.g.
// "quay.io/openshift-release-dev/ocp-release". It follows the Link
// header pagination of the registry v2 tags API and drops duplicate
// tags. A client with no credentials for the registry lists anonymously.
func (c *Client) ListTags(ctx context.Context, repository string) ([]string, error) {
	repo, err := name.NewRepository(repository)
	if err != nil {
		return nil, fmt.Errorf("parse repository reference: %w", err)
	}
	tags, err := remote.List(repo, remote.WithContext(ctx), remote.WithAuthFromKeychain(c.keychain))
	if err != nil {
		return nil, fmt.Errorf("list tags for %q: %w", repository, err)
	}
	seen := make(map[string]bool, len(tags))
	unique := make([]string, 0, len(tags))
	for _, tag := range tags {
		if !seen[tag] {
			seen[tag] = true
			unique = append(unique, tag)
		}
	}
	return unique, nil
}
