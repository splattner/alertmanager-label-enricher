// Command enricher runs the alertmanager-label-enricher: an inline proxy
// between Prometheus and Alertmanager that enriches alert labels from
// configured lookup sources before forwarding.
package main

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/spf13/cobra"
)

// version is set at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if err := newRootCmd().Execute(); err != nil {
		os.Exit(1)
	}
}

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "enricher",
		Short:         "Enrich Prometheus alert labels before forwarding to Alertmanager",
		SilenceUsage:  true,
		SilenceErrors: false,
	}
	root.AddCommand(newServeCmd())
	root.AddCommand(newCheckCmd())
	root.AddCommand(newTestCmd())
	root.AddCommand(newVersionCmd())
	return root
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the build version",
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), version)
			return nil
		},
	}
}

func newLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(os.Stderr, nil))
}
