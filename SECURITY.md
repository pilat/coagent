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

Default project confinement is an integrity boundary, not a confidentiality or
multi-tenant boundary. The host is readable and the project is writable.
Coagent ships no credential-path rules: the operator configures ordered
allow/deny rules globally and per project. A linked worktree inherits its source
project's rules. Session processes and built-in file tools enforce the same
compiled policy; `sandbox.enabled: false` disables confinement.
The project section ends with mandatory read-only access to its captured process
output, including for subagents; this exception exposes no other daemon
state or project output.

Project-local instructions use project-confined access, and marketplace
instructions stay within their own repository clone. Global instructions are
trusted daemon inputs. The filesystem policy does not restrict network egress,
inherited environment variables, prior model history, or access through an
allowed service socket. Stronger isolation belongs to the daemon's deployment.
The complete boundary is defined in [ARCHITECTURE.md](ARCHITECTURE.md)
and [the filesystem policy](docs/sandbox-boundary.md).
