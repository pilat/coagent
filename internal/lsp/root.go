package lsp

import (
	"os"
	"path/filepath"
)

type markerKind uint8

const (
	markerFile markerKind = iota
	markerDirectory
	markerEither
)

type rootMarker struct {
	name    string
	pattern bool
	kind    markerKind
}

func exactFile(name string) rootMarker { return rootMarker{name: name, kind: markerFile} }
func exactDir(name string) rootMarker  { return rootMarker{name: name, kind: markerDirectory} }
func filePattern(name string) rootMarker {
	return rootMarker{name: name, pattern: true, kind: markerFile}
}

func findNearestRoot(workDir, startFile string, names []string) string {
	markers := make([]rootMarker, 0, len(names))
	for _, name := range names {
		if name != "" && name[0] == '*' {
			markers = append(markers, filePattern(name))
			continue
		}

		markers = append(markers, exactFile(name))
	}

	return findNearestRootMarkers(workDir, startFile, markers)
}

func findNearestRootMarkers(workDir, startFile string, markers []rootMarker) string {
	identity, err := resolveFile(workDir, startFile)
	if err != nil {
		return ""
	}

	dir := filepath.Dir(identity.path)
	for {
		if containsRootMarker(dir, markers) {
			return dir
		}

		rel, err := filepath.Rel(workDir, dir)
		if err != nil || rel == "." {
			return workDir
		}

		dir = filepath.Dir(dir)
	}
}

func containsRootMarker(dir string, markers []rootMarker) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}

	for _, marker := range markers {
		for _, entry := range entries {
			match := entry.Name() == marker.name
			if marker.pattern {
				match, _ = filepath.Match(marker.name, entry.Name())
			}

			if !match || !matchesMarkerKind(entry, marker.kind) {
				continue
			}

			return true
		}
	}

	return false
}

func matchesMarkerKind(entry os.DirEntry, kind markerKind) bool {
	if kind == markerEither {
		return true
	}

	if kind == markerDirectory {
		return entry.IsDir()
	}

	return !entry.IsDir()
}
