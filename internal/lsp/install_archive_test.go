package lsp

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type downloadArchiveCase struct {
	name          string
	status        int
	body          []byte
	digest        string
	contentLength int64
	chunked       bool
	wantErr       string
}

func TestDownloadVerifiedArchive(t *testing.T) {
	originalLimit := maxLSPArchiveBytes
	maxLSPArchiveBytes = 8
	defer func() { maxLSPArchiveBytes = originalLimit }()

	for _, tt := range downloadArchiveCases([]byte("artifact")) {
		t.Run(tt.name, func(t *testing.T) {
			runDownloadArchiveCase(t, tt)
		})
	}
}

func downloadArchiveCases(payload []byte) []downloadArchiveCase {
	digest := sha256Hex(payload)

	return []downloadArchiveCase{
		{name: "success", status: http.StatusOK, body: payload, digest: digest},
		{name: "http status", status: http.StatusBadGateway, digest: digest, wantErr: "HTTP status 502"},
		{
			name: "checksum mismatch", status: http.StatusOK, body: payload,
			digest: sha256Hex([]byte("other")), wantErr: "SHA-256 mismatch",
		},
		{
			name: "content length too large", status: http.StatusOK,
			body: []byte("123456789"), digest: digest, contentLength: 9, wantErr: "content length",
		},
		{
			name: "chunked body too large", status: http.StatusOK,
			body: []byte("123456789"), digest: digest, chunked: true, wantErr: "body exceeds",
		},
	}
}

func runDownloadArchiveCase(t *testing.T, tt downloadArchiveCase) {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if tt.contentLength > 0 {
			w.Header().Set("Content-Length", strconv.FormatInt(tt.contentLength, 10))
		}
		w.WriteHeader(tt.status)
		if tt.chunked {
			w.(http.Flusher).Flush()
		}
		_, _ = w.Write(tt.body)
	}))
	defer server.Close()

	dir := t.TempDir()
	artifact := releaseArtifact{url: server.URL, sha256: tt.digest}
	path, err := downloadVerifiedArchive(context.Background(), server.Client(), dir, artifact)
	if tt.wantErr != "" {
		require.Error(t, err)
		assert.Contains(t, err.Error(), tt.wantErr)
		assert.Empty(t, path)
		entries, readErr := os.ReadDir(dir)
		require.NoError(t, readErr)
		assert.Empty(t, entries)

		return
	}

	require.NoError(t, err)
	content, readErr := os.ReadFile(path)
	require.NoError(t, readErr)
	assert.Equal(t, tt.body, content)
}

func TestInstallArtifactPublishesExecutableAndCleansStaging(t *testing.T) {
	payload := gzipTestBytes(t, []byte("language-server"))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(payload)
	}))
	defer server.Close()

	dir := t.TempDir()
	destination := filepath.Join(dir, "server")
	artifact := releaseArtifact{
		url:    server.URL,
		sha256: sha256Hex(payload),
		kind:   archiveGzip,
	}

	require.NoError(t, installArtifact(context.Background(), server.Client(), destination, artifact))
	content, err := os.ReadFile(destination)
	require.NoError(t, err)
	assert.Equal(t, "language-server", string(content))
	info, err := os.Stat(destination)
	require.NoError(t, err)
	assert.NotZero(t, info.Mode().Perm()&0o111)
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Len(t, entries, 1)
}

func TestInstallArtifactFailureLeavesNoDestinationOrStaging(t *testing.T) {
	payload := []byte("not gzip")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(payload)
	}))
	defer server.Close()

	dir := t.TempDir()
	destination := filepath.Join(dir, "server")
	artifact := releaseArtifact{
		url:    server.URL,
		sha256: sha256Hex(payload),
		kind:   archiveGzip,
	}

	require.Error(t, installArtifact(context.Background(), server.Client(), destination, artifact))
	assert.NoFileExists(t, destination)
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Empty(t, entries)
}

func gzipTestBytes(t *testing.T, content []byte) []byte {
	t.Helper()

	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, err := zw.Write(content)
	require.NoError(t, err)
	require.NoError(t, zw.Close())

	return buf.Bytes()
}

func sha256Hex(content []byte) string {
	digest := sha256.Sum256(content)

	return hex.EncodeToString(digest[:])
}
