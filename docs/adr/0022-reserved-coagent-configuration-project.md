# ADR-0022: A reserved logical project identifies the coagent configuration session

- **Status:** Accepted
- **Date:** 2026-08-19

## Context

Daemon-wide configuration tools must exist in one durable conversation and nowhere else. A numeric session ID is unsuitable because clearing can recreate a session. The transport attribute `channel=cli` is also too broad: it describes how a message arrived, not what authority the conversation has, and future local project chats must not inherit configuration authority.

The configuration conversation still needs a project directory, while project identity is normally derived from a user-controlled folder basename. A filesystem-only sentinel can therefore collide with an ordinary project.

## Decision

- The configuration conversation belongs to the reserved logical project `sys:coagent`, stored in the directory `sys_coagent`.
- `:` is reserved for system project names and rejected in every user project name. User project creation also rejects the exact `sys_coagent` directory name.
- Only the built-in CLI manager can create or open this project, using an explicit private-controller marker. The daemon requires both its durable logical name and the exact canonical `<projects_root>/sys_coagent` path; the marker cannot designate another directory. This pair, not the channel or numeric session ID, gates provider, model and manager tools.
- Configuration tools are registered only on the project's root session. Subagents never inherit them.
- `request_secret` and automatic onboarding-skill activation require both the reserved project identity and `channel=cli`, because their protocol depends on a person attached to the terminal.
- The system project is omitted from ordinary recent-project listings.

## Consequences

- A future local chat for an ordinary code project can use the CLI transport without receiving daemon-wide authority.
- Clear/recreation preserves the authority because the replacement session retains its project identity.
- Logical and filesystem names deliberately differ, so every internal creation path must carry the system marker and prove the canonical path instead of reconstructing authority from a basename.
- This is a tool-registration boundary inside a same-user application, not a confidentiality boundary. The daemon user and unrestricted coding tools retain their documented filesystem and control-socket access.

## Alternatives Considered

- **Any CLI root.** Rejected because transport is not authority and would grant configuration tools to future local project chats.
- **A hard-coded numeric session ID.** Rejected because session clear/recreation changes it and database IDs are storage details.
- **The directory basename or logical name alone.** Rejected because either value can be supplied outside the configured project root; authority requires the reserved pair at the canonical path.
- **A new database project-kind column.** Viable, but unnecessary while a reserved logical namespace provides an explicit durable identity without a migration.
