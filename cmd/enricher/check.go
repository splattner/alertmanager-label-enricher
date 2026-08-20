package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/splattner/alertmanager-label-enricher/internal/config"
	"github.com/splattner/alertmanager-label-enricher/internal/tlsutil"
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

	if err := checkTLS(cfg); err != nil {
		return err
	}

	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "ok: %d source(s), %d rule(s)\n", len(cfg.Sources), len(cfg.Rules))
	return nil
}

// checkTLS builds the same tls.Config values serve would, so a bad or
// missing cert/key/CA file is caught here rather than surfacing later at
// startup or, worse, at first client connection.
func checkTLS(cfg *config.Config) error {
	if cfg.Server.TLS != nil {
		t := cfg.Server.TLS
		if _, err := tlsutil.ServerConfig(t.CertFile, t.KeyFile, t.ClientCAFile); err != nil {
			return fmt.Errorf("server.tls: %w", err)
		}
	}
	if cfg.Forward.TLS != nil {
		t := cfg.Forward.TLS
		if _, err := tlsutil.ClientConfig(t.CAFile, t.CertFile, t.KeyFile, t.InsecureSkipVerify); err != nil {
			return fmt.Errorf("forward.tls: %w", err)
		}
	}
	return nil
}
