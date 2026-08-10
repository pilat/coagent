# ADR-0009: System-scope daemon, user-home binary, sudo-free updates

- **Status:** Accepted
- **Date:** 2026-08-09

## Context

The daemon must start at boot and survive logout, but it is per-user to the bone: control socket, config, and secrets all live under `~/.coagent`. The install layout has to reconcile three forces: boot-time autostart (classically a system service), a binary the user updates often (onboarding replaces it on every version skew), and an update path that must not ask for sudo every time. The previous layout — binary in `/usr/local/bin`, plus a parallel user-scope mode (`--user` flag, user units, linger) — failed both ends: system scope demanded root on every binary update, and user scope didn't start until login unless linger was enabled, an account-wide switch that can require admin auth over SSH.

## Decision

One install mode per platform, system scope only, with the binary in the user's home:

- **Binary**: `~/.local/bin/coagent` on both platforms. Owned and writable by the user.
- **Linux**: system unit `/etc/systemd/system/coagent.service` with `User=<login>`, `WantedBy=multi-user.target`.
- **macOS**: LaunchDaemon `/Library/LaunchDaemons/` plist (root:wheel, 0644) with `UserName=<user>`, with KeepAlive/retry because `/Users/<user>` is unreadable until FileVault unlock.
- **Install** (the only privileged step): the CLI auto-escalates by re-exec'ing itself under `sudo` to write the unit/plist and enable the service.
- **Update**: replace the binary in place (atomic rename, no privileges), then a control-socket restart op makes the daemon drain and `syscall.Exec` itself — the exec path resolved at boot now holds the new binary. No systemctl, no sudo.
- User scope is deleted entirely: no `--user` flag, no user units, no LaunchAgents, no linger handling.

The daemon process never runs as root — `User=`/`UserName=` drop it to the owning user, so the user-writable binary crosses no privilege boundary.

## Consequences

- Updates and daemon restarts need no privileges, ever; sudo appears exactly once per machine, at install.
- Boot-time autostart works without linger or login on Linux; on macOS the daemon starts as soon as FileVault unlocks.
- The unit/plist can drift: binary updates never rewrite it, so a template change in a new version sits unapplied until someone reruns `sudo coagent daemon install`. The update path must render-and-compare, warning when the on-disk unit is stale.
- The install code sheds a whole axis: one manager per platform, no scope branching, no sudo-or-fallback logic on update.
- `sudo coagent daemon install` writes into `~/.local/bin` as root, so the installer must chown the binary to the target user (`SUDO_USER`).
- macOS Background Task Management will list an ad-hoc-signed home-dir binary as an unattributed background item — accepted cosmetics for a self-hosted tool.

## Alternatives Considered

- **User scope + linger (previous Linux mode)**: user unit starts only at login unless `loginctl enable-linger` is on; linger is an account-wide behavior change and may require admin auth in non-local sessions. Rejected as onboarding friction disguised as simplicity.
- **Binary in `/usr/local/bin` (previous system mode)**: every binary update needs root — the exact failure that triggered this decision (`create temp file next to /usr/local/bin/coagent: permission denied`).
- **macOS LaunchAgent instead of LaunchDaemon**: zero sudo ever, but dies at logout and never starts on a headless Mac; rejected to keep one system-scope story on both platforms.
- **Root-owned privileged helper (Docker Desktop pattern)**: solves a problem coagent doesn't have — nothing in the daemon needs root at runtime.
