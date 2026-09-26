# ADR-0066: Optional developer profiles use escalated grants

- **Status:** Accepted
- **Date:** 2026-09-25
- **Extends:** [ADR-0063](0063-default-project-confinement-with-tool-profiles.md)

## Context

The original shell, mise, Git, SSH and gh profiles provide a small reviewed
default. The catalog now covers many more developer tools, including shared
package caches, credential files and daemon sockets. In the existing policy
model, every `basic` entry of every catalog profile is active for every
sandboxed project. Merely adding a `basic` entry would therefore broaden all
sessions, even when no operator has selected the new tool.

## Decision

All entries of the additional developer profiles are `escalated`. A profile
exists in the catalog but contributes no mount, socket or network grant until
the operator names it globally or for an exact project root. Raised shields
still remove those entries. The five original profiles keep their current
basic and escalated entries.

This uses the existing operator escalation set instead of adding a second
activation schema. Where a tool creates state on first use, an explicitly
selected profile may grant a scoped tool-owned directory read-write; credential
files and powerful daemon sockets are called out in the profile documentation.

## Consequences

- Shipping a new profile changes no existing project's authority.
- Operators opt in to a tool's reviewed shared state and credentials together;
  a narrower or relocated grant requires a replacement profile.
- A future change from `escalated` to `basic` would expose that entry to every
  sandboxed project. Such a change requires its own security review or a
  separate activation mechanism, not a catalog-only edit.

## Alternatives Considered

- **Add a profile activation field.** This would separate activation from entry
  level but add configuration and policy states without a current use case.
- **Publish templates outside the runtime catalog.** This keeps them dormant
  but requires copying definitions into configuration before they can be used.
- **Mark harmless caches basic.** Even shared writable caches can affect other
  projects and would become ambient authority immediately.
