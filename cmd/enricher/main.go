// Command enricher runs the alertmanager-label-enricher: an inline proxy
// between Prometheus and Alertmanager that enriches alert labels from
// configured lookup sources before forwarding.
package main

import (
	"fmt"
	"log/slog"
	"os"
)

// version is set at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	var err error
	switch os.Args[1] {
	case "serve":
		err = runServe(os.Args[2:])
	case "check":
		err = runCheck(os.Args[2:])
	case "test":
		err = runTest(os.Args[2:])
	case "version":
		fmt.Println(version)
		return
	default:
		usage()
		os.Exit(2)
	}

	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage: enricher <command> [flags]

commands:
  serve    run the enrichment proxy
  check    validate a config file and exit
  test     run one alert through the rule engine and print the label diff
  version  print the build version`)
}

func newLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(os.Stderr, nil))
}
