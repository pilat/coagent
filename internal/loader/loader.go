package loader

import (
	"context"

	"github.com/pilat/coagent/internal/config"
)

// Loader handles setup-time operations: loading artifacts from disk.
type Loader interface {
	ProcessMarketplace(ctx context.Context, entry config.MarketplaceEntry, resolver RepositoryResolver) error
	ProcessMarketplaces(ctx context.Context, entries []config.MarketplaceEntry, resolver RepositoryResolver)
	LoadAgentsMD(workDir string) (string, error)
	LoadSkills(workDir string) error
	LoadSubagents(workDir string) error
}

// Registry provides runtime access to loaded artifacts.
type Registry interface {
	GetSkill(name string) *Skill
	ListSkills() []*Skill
	ListUserInvocableSkills() []*Skill
	ListModelInvocableSkills() []*Skill
	RegisterSkill(skill *Skill)
	GetSubagent(name string) *Subagent
	ListSubagents() []*Subagent
	RegisterSubagent(subagent *Subagent)
}

// Service is the union of Loader and Registry. Used by code that holds the full object.
type Service interface {
	Loader
	Registry
}

var _ Service = (*svc)(nil)

type svc struct {
	// WARNING: map iteration is non-deterministic — all List*() methods MUST sort
	// results, otherwise tool descriptions change on every call and break LLM cache.
	skills    map[string]*Skill
	subagents map[string]*Subagent

	marketplaceSkillPaths []sourceInfo
	marketplaceAgentPaths []sourceInfo
	marketplaceCache      MarketplaceCache
}

// New creates a new loader service. Optional cache enables daemon-level marketplace caching.
func New(cache ...MarketplaceCache) Service {
	s := &svc{
		skills:    make(map[string]*Skill),
		subagents: make(map[string]*Subagent),
	}
	if len(cache) > 0 {
		s.marketplaceCache = cache[0]
	}

	return s
}
