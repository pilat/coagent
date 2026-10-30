package loader

// Service loads project artifacts and provides runtime access to them.
type Service struct {
	// WARNING: map iteration is non-deterministic — all List*() methods MUST sort
	// results, otherwise tool descriptions change on every call and break LLM cache.
	skills    map[string]*Skill
	subagents map[string]*Subagent

	marketplaceSkillPaths []sourceInfo
	marketplaceAgentPaths []sourceInfo
	marketplaceCache      *MarketplaceCache
}

// New creates a new loader service. Optional cache enables daemon-level marketplace caching.
func New(cache ...*MarketplaceCache) *Service {
	s := &Service{
		skills:    make(map[string]*Skill),
		subagents: make(map[string]*Subagent),
	}
	if len(cache) > 0 {
		s.marketplaceCache = cache[0]
	}

	return s
}
