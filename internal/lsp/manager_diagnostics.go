package lsp

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/logger"
)

func (m *manager) GetDiagnostics(ctx context.Context, workDir, file string) ([]Diagnostic, error) {
	log := logger.Ctx(ctx).Named("lsp.manager")

	identity, err := resolveFile(workDir, file)
	if err != nil {
		return nil, err
	}

	cl, err := m.getClient(ctx, workDir, identity.path)
	if err != nil {
		return nil, err
	}

	document, err := cl.syncFile(ctx, identity.path)
	if err != nil {
		return nil, fmt.Errorf("sync file: %w", err)
	}

	diagnostics, err := cl.awaitDiagnostics(ctx, document)
	if err != nil {
		return nil, err
	}

	log.Info("GetDiagnostics",
		zap.String("file", file),
		zap.String("uri", identity.uri),
		zap.Int("count", len(diagnostics)),
	)

	if len(diagnostics) > 0 {
		log.Debug("GetDiagnostics: errors", zap.Any("diagnostics", diagnostics))
	}

	return diagnostics, nil
}

func (m *manager) GetAllDiagnostics(
	ctx context.Context,
	workDir string,
	maxErrorsPerFile, maxFiles int,
) []FileDiagnostics {
	if maxErrorsPerFile <= 0 || maxFiles <= 0 {
		return nil
	}

	absWorkDir, err := filepath.Abs(workDir)
	if err != nil {
		return nil
	}

	result, totalErrors := m.collectDiagnostics(filepath.Clean(absWorkDir), maxErrorsPerFile, maxFiles)
	log := logger.Ctx(ctx).Named("lsp.manager")
	log.Info("GetAllDiagnostics",
		zap.Int("filesWithErrors", len(result)),
		zap.Int("totalErrors", totalErrors),
		zap.Int("maxFiles", maxFiles),
	)

	if len(result) > 0 {
		log.Debug("GetAllDiagnostics: files", zap.Any("files", result))
	}

	return result
}

func (m *manager) collectDiagnostics(workDir string, maxErrorsPerFile, maxFiles int) ([]FileDiagnostics, int) {
	m.mu.RLock()

	clients := make([]*client, 0, len(m.clients))
	for _, cl := range m.clients {
		if !cl.hasExited() && insideWorkDir(workDir, cl.rootPath) {
			clients = append(clients, cl)
		}
	}

	m.mu.RUnlock()

	byPath := diagnosticsByPath(clients, workDir, maxErrorsPerFile)

	paths := make([]string, 0, len(byPath))
	for path := range byPath {
		paths = append(paths, path)
	}

	sort.Strings(paths)

	result := make([]FileDiagnostics, 0, min(maxFiles, len(paths)))
	totalErrors := 0

	for _, path := range paths {
		if len(result) == maxFiles {
			break
		}

		diagnostics := byPath[path]
		if len(diagnostics) > maxErrorsPerFile {
			diagnostics = diagnostics[:maxErrorsPerFile]
		}

		totalErrors += len(diagnostics)
		result = append(result, FileDiagnostics{Path: path, Diagnostics: diagnostics})
	}

	return result, totalErrors
}

func diagnosticsByPath(clients []*client, workDir string, maxErrors int) map[string][]Diagnostic {
	byPath := make(map[string][]Diagnostic)

	for _, cl := range clients {
		if cl.hasExited() {
			continue
		}

		for uri, diagnostics := range cl.getAllDiagnostics() {
			path, err := filePathFromURI(uri)
			if err != nil || !insideWorkDir(workDir, path) {
				continue
			}

			for _, diagnostic := range diagnostics {
				if diagnostic.Severity == 1 && len(byPath[path]) < maxErrors {
					byPath[path] = append(byPath[path], diagnostic)
				}
			}
		}
	}

	return byPath
}

func insideWorkDir(workDir, path string) bool {
	rel, err := filepath.Rel(workDir, path)

	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
