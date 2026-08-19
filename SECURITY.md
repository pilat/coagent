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

The filesystem sandbox is an integrity boundary, not a confidentiality or
multi-tenant boundary. Its supported guarantees and open egress/read paths are
defined in `ARCHITECTURE.md`.
