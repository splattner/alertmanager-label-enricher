package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/client-go/dynamic"

	"github.com/splattner/alertmanager-label-enricher/internal/alert"
	"github.com/splattner/alertmanager-label-enricher/internal/config"
	"github.com/splattner/alertmanager-label-enricher/internal/engine"
	"github.com/splattner/alertmanager-label-enricher/internal/wiring"
)

// startupTimeout bounds how long `test` waits for sources (chiefly
// Kubernetes informers) to complete their initial sync before running the
// alert through the engine.
const startupTimeout = 30 * time.Second

func newTestCmd() *cobra.Command {
	var configPath, alertPath string

	cmd := &cobra.Command{
		Use:   "test",
		Short: "Run one alert through the rule engine and print the label diff",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runTest(cmd, configPath, alertPath)
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "/etc/enricher/config.yaml", "path to the config file")
	cmd.Flags().StringVar(&alertPath, "alert", "", "path to a JSON file containing one alert object or an array of alerts")
	_ = cmd.MarkFlagRequired("alert")
	return cmd
}

func runTest(cmd *cobra.Command, configPath, alertPath string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}

	var kubeClient dynamic.Interface
	if wiring.NeedsKubeClient(cfg) {
		kubeClient, err = wiring.KubeClient()
		if err != nil {
			return fmt.Errorf("build kubernetes client: %w", err)
		}
	}

	sources, err := wiring.BuildSources(cfg, kubeClient, func(format string, args ...any) {
		fmt.Fprintf(os.Stderr, format+"\n", args...)
	})
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), startupTimeout)
	defer cancel()
	if err := sources.Start(ctx); err != nil {
		return fmt.Errorf("start sources: %w", err)
	}

	eng, err := wiring.BuildEngine(cfg, sources)
	if err != nil {
		return err
	}

	alerts, err := readAlerts(alertPath)
	if err != nil {
		return err
	}

	out := cmd.OutOrStdout()
	for i, a := range alerts {
		before, err := a.Labels()
		if err != nil {
			return fmt.Errorf("alert %d: %w", i, err)
		}
		beforeCopy := make(map[string]string, len(before))
		for k, v := range before {
			beforeCopy[k] = v
		}

		results, applyErr := eng.Apply(ctx, a)
		after, _ := a.Labels()

		_, _ = fmt.Fprintf(out, "=== alert %d ===\n", i)
		printDiff(out, beforeCopy, after)
		for _, r := range results {
			printResult(out, r)
		}
		if applyErr != nil {
			_, _ = fmt.Fprintf(out, "  ERROR: %v\n", applyErr)
		}
	}

	return nil
}

func readAlerts(path string) ([]alert.Alert, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if alerts, err := alert.DecodeBatch(raw); err == nil {
		return alerts, nil
	}

	var single alert.Alert
	if err := json.Unmarshal(raw, &single); err != nil {
		return nil, fmt.Errorf("parse %s as an alert object or array: %w", path, err)
	}
	return []alert.Alert{single}, nil
}

func printDiff(out io.Writer, before, after map[string]string) {
	keys := make(map[string]bool, len(before)+len(after))
	for k := range before {
		keys[k] = true
	}
	for k := range after {
		keys[k] = true
	}
	sorted := make([]string, 0, len(keys))
	for k := range keys {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)

	for _, k := range sorted {
		b, hasB := before[k]
		a, hasA := after[k]
		switch {
		case !hasB && hasA:
			_, _ = fmt.Fprintf(out, "  + %s=%s\n", k, a)
		case hasB && !hasA:
			_, _ = fmt.Fprintf(out, "  - %s=%s\n", k, b)
		case b != a:
			_, _ = fmt.Fprintf(out, "  ~ %s=%s -> %s\n", k, b, a)
		default:
			_, _ = fmt.Fprintf(out, "    %s=%s\n", k, b)
		}
	}
}

func printResult(out io.Writer, r engine.Result) {
	if r.Skipped {
		_, _ = fmt.Fprintf(out, "  rule %s: skipped (no match)\n", r.Rule)
		return
	}
	dryRun := ""
	if r.DryRun {
		dryRun = " (dry-run, not applied)"
	}
	_, _ = fmt.Fprintf(out, "  rule %s: matched%s added=%v overwritten=%v dropped=%v\n",
		r.Rule, dryRun, r.Added, r.Overwritten, r.Dropped)
}
