package lsp

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

type fileIdentity struct {
	path string
	uri  string
}

func resolveFile(workDir, file string) (fileIdentity, error) {
	if workDir == "" {
		return fileIdentity{}, errors.New("empty work directory")
	}

	absWorkDir, err := filepath.Abs(workDir)
	if err != nil {
		return fileIdentity{}, fmt.Errorf("resolve work directory: %w", err)
	}

	absWorkDir = filepath.Clean(absWorkDir)

	info, err := os.Stat(absWorkDir)
	if err != nil || !info.IsDir() {
		return fileIdentity{}, fmt.Errorf("work directory is not an existing directory: %s", absWorkDir)
	}

	if file == "" {
		return fileIdentity{}, errors.New("empty file path")
	}

	absFile := file
	if !filepath.IsAbs(absFile) {
		absFile = filepath.Join(absWorkDir, absFile)
	}

	absFile = filepath.Clean(absFile)

	rel, err := filepath.Rel(absWorkDir, absFile)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fileIdentity{}, fmt.Errorf("file path escapes work directory: %s", file)
	}

	return fileIdentity{path: absFile, uri: fileURI(absFile)}, nil
}

func fileURI(absPath string) string {
	return (&url.URL{Scheme: "file", Path: filepath.ToSlash(absPath)}).String()
}

func filePathFromURI(value string) (string, error) {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "file" || parsed.Host != "" && parsed.Host != "localhost" ||
		parsed.RawQuery != "" ||
		parsed.Fragment != "" {
		return "", fmt.Errorf("invalid file URI: %s", value)
	}

	if parsed.Path == "" || !filepath.IsAbs(filepath.FromSlash(parsed.Path)) {
		return "", fmt.Errorf("invalid file URI path: %s", value)
	}

	return filepath.Clean(filepath.FromSlash(parsed.Path)), nil
}
