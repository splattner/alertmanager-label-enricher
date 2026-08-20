package main

import (
	"flag"
	"fmt"

	"github.com/splattner/alertmanager-label-enricher/internal/config"
)

func runCheck(args []string) error {
	fs := flag.NewFlagSet("check", flag.ExitOnError)
	configPath := fs.String("config", "/etc/enricher/config.yaml", "path to the config file")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}

	fmt.Printf("ok: %d source(s), %d rule(s)\n", len(cfg.Sources), len(cfg.Rules))
	return nil
}
