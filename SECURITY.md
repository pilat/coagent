# Security policy

## Supported versions

Security fixes target the latest released version and the current `main` branch.
Before the first release, only `main` is supported.

## Report a vulnerability privately

Do not open a public issue for a suspected vulnerability. Use GitHub's private
security-advisory form:

<https://github.com/pilat/coagent/security/advisories/new>

Include the affected version or commit, impact, reproduction steps and any known
mitigation. Please avoid accessing data that is not yours and allow maintainers
time to investigate before public disclosure.

Repository owners must enable GitHub private vulnerability reporting before the
first public release. If the private form is unavailable, do not publish exploit
details; open a minimal issue asking maintainers to enable the private channel.

The default filesystem-write sandbox is an integrity boundary, not a
confidentiality or multi-tenant boundary. With session shields down, tools and
session processes retain the daemon user's ordinary read access.

An operator may raise durable session shields to confine a complete session
tree's built-in file tools and session-owned processes to its project plus a
fixed read-only system execution substrate. This blocks those surfaces from
same-user files outside the project, host temporary storage, user caches, and
configured writable exceptions. Global and marketplace instruction sources
remain trusted daemon inputs outside this boundary; project-local instruction
sources use rooted project access. Shields do not restrict network egress,
inherited environment variables, project-data disclosure, prior model history,
or deliberately detached processes. System resolver, host-name, account,
loader, and certificate files remain readable when required by the execution substrate.
Shields require the native sandbox and cannot be lowered while the tree is
running. The complete boundary is defined in [ARCHITECTURE.md](ARCHITECTURE.md)
and [ADR-0044](docs/adr/0044-session-shields-confine-project-filesystem.md).
