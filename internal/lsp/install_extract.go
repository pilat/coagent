package lsp

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
)

// maxLSPExecutableBytes is mutable so tests can exercise the decompression bound
// without constructing a 128 MiB fixture.
var maxLSPExecutableBytes int64 = 128 << 20

func extractArtifact(archivePath, destination string, artifact releaseArtifact) error {
	out, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create staged LSP executable: %w", err)
	}

	success := false

	defer func() {
		_ = out.Close()

		if !success {
			_ = os.Remove(destination)
		}
	}()

	if err := extractArtifactTo(archivePath, out, artifact); err != nil {
		return err
	}

	if err := out.Sync(); err != nil {
		return fmt.Errorf("sync staged LSP executable: %w", err)
	}

	if err := out.Chmod(0o755); err != nil {
		return fmt.Errorf("chmod staged LSP executable: %w", err)
	}

	if err := out.Close(); err != nil {
		return fmt.Errorf("close staged LSP executable: %w", err)
	}

	success = true

	return nil
}

func extractArtifactTo(archivePath string, dst io.Writer, artifact releaseArtifact) error {
	switch artifact.kind {
	case archiveGzip:
		return extractGzip(archivePath, dst)
	case archiveTarGzip:
		return extractTarGzip(archivePath, artifact.entry, dst)
	case archiveZip:
		return extractZip(archivePath, artifact.entry, dst)
	default:
		return fmt.Errorf("unknown LSP archive kind %d", artifact.kind)
	}
}

func extractGzip(archivePath string, dst io.Writer) error {
	archive, err := os.Open(archivePath)
	if err != nil {
		return fmt.Errorf("open gzip archive: %w", err)
	}
	defer func() { _ = archive.Close() }()

	zr, err := gzip.NewReader(archive)
	if err != nil {
		return fmt.Errorf("open gzip payload: %w", err)
	}
	defer func() { _ = zr.Close() }()

	return copyExecutable(dst, zr, "gzip payload")
}

func extractTarGzip(archivePath, expected string, dst io.Writer) error {
	archive, err := os.Open(archivePath)
	if err != nil {
		return fmt.Errorf("open tar archive: %w", err)
	}
	defer func() { _ = archive.Close() }()

	zr, err := gzip.NewReader(archive)
	if err != nil {
		return fmt.Errorf("open tar gzip payload: %w", err)
	}
	defer func() { _ = zr.Close() }()

	return copyTarEntry(tar.NewReader(zr), expected, dst)
}

func copyTarEntry(tr *tar.Reader, expected string, dst io.Writer) error {
	found := false

	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}

		if err != nil {
			return fmt.Errorf("read tar archive: %w", err)
		}

		matches, err := exactArchiveEntry(header.Name, expected)
		if err != nil {
			return err
		}

		if !matches {
			continue
		}

		if found {
			return fmt.Errorf("archive contains duplicate entry %s", expected)
		}

		if header.Typeflag != tar.TypeReg {
			return fmt.Errorf("archive entry %s is not a regular file", expected)
		}

		if err := copyExecutable(dst, tr, "tar entry "+expected); err != nil {
			return err
		}

		found = true
	}

	if !found {
		return fmt.Errorf("archive entry %s not found", expected)
	}

	return nil
}

func extractZip(archivePath, expected string, dst io.Writer) error {
	archive, err := zip.OpenReader(archivePath)
	if err != nil {
		return fmt.Errorf("open zip archive: %w", err)
	}
	defer func() { _ = archive.Close() }()

	found := false

	for _, file := range archive.File {
		matches, err := exactArchiveEntry(file.Name, expected)
		if err != nil {
			return err
		}

		if !matches {
			continue
		}

		if found {
			return fmt.Errorf("archive contains duplicate entry %s", expected)
		}

		if !file.Mode().IsRegular() {
			return fmt.Errorf("archive entry %s is not a regular file", expected)
		}

		if err := copyZipFile(file, dst); err != nil {
			return err
		}

		found = true
	}

	if !found {
		return fmt.Errorf("archive entry %s not found", expected)
	}

	return nil
}

func copyZipFile(file *zip.File, dst io.Writer) error {
	src, err := file.Open()
	if err != nil {
		return fmt.Errorf("open zip entry %s: %w", file.Name, err)
	}
	defer func() { _ = src.Close() }()

	return copyExecutable(dst, src, "zip entry "+file.Name)
}

func copyExecutable(dst io.Writer, src io.Reader, label string) error {
	written, err := io.Copy(dst, io.LimitReader(src, maxLSPExecutableBytes+1))
	if err != nil {
		return fmt.Errorf("extract %s: %w", label, err)
	}

	if written > maxLSPExecutableBytes {
		return fmt.Errorf("extract %s: executable exceeds %d bytes", label, maxLSPExecutableBytes)
	}

	return nil
}

func exactArchiveEntry(name, expected string) (bool, error) {
	if name == expected {
		return true, nil
	}

	if path.Base(path.Clean(name)) == path.Base(expected) {
		return false, fmt.Errorf("archive target %s appears at unexpected path %s", expected, name)
	}

	return false, nil
}
