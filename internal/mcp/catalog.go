package mcp

import (
	"encoding/json"
	"sort"
	"time"
)

// ToolMeta is one cached MCP tool's model-facing metadata: the projection a
// coagent tool.Tool needs. Schema bytes are a defensive copy of the client's
// serialized input schema.
type ToolMeta struct {
	Name        string
	Description string
	Schema      json.RawMessage
}

// Catalog is a server's cached tool metadata, keyed by the same resolved
// ServerConfig hash as its live client. It is plain data: it must never retain
// a client, transport, subprocess handle, cancel function, or context.
type Catalog struct {
	name     string // human-readable server name (first discovery), for logs
	tools    []ToolMeta
	lastUsed time.Time
}

// Tools returns the cached descriptors, ordered by tool name.
func (c *Catalog) Tools() []ToolMeta {
	if c == nil {
		return nil
	}

	return c.tools
}

// newCatalog copies the client's discovered tool metadata into an immutable
// catalog. A schema that fails to serialize degrades to an empty object schema
// like the live projection does, so the tool stays advertised.
func newCatalog(name string, client *Client, now time.Time) *Catalog {
	names := make([]string, 0, len(client.tools))
	for toolName := range client.tools {
		names = append(names, toolName)
	}

	sort.Strings(names)

	tools := make([]ToolMeta, 0, len(names))

	for _, toolName := range names {
		meta := ToolMeta{
			Name:        toolName,
			Description: client.tools[toolName].Description,
		}

		schema, err := client.ToolSchema(toolName)
		if err != nil || len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object"}`)
		}

		meta.Schema = append(json.RawMessage(nil), schema...)
		tools = append(tools, meta)
	}

	return &Catalog{name: name, tools: tools, lastUsed: now}
}

func (c *Catalog) touch(now time.Time) {
	c.lastUsed = now
}

// reapCatalogsLocked removes catalogs idle past their TTL and name→hash
// associations whose hash has neither a live entry nor a catalog left — no
// invalidatable state remains for those. Must be called with p.mu held.
// Returns the expired catalogs' names so callers can log outside p.mu.
func (p *pool) reapCatalogsLocked(now time.Time) []string {
	var expired []string

	for hash, cat := range p.catalogs {
		if now.Sub(cat.lastUsed) > p.catalogTTL {
			expired = append(expired, cat.name)

			delete(p.catalogs, hash)
		}
	}

	for name, hashes := range p.names {
		for hash := range hashes {
			_, hasEntry := p.entries[hash]
			_, hasCatalog := p.catalogs[hash]

			if !hasEntry && !hasCatalog {
				delete(hashes, hash)
			}
		}

		if len(hashes) == 0 {
			delete(p.names, name)
		}
	}

	return expired
}
