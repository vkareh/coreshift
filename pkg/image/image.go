// Package image pulls OCI images and unpacks their filesystem layers.
package image

import (
	"archive/tar"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// Client pulls images using credentials from a Docker or Kubernetes pull
// secret. PullSecret must contain its JSON contents, not a file name.
type Client struct {
	keychain authn.Keychain
}

type dockerConfig struct {
	Auths map[string]dockerAuth `json:"auths"`
}

type dockerAuth struct {
	Auth          string `json:"auth"`
	Username      string `json:"username"`
	Password      string `json:"password"`
	IdentityToken string `json:"identitytoken"`
}

// NewClient creates a client from a Docker config JSON pull secret.
func NewClient(pullSecret []byte) (*Client, error) {
	var config dockerConfig
	if err := json.Unmarshal(pullSecret, &config); err != nil {
		return nil, fmt.Errorf("parse pull secret: %w", err)
	}
	if len(config.Auths) == 0 {
		return nil, errors.New("pull secret contains no registry credentials")
	}
	return &Client{keychain: pullSecretKeychain{auths: config.Auths}}, nil
}

// PullAndUnpack downloads imageRef and applies its layers to destination.
// destination is created if it does not exist. Existing files are preserved
// unless replaced or removed by an image layer.
func (c *Client) PullAndUnpack(ctx context.Context, imageRef, destination string) error {
	ref, err := name.ParseReference(imageRef)
	if err != nil {
		return fmt.Errorf("parse image reference: %w", err)
	}
	img, err := remote.Image(ref, remote.WithContext(ctx), remote.WithAuthFromKeychain(c.keychain))
	if err != nil {
		return fmt.Errorf("pull image %q: %w", imageRef, err)
	}
	if err := os.MkdirAll(destination, 0o755); err != nil {
		return fmt.Errorf("create destination: %w", err)
	}
	layers, err := img.Layers()
	if err != nil {
		return fmt.Errorf("read image layers: %w", err)
	}
	for i, layer := range layers {
		if err := unpackLayer(layer, destination); err != nil {
			return fmt.Errorf("unpack layer %d: %w", i, err)
		}
	}
	return nil
}

type pullSecretKeychain struct{ auths map[string]dockerAuth }

func (k pullSecretKeychain) Resolve(target authn.Resource) (authn.Authenticator, error) {
	registry := target.RegistryStr()
	for key, cfg := range k.auths {
		if registry == normalizeRegistry(key) || (registry == "docker.io" && normalizeRegistry(key) == "index.docker.io") {
			if cfg.IdentityToken != "" {
				return authn.FromConfig(authn.AuthConfig{IdentityToken: cfg.IdentityToken}), nil
			}
			username, password := cfg.Username, cfg.Password
			if username == "" && cfg.Auth != "" {
				decoded, err := base64.StdEncoding.DecodeString(cfg.Auth)
				if err != nil {
					return nil, fmt.Errorf("decode credentials for %s: %w", registry, err)
				}
				parts := strings.SplitN(string(decoded), ":", 2)
				if len(parts) == 2 {
					username, password = parts[0], parts[1]
				}
			}
			return authn.FromConfig(authn.AuthConfig{Username: username, Password: password}), nil
		}
	}
	return authn.Anonymous, nil
}

func normalizeRegistry(value string) string {
	value = strings.TrimSuffix(value, "/")
	if parsed, err := url.Parse(value); err == nil && parsed.Host != "" {
		value = parsed.Host
	}
	value = strings.TrimPrefix(value, "https://")
	value = strings.TrimPrefix(value, "http://")
	value = strings.TrimSuffix(value, "/v1")
	return value
}

func unpackLayer(layer v1.Layer, destination string) error {
	r, err := layer.Uncompressed()
	if err != nil {
		return err
	}
	defer r.Close()
	tr := tar.NewReader(r)
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		name, err := safeName(destination, header.Name)
		if err != nil {
			return err
		}
		base := filepath.Base(name)
		if strings.HasPrefix(base, ".wh.") {
			if base == ".wh..wh..opq" {
				if err := clearDirectory(name[:len(name)-len(base)]); err != nil {
					return err
				}
			} else if err := os.RemoveAll(filepath.Join(filepath.Dir(name), strings.TrimPrefix(base, ".wh."))); err != nil {
				return err
			}
			continue
		}
		if err := extractEntry(tr, destination, name, header); err != nil {
			return err
		}
	}
}

func safeName(destination, archiveName string) (string, error) {
	archiveName = strings.TrimPrefix(archiveName, "/")
	clean := path.Clean(archiveName)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("archive path escapes destination: %q", archiveName)
	}
	return filepath.Join(destination, filepath.FromSlash(clean)), nil
}

func extractEntry(r io.Reader, destination, target string, h *tar.Header) error {
	if err := ensureParents(destination, filepath.Dir(target)); err != nil {
		return err
	}
	if h.Typeflag == tar.TypeDir {
		// Image ownership is intentionally ignored. A root-owned, read-only
		// directory in an image must still be writable while later layers apply.
		if err := os.MkdirAll(target, 0o755); err != nil {
			return err
		}
		return nil
	}
	if err := os.RemoveAll(target); err != nil {
		return err
	}
	switch h.Typeflag {
	case tar.TypeSymlink:
		return os.Symlink(h.Linkname, target)
	case tar.TypeLink:
		link, err := safeName(destination, h.Linkname)
		if err != nil {
			return err
		}
		return os.Link(link, target)
	case tar.TypeReg, tar.TypeRegA:
		f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC|os.O_EXCL, os.FileMode(h.Mode)&0o777)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(f, r)
		closeErr := f.Close()
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	default:
		return nil
	}
}

func ensureParents(destination, parent string) error {
	rel, err := filepath.Rel(destination, parent)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return errors.New("archive path escapes destination")
	}
	current := destination
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if part == "." || part == "" {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			if err := os.Mkdir(current, 0o755); err != nil {
				return err
			}
			continue
		}
		if err != nil || !info.IsDir() {
			return fmt.Errorf("archive parent is not directory: %s", current)
		}
	}
	return nil
}

func clearDirectory(directory string) error {
	entries, err := os.ReadDir(directory)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := os.RemoveAll(filepath.Join(directory, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}
