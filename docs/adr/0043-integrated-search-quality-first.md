# ADR-0043: Integrated web search is quality-first — no keyless scraping default

- **Status:** Accepted
- **Date:** 2026-09-05

## Context

Coagent sessions had no builtin web search; search arrived only through user-configured MCP servers (typically Tavily MCP). Every competing agent ships integrated search, and the absence was a top friction point for new users.

The exploration for this feature tested every keyless option live. DuckDuckGo's HTML endpoint returns full SERP results keylessly (and robots.txt on that host allows crawling), but it is unofficial, degrades to captchas from datacenter IPs, and sits in a ToS gray zone. No ToS-clean keyless SERP exists in 2026: Bing's API is dead (HTTP 410), Brave killed its free tier, Mojeek's API is paid, Marginalia's public key serves a small non-commercial index with frequent rate limits. The only robust zero-config search is provider-hosted: OpenRouter's `openrouter:web_search` server tool gives any model on OpenRouter real search, billed to the OR credits the user already has.

The maintainer initially wanted zero-config first with quality as an upgrade path, then explicitly reprioritized: product quality wins, and zero-config survives only where it does not fight quality.

## Decision

Integrated search is quality-first:

1. A builtin `websearch` tool speaks to a user-configured REST provider (Tavily, SearXNG) and is registered only when configured — an unconfigured session has no search tool at all, with a human-facing notice instead of a model-facing dead tool.
2. OpenRouter's native server tool is injected by default for OR-driver models under a hard per-request cap (`max_tool_calls` = 5).
3. There is no keyless scraping default. An error or captcha from a search provider surfaces as an actionable message pointing at `tools.search`, never as degraded results.

## Consequences

Users on OpenRouter get quality search with zero additional signup; users elsewhere configure a keyed provider (Tavily free tier: 1,000 credits/month, no card) or their own SearXNG instance. The project never ships scraping logic against search engines — its ToS posture stays clean and there are no scrapers to repair when markup changes. Native searches are billed to the user's existing OR credits (accepted as part of default-on). The empty state is discoverable (daemon notice + `coagent status`), not silent. MCP search servers remain fully supported and unaffected.

The quality-over-reach posture also means: failover chains and a project-operated hosted search endpoint were rejected; if a search provider refuses, the correct outcome is a visible upgrade path, not silent degradation.

## Alternatives Considered

- **DuckDuckGo HTML scraping as the default.** Works today (verified live), robots-allowed, but unofficial, captcha-prone from datacenter IPs, and degrades without notice. Rejected under the quality-first priority after the maintainer flagged the ToS concern.
- **No default at all with onboarding-time setup.** ToS-clean but abandons zero-config entirely; rejected as worse UX than an honest empty state.
- **Project-operated hosted search endpoint (the Cursor model).** The only robust keyless option, but it drags infrastructure, abuse surface, and running costs into a self-hosted OSS project. Rejected.
- **Failover chains across providers.** More resilient but a mini-framework (quotas, retries, provider diagnostics) for a v1. Rejected; one active mechanism with honest errors.
