package loader

import (
	"maps"
	"slices"
	"time"
)

// marketplaceCacheEntry is immutable once stored: a re-scan clones it, so readers
// holding an older pointer never see a half-appended source list.
type marketplaceCacheEntry struct {
	repoPath string
	skills   []sourceInfo
	agents   []sourceInfo
	scanned  map[string]struct{} // plugin names already scanned against repoPath
	err      error               // set on a failed resolve; the entry is then the negative cache
	lastPull time.Time
}

// cacheResult is what a fresh cache entry answers with — including a remembered failure.
type cacheResult struct {
	skills []sourceInfo
	agents []sourceInfo
	err    error
}

// covers reports whether the entry was already scanned for every requested plugin.
func (e *marketplaceCacheEntry) covers(plugins []string) bool {
	for _, p := range plugins {
		if _, ok := e.scanned[p]; !ok {
			return false
		}
	}

	return true
}

func (e *marketplaceCacheEntry) clone() *marketplaceCacheEntry {
	cloned := &marketplaceCacheEntry{
		repoPath: e.repoPath,
		skills:   slices.Clone(e.skills),
		agents:   slices.Clone(e.agents),
		scanned:  maps.Clone(e.scanned),
		lastPull: e.lastPull,
	}

	if cloned.scanned == nil {
		cloned.scanned = make(map[string]struct{})
	}

	return cloned
}

func filterByPlugins(cached *marketplaceCacheEntry, plugins []string) ([]sourceInfo, []sourceInfo) {
	wanted := make(map[string]struct{}, len(plugins))
	for _, p := range plugins {
		wanted[p] = struct{}{}
	}

	var skills []sourceInfo

	for _, s := range cached.skills {
		if _, ok := wanted[s.pluginName]; ok {
			skills = append(skills, s)
		}
	}

	var agents []sourceInfo

	for _, a := range cached.agents {
		if _, ok := wanted[a.pluginName]; ok {
			agents = append(agents, a)
		}
	}

	return skills, agents
}
