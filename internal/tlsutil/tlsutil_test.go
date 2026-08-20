package tlsutil

import (
	"crypto/tls"
	"io"
	"testing"
)

func TestServerConfigRequiresBothCertAndKey(t *testing.T) {
	if _, err := ServerConfig("", "", ""); err == nil {
		t.Fatal("expected an error when certFile/keyFile are empty")
	}
}

func TestClientConfigRejectsUnpairedCertOrKey(t *testing.T) {
	if _, err := ClientConfig("", "/only/cert.pem", "", false); err == nil {
		t.Fatal("expected an error when only certFile is set")
	}
	if _, err := ClientConfig("", "", "/only/key.pem", false); err == nil {
		t.Fatal("expected an error when only keyFile is set")
	}
}

// serveOnce accepts exactly one TLS connection with cfg and echoes what it
// reads, then closes. Returns the listener's address.
func serveOnce(t *testing.T, cfg *tls.Config) string {
	t.Helper()
	ln, err := tls.Listen("tcp", "127.0.0.1:0", cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		buf := make([]byte, 64)
		n, _ := conn.Read(buf)
		_, _ = conn.Write(buf[:n])
	}()
	return ln.Addr().String()
}

func dial(addr string, cfg *tls.Config) error {
	conn, err := tls.Dial("tcp", addr, cfg)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.Write([]byte("ping")); err != nil {
		return err
	}
	buf := make([]byte, 4)
	_, err = io.ReadFull(conn, buf)
	return err
}

func TestServerAndClientConfigHandshakeWithTrustedCA(t *testing.T) {
	ca := newTestCA(t)
	serverCertPEM, serverKeyPEM := ca.leaf(t, "server")
	certFile := writeFile(t, "server-cert.pem", serverCertPEM)
	keyFile := writeFile(t, "server-key.pem", serverKeyPEM)
	caFile := writeFile(t, "ca.pem", ca.certPEM)

	serverCfg, err := ServerConfig(certFile, keyFile, "")
	if err != nil {
		t.Fatal(err)
	}
	addr := serveOnce(t, serverCfg)

	clientCfg, err := ClientConfig(caFile, "", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := dial(addr, clientCfg); err != nil {
		t.Fatalf("handshake with a trusted CA should succeed: %v", err)
	}
}

func TestClientConfigRejectsUntrustedServer(t *testing.T) {
	ca := newTestCA(t)
	serverCertPEM, serverKeyPEM := ca.leaf(t, "server")
	certFile := writeFile(t, "server-cert.pem", serverCertPEM)
	keyFile := writeFile(t, "server-key.pem", serverKeyPEM)

	serverCfg, err := ServerConfig(certFile, keyFile, "")
	if err != nil {
		t.Fatal(err)
	}
	addr := serveOnce(t, serverCfg)

	// No caFile: the client only trusts the system pool, which does not
	// include our test CA.
	clientCfg, err := ClientConfig("", "", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := dial(addr, clientCfg); err == nil {
		t.Fatal("expected the handshake to fail against an untrusted server")
	}
}

func TestClientConfigInsecureSkipVerifyBypassesTrust(t *testing.T) {
	ca := newTestCA(t)
	serverCertPEM, serverKeyPEM := ca.leaf(t, "server")
	certFile := writeFile(t, "server-cert.pem", serverCertPEM)
	keyFile := writeFile(t, "server-key.pem", serverKeyPEM)

	serverCfg, err := ServerConfig(certFile, keyFile, "")
	if err != nil {
		t.Fatal(err)
	}
	addr := serveOnce(t, serverCfg)

	clientCfg, err := ClientConfig("", "", "", true)
	if err != nil {
		t.Fatal(err)
	}
	if err := dial(addr, clientCfg); err != nil {
		t.Fatalf("insecureSkipVerify should bypass trust: %v", err)
	}
}

func TestServerConfigWithClientCARequiresClientCertificate(t *testing.T) {
	serverCA := newTestCA(t)
	serverCertPEM, serverKeyPEM := serverCA.leaf(t, "server")
	serverCertFile := writeFile(t, "server-cert.pem", serverCertPEM)
	serverKeyFile := writeFile(t, "server-key.pem", serverKeyPEM)
	serverCAFile := writeFile(t, "server-ca.pem", serverCA.certPEM)

	clientCA := newTestCA(t)
	clientCAFile := writeFile(t, "client-ca.pem", clientCA.certPEM)
	clientCertPEM, clientKeyPEM := clientCA.leaf(t, "client")
	clientCertFile := writeFile(t, "client-cert.pem", clientCertPEM)
	clientKeyFile := writeFile(t, "client-key.pem", clientKeyPEM)

	serverCfg, err := ServerConfig(serverCertFile, serverKeyFile, clientCAFile)
	if err != nil {
		t.Fatal(err)
	}
	if serverCfg.ClientAuth != tls.RequireAndVerifyClientCert {
		t.Fatalf("ClientAuth = %v, want RequireAndVerifyClientCert", serverCfg.ClientAuth)
	}

	t.Run("without a client certificate the handshake fails", func(t *testing.T) {
		addr := serveOnce(t, serverCfg)
		clientCfg, err := ClientConfig(serverCAFile, "", "", false)
		if err != nil {
			t.Fatal(err)
		}
		if err := dial(addr, clientCfg); err == nil {
			t.Fatal("expected the handshake to fail without a client certificate")
		}
	})

	t.Run("with a valid client certificate the handshake succeeds", func(t *testing.T) {
		addr := serveOnce(t, serverCfg)
		clientCfg, err := ClientConfig(serverCAFile, clientCertFile, clientKeyFile, false)
		if err != nil {
			t.Fatal(err)
		}
		if err := dial(addr, clientCfg); err != nil {
			t.Fatalf("handshake with a valid client certificate should succeed: %v", err)
		}
	})

	t.Run("with a client certificate from an untrusted CA the handshake fails", func(t *testing.T) {
		otherCA := newTestCA(t)
		otherCertPEM, otherKeyPEM := otherCA.leaf(t, "impostor")
		otherCertFile := writeFile(t, "other-cert.pem", otherCertPEM)
		otherKeyFile := writeFile(t, "other-key.pem", otherKeyPEM)

		addr := serveOnce(t, serverCfg)
		clientCfg, err := ClientConfig(serverCAFile, otherCertFile, otherKeyFile, false)
		if err != nil {
			t.Fatal(err)
		}
		if err := dial(addr, clientCfg); err == nil {
			t.Fatal("expected the handshake to fail with a client cert from an untrusted CA")
		}
	})
}

func TestLoadCAPoolRejectsGarbage(t *testing.T) {
	path := writeFile(t, "not-a-cert.pem", []byte("not a certificate"))
	if _, err := ClientConfig(path, "", "", false); err == nil {
		t.Fatal("expected an error for a CA file with no usable PEM certificates")
	}
}
