//go:build integration

package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/coagenthome"
)

func TestHarnessE2E_CatalogReturnToEarlierOrigin(t *testing.T) {
	home, err := os.MkdirTemp("/tmp", "coa-harness")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	binary := buildBinary(t)
	kept := fmt.Sprintf(catalogDropModel, "kept", "Kept")
	both := `{"data":[` + kept + "," + fmt.Sprintf(catalogDropModel, "dropped", "Dropped") + `]}`
	one := `{"data":[` + kept + `]}`
	var dropped atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/models" && r.URL.Path != "/repointed/api/v1/models" {
			http.NotFound(w, r)
			return
		}
		body := both
		if r.URL.Path == "/api/v1/models" && dropped.Load() {
			body = one
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	dir := filepath.Join(home, coagenthome.DirName)
	path := filepath.Join(dir, coagenthome.ConfigFileName)
	socket := filepath.Join(dir, coagenthome.SocketFileName)
	require.NoError(t, os.MkdirAll(dir, 0o700))
	a := fmt.Sprintf(catalogDropConfig, srv.URL)
	b := fmt.Sprintf(catalogDropConfig, srv.URL+"/repointed")
	for boot, body := range []string{a, a, b, a} {
		require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
		assertCatalogDropBoot(t, binary, home, socket, boot == 1 || boot == 3)
		dropped.Store(true)
	}
}
