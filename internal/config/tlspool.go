package config

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"os"
)

// HTTPClientWithCA returns an *http.Client whose TLS trust anchors are the
// system roots PLUS the extra CA certificates in the PEM file at path. An empty
// path returns a plain default client (system trust only). A missing file or a
// PEM with no usable certificate is an error.
//
// This is the in-process trust used by the worker's own outbound TLS (the
// harness LLM client via llm.WithHTTPClient). It APPENDS to the system pool, so
// public endpoints keep working alongside a private/interception CA.
func HTTPClientWithCA(path string) (*http.Client, error) {
	if path == "" {
		return &http.Client{}, nil
	}

	tr, err := CATransport(path)
	if err != nil {
		return nil, err
	}

	return &http.Client{Transport: tr}, nil
}

// CATransport returns an *http.Transport cloned from http.DefaultTransport with
// its TLS trust extended by the extra CA certs in the PEM at path. Cloning
// preserves proxy and timeout behaviour — corporate TLS interception usually
// implies an HTTP(S) proxy too — while overriding only the trust store. An
// empty path returns a nil transport so callers keep their default
// RoundTripper. It is the seam the cmclient/MCP wiring uses to share the same
// trust store as the LLM client.
func CATransport(path string) (*http.Transport, error) {
	if path == "" {
		return nil, nil
	}

	pool, err := caPool(path)
	if err != nil {
		return nil, err
	}

	tlsConf := &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}

	if base, ok := http.DefaultTransport.(*http.Transport); ok {
		clone := base.Clone()
		clone.TLSClientConfig = tlsConf

		return clone, nil
	}

	// Defensive: http.DefaultTransport is always *http.Transport in the stdlib,
	// but if that ever changes, still honour proxy env like the default does.
	return &http.Transport{Proxy: http.ProxyFromEnvironment, TLSClientConfig: tlsConf}, nil
}

// caPool clones the system cert pool (falling back to an empty pool if the
// system pool is unavailable) and appends the PEM certificates in path.
func caPool(path string) (*x509.CertPool, error) {
	pemBytes, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read ca_cert_file %q: %w", path, err)
	}

	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}

	if !pool.AppendCertsFromPEM(pemBytes) {
		return nil, fmt.Errorf("ca_cert_file %q: no valid PEM certificates found", path)
	}

	return pool, nil
}
