# ADR-0091: The browser runs on a fixed model with operator-granted built-in tools

- **Status:** Accepted
- **Date:** 2026-11-11
- **Amends:** [ADR-0088](0088-browser-surface-is-the-configured-mcp-server.md)

## Context

ADR-0088 gave the `browser` subagent an exclusive view of the MCP server named by
`browser.mcp_name` and nothing else. The child ran on the model the parent passed to `task`, or on the
parent's model, at the parent's effort. The browser exists to save tokens. A root on an expensive model
therefore spent that model on clicking, and the browser could not even save collected data to a file
or read an input file.

## Decision

The `browser` section gains three daemon-wide optional keys: `model`, `effort` and `tools`.

- `browser.model` names an entry of `models:` and pins every new browser child to it. A `model` argument
  of `task` is ignored for browser, not refused. Without the key, today's resolution stays.
- `browser.effort` is passed as an explicit effort and requires `browser.model`. A level the model's
  catalog allowlist does not accept, including a model with no effort selector, fails the boot.
- `browser.tools` lists built-in tool IDs by their raw names. The child receives their intersection with
  a whitelist kept beside the browser agent-type definition in `registry`:
  `read, write, edit, apply_patch, ls, glob, grep, bash, tail, cancel_process, webfetch`.
  Every other name is ignored silently. Nothing is granted by default.
- Granted tools are not frames. The one-action rule, `task_state` and frame replacement still apply
  only to the bound server's calls. The root's `task` description lists the effective grants.

## Consequences

- A cheap model can do browser work regardless of the root's model, and the browser can exchange data
  with the root through files.
- An unrecognised tool name has no effect and gives no feedback. The README is the only list of
  accepted names.
- The whitelist exists once in code. A new built-in reaches the browser only when it is added there.
- `batch` is never grantable: its nested calls would bypass the one-action rule and its combined result
  would not be a frame.
- Granting `bash` gives an agent that reads untrusted web content arbitrary sandboxed commands,
  including network access. This is the operator's informed choice.
- **Accepted risk:** the catalog recomputes a model's allowlist on every boot. If a vendor later drops
  the configured level, the next restart fails and the daemon crash-loops until `config.yaml` is
  edited by hand. The `configops` rollback does not cover it, and the in-daemon Telegram manager cannot
  repair it. This knowingly departs from ADR-0086 for this one key.
- Existing browser children keep the model and effort they were spawned with.

## Alternatives Considered

- **Capability groups (`file_read`, `file_write`, `shell`).** Rejected: a second vocabulary to keep in
  step with the tools.
- **Reject unknown tool names in config.** Rejected: `config` cannot import `registry`, so the
  whitelist would exist twice.
- **Denylist instead of whitelist.** Rejected: every new built-in would silently reach a risky agent.
- **Refuse a `task` `model` argument for browser.** Rejected: it costs the root a turn and the outcome
  is the same.
- **Clamp an unaccepted effort to the nearest level.** Rejected twice by the owner. First because the
  model would silently run at a different level. Then as a split, with a hard error only for a level
  outside the gateway vocabulary and warn-and-clamp for catalog drift, after the crash-loop risk was
  shown.
- **Effort without a model, applied to the inherited parent model.** Rejected: an effort is meaningful
  only against a known model's allowlist.
