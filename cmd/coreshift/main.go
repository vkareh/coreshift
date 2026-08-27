package main

import (
	"os"

	"github.com/spf13/cobra"
)

var version = "dev"

func newRootCommand() *cobra.Command {
	return &cobra.Command{
		Use:     "coreshift",
		Short:   "Run a lightweight OpenShift control plane",
		Version: version,
	}
}

func main() {
	if err := newRootCommand().Execute(); err != nil {
		os.Exit(1)
	}
}
