//go:build integration

package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/coagenthome"
)

const catalogSectionConfig = `providers:
  fake:
    driver: openrouter
    api_key: test-key
    base_url: %s/api/v1
  pinned:
    driver: openai
    api_key: test-key
    base_url: http://127.0.0.1:9/v1
    catalog: acme
models:
  - id: vendor/kept
    provider: fake
  - id: vendor/dropped
    provider: pinned
sandbox:
  enabled: false
`

func TestHarnessE2E_CatalogSectionDropSurvivesRestart(t *testing.T) {
	home, err := os.MkdirTemp("/tmp", "coa-harness")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	binary := buildBinary(t)
	listed := `{"acme":{"models":{"vendor/dropped":{"name":"Dropped","limit":{"context":10000,"output":1000}}}}}`
	gone := `{"other":{"models":{}}}`
	var body atomic.Pointer[string]
	body.Store(&listed)
	serveModelsDev(t, &body)
	openRouter := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, `{"data":[`+catalogDropModel+`]}`, "kept", "Kept")
	}))
	t.Cleanup(openRouter.Close)
	dir := filepath.Join(home, coagenthome.DirName)
	require.NoError(t, os.MkdirAll(dir, 0o700))
	configBody := fmt.Sprintf(catalogSectionConfig, openRouter.URL)
	require.NoError(t, os.WriteFile(filepath.Join(dir, coagenthome.ConfigFileName), []byte(configBody), 0o600))
	for boot := range 3 {
		assertCatalogDropBoot(t, binary, home, filepath.Join(dir, coagenthome.SocketFileName), boot > 0)
		body.Store(&gone)
	}
}

// serveModelsDev proxies the fixed catalog URL so the compiled daemon needs no
// production override and never reaches the real models.dev service.
func serveModelsDev(t *testing.T, body *atomic.Pointer[string]) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		DNSNames:     []string{"models.dev"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	require.NoError(t, err)
	certFile := filepath.Join(t.TempDir(), "models-dev.pem")
	require.NoError(t, os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600))
	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(*body.Load()))
	}))
	upstream.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
	upstream.StartTLS()
	t.Cleanup(upstream.Close)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect || r.Host != "models.dev:443" {
			http.Error(w, "only models.dev", http.StatusForbidden)
			return
		}
		server, err := net.Dial("tcp", upstream.Listener.Addr().String())
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		client, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			_ = server.Close()
			return
		}
		_, _ = client.Write([]byte("HTTP/1.1 200 OK\r\n\r\n"))
		go func() { _, _ = io.Copy(server, client); _ = server.Close() }()
		_, _ = io.Copy(client, server)
		_ = client.Close()
	}))
	t.Cleanup(proxy.Close)
	t.Setenv("HTTPS_PROXY", proxy.URL)
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")
	t.Setenv("SSL_CERT_FILE", certFile)
}
