// Package tlsutil builds tls.Config values for the enricher's inbound
// listener and outbound connections from PEM file paths.
package tlsutil

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
)

// ServerConfig builds a tls.Config for the enricher's own listener from a
// server certificate/key pair. If clientCAFile is non-empty, it also loads
// a client CA pool and requires+verifies a client certificate on every
// connection (mutual TLS) — the intended use is authenticating the
// Prometheus instances that are allowed to POST alerts.
func ServerConfig(certFile, keyFile, clientCAFile string) (*tls.Config, error) {
	if certFile == "" || keyFile == "" {
		return nil, fmt.Errorf("certFile and keyFile are both required")
	}
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("load server certificate: %w", err)
	}

	cfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}

	if clientCAFile != "" {
		pool, err := loadCAPool(clientCAFile)
		if err != nil {
			return nil, fmt.Errorf("load client CA: %w", err)
		}
		cfg.ClientCAs = pool
		cfg.ClientAuth = tls.RequireAndVerifyClientCert
	}

	return cfg, nil
}

// ClientConfig builds a tls.Config for outbound connections to an
// Alertmanager target. caFile, if set, trusts an additional CA (e.g. a
// self-signed or internal one) alongside the system pool. certFile/keyFile,
// if set, present a client certificate for mutual TLS. insecureSkipVerify
// is an explicit, discouraged escape hatch for environments without a
// usable CA.
func ClientConfig(caFile, certFile, keyFile string, insecureSkipVerify bool) (*tls.Config, error) {
	cfg := &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: insecureSkipVerify, //nolint:gosec // explicit opt-in, documented above
	}

	if caFile != "" {
		pool, err := loadCAPool(caFile)
		if err != nil {
			return nil, fmt.Errorf("load CA: %w", err)
		}
		cfg.RootCAs = pool
	}

	switch {
	case certFile != "" && keyFile != "":
		cert, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			return nil, fmt.Errorf("load client certificate: %w", err)
		}
		cfg.Certificates = []tls.Certificate{cert}
	case certFile != "" || keyFile != "":
		return nil, fmt.Errorf("certFile and keyFile must both be set, or both empty")
	}

	return cfg, nil
}

func loadCAPool(path string) (*x509.CertPool, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(raw) {
		return nil, fmt.Errorf("%s contains no usable PEM certificates", path)
	}
	return pool, nil
}
