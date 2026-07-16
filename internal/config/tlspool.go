package config

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"os"
)

// CATransport returns an *http.Transport cloned from http.DefaultTransport with
// its TLS trust extended by the extra CA certs in the PEM at path — the system
// roots PLUS the private/interception CA, so public endpoints keep working
// alongside it. Cloning preserves proxy and timeout behaviour — corporate TLS
// interception usually implies an HTTP(S) proxy too — while overriding only
// the trust store. An empty path returns a nil transport so callers keep their
// default RoundTripper. A missing file or a PEM with no usable certificate is
// an error. It backs both the worker's harness LLM client (via
// llm.WithHTTPClient) and the cmclient/MCP wiring, so they share the same
// trust.
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
