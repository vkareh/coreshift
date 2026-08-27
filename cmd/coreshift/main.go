package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/spf13/cobra"
	"github.com/vkareh/coreshift/pkg/image"
)

var version = "dev"

const DefaultReleaseImage = "quay.io/openshift-release-dev/ocp-release:4.18.0-x86_64"

func newRootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:     "coreshift",
		Short:   "Run a lightweight OpenShift control plane",
		Version: version,
	}
	root.AddCommand(newAPICommand())
	return root
}

func newAPICommand() *cobra.Command {
	return &cobra.Command{
		Use:   "api",
		Short: "Run the OpenShift API control plane",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runAPI(cmd.Context())
		},
	}
}

func runAPI(ctx context.Context) error {
	client, err := image.NewClient([]byte(`{"auths":{"quay.io":{}}}`))
	if err != nil {
		return fmt.Errorf("create image client: %w", err)
	}

	filesystem, err := os.MkdirTemp("", "coreshift-api-image-")
	if err != nil {
		return fmt.Errorf("create image directory: %w", err)
	}
	defer os.RemoveAll(filesystem)
	if err := client.PullAndUnpack(ctx, DefaultReleaseImage, filesystem); err != nil {
		return err
	}

	binaries, err := os.MkdirTemp("", "coreshift-api-bin-")
	if err != nil {
		return fmt.Errorf("create binary directory: %w", err)
	}
	defer os.RemoveAll(binaries)
	if err := image.ExtractBinary(filesystem, "/usr/bin/etcd", binaries); err != nil {
		return fmt.Errorf("extract etcd: %w", err)
	}

	process := exec.CommandContext(ctx, filepath.Join(binaries, "etcd"))
	process.Stdout = os.Stdout
	process.Stderr = os.Stderr
	return process.Run()
}

func main() {
	if err := newRootCommand().Execute(); err != nil {
		os.Exit(1)
	}
}
