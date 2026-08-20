package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/splattner/alertmanager-label-enricher/internal/config"
)

func newCheckCmd() *cobra.Command {
	var configPath string

	cmd := &cobra.Command{
		Use:   "check",
		Short: "Validate a config file and exit",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runCheck(cmd, configPath)
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "/etc/enricher/config.yaml", "path to the config file")
	return cmd
}

func runCheck(cmd *cobra.Command, configPath string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}

	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "ok: %d source(s), %d rule(s)\n", len(cfg.Sources), len(cfg.Rules))
	return nil
}
