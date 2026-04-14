package lsp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
)

// maxLSPArchiveBytes is mutable so tests can exercise oversize responses
// without writing a 256 MiB fixture.
var maxLSPArchiveBytes int64 = 256 << 20

type httpDoer interface {
	Do(*http.Request) (*http.Response, error)
}

func installRelease(
	ctx context.Context,
	client httpDoer,
	destination, name, goos, goarch string,
) error {
	artifact, ok := releaseArtifactFor(name, goos, goarch)
	if !ok {
		return fmt.Errorf("%s auto-install unsupported on %s/%s", name, goos, goarch)
	}

	return installArtifact(ctx, client, destination, artifact)
}

func installArtifact(
	ctx context.Context,
	client httpDoer,
	destination string,
	artifact releaseArtifact,
) error {
	return stageInstallFile(destination, filepath.Base(destination), func(stage string) error {
		installCtx, cancel := context.WithTimeout(ctx, lspInstallTimeout)
		defer cancel()

		archivePath, err := downloadVerifiedArchive(installCtx, client, stage, artifact)
		if err != nil {
			return err
		}
		defer func() { _ = os.Remove(archivePath) }()

		return extractArtifact(archivePath, filepath.Join(stage, filepath.Base(destination)), artifact)
	})
}

func downloadVerifiedArchive(
	ctx context.Context,
	client httpDoer,
	dir string,
	artifact releaseArtifact,
) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, artifact.url, http.NoBody)
	if err != nil {
		return "", fmt.Errorf("build LSP download request: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("download LSP archive: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return "", fmt.Errorf("download LSP archive: unexpected HTTP status %d", resp.StatusCode)
	}

	if resp.ContentLength > maxLSPArchiveBytes {
		return "", fmt.Errorf(
			"download LSP archive: content length %d exceeds %d",
			resp.ContentLength,
			maxLSPArchiveBytes,
		)
	}

	return copyVerifiedArchive(resp.Body, dir, artifact.sha256)
}

func copyVerifiedArchive(src io.Reader, dir, expectedDigest string) (string, error) {
	tmp, err := os.CreateTemp(dir, ".lsp-archive-*")
	if err != nil {
		return "", fmt.Errorf("create LSP archive temp file: %w", err)
	}

	name := tmp.Name()
	success := false

	defer func() {
		_ = tmp.Close()

		if !success {
			_ = os.Remove(name)
		}
	}()

	hash := sha256.New()

	written, err := io.Copy(io.MultiWriter(tmp, hash), io.LimitReader(src, maxLSPArchiveBytes+1))
	if err != nil {
		return "", fmt.Errorf("read LSP archive: %w", err)
	}

	if written > maxLSPArchiveBytes {
		return "", fmt.Errorf("download LSP archive: body exceeds %d bytes", maxLSPArchiveBytes)
	}

	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("close LSP archive: %w", err)
	}

	actualDigest := hex.EncodeToString(hash.Sum(nil))
	if actualDigest != expectedDigest {
		return "", fmt.Errorf("verify LSP archive: SHA-256 mismatch: got %s", actualDigest)
	}

	success = true

	return name, nil
}
