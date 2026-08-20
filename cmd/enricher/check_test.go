package main

import (
	"strings"
	"testing"

	"github.com/splattner/alertmanager-label-enricher/internal/config"
)

func TestCheckTLSPassesWithoutTLSConfigured(t *testing.T) {
	if err := checkTLS(&config.Config{}); err != nil {
		t.Fatalf("expected nil error, got: %v", err)
	}
}

func TestCheckTLSRejectsMissingServerCertFile(t *testing.T) {
	cfg := &config.Config{}
	cfg.Server.TLS = &config.ServerTLSSpec{CertFile: "does-not-exist.pem", KeyFile: "does-not-exist-key.pem"}
	err := checkTLS(cfg)
	if err == nil {
		t.Fatal("expected an error for a missing server cert file")
	}
	if !strings.Contains(err.Error(), "server.tls") {
		t.Fatalf("error = %q, want it to mention server.tls", err.Error())
	}
}

func TestCheckTLSRejectsMissingForwardCAFile(t *testing.T) {
	cfg := &config.Config{}
	cfg.Forward.TLS = &config.ClientTLSSpec{CAFile: "does-not-exist-ca.pem"}
	err := checkTLS(cfg)
	if err == nil {
		t.Fatal("expected an error for a missing forward CA file")
	}
	if !strings.Contains(err.Error(), "forward.tls") {
		t.Fatalf("error = %q, want it to mention forward.tls", err.Error())
	}
}
