# ADR-0111: Operator hooks follow the Claude Code command contract

- **Status:** Accepted
- **Date:** 2026-11-25
- **Amends:** [ADR-0068](0068-one-completion-nudge-per-model-input.md)

## Context

Operators already have command hooks written for Claude Code. There is no
cross-vendor hook standard, and the Claude Code [hooks reference](https://code.claude.com/docs/en/hooks)
is the de facto contract those scripts consume. Matching a Claude tool spelling
without also sending that spelling in the payload fails open when a script
checks `tool_name` again. Coagent needs declared hook identities and compatible
inputs, while preserving its existing confinement and bounded completion check.
Copilot, Cursor, Codex, Grok and Qwen also map their hooks to that vocabulary.

## Decision

1. Support command handlers for `PreToolUse`, `PostToolUse` and `Stop` from
   `~/.coagent/settings.json`, `<workDir>/.claude/settings.json` and
   `<workDir>/.claude/settings.local.json`. Merge handlers in that order, deduplicate
   identical commands and arguments across files under the same event and matcher.
2. Keep global hook authority in `~/.coagent/settings.json`.
   Global `disableAllHooks: true` disables all hooks. Project and local values
   govern only project handlers, with local taking precedence over project;
   they cannot disable global guards. An effective disable logs `hooks_disabled`
   at info with its file path. For a project rooted at the user home, skip both
   project settings files with a debug log.
   Never read `~/.claude/settings.json`: the operator's existing interactive hooks
   would otherwise start unsolicited processes around coagent calls.
3. Snapshot settings once per activation, when session assembly rebuilds the
   session and its tools. Project edits reach the next activation. A durable
   session snapshot would require a migration and prevent new global guards from
   reaching existing long-lived roots.
4. Run commands with the session's confinement and shell environment. Read project
   settings through project-confined file access, refusing symlinks outside the
   project; read the global settings file as trusted host input.
   The hooks package cannot spawn by itself: the existing `exec-confined` lint
   rule keeps it off `os/exec`, so every hook process goes through the stack's
   confinement runner.
   `hooks` declares a run-to-completion `Process`/`Outcome`/`Launcher` seam;
   session assembly adapts the stack's process runner. The stack owns bounded
   64 KiB raw stdout/stderr capture, a five-second wait delay and process-group
   cancellation through the existing command-cancellation helper, so timeout or
   stop kills descendants too. It reuses `procexec` descriptor cleanup, including
   failed starts. Hooks own per-handler timeouts (600 seconds
   by default), parallel event dispatch, argv/environment construction and decisions.
   Shell form uses `bash -c`; exec form substitutes `${CLAUDE_PROJECT_DIR}` in
   command and arguments, and both forms export the canonical project directory.
5. Tools declare their Claude name, Codex and OpenCode aliases and optional response
   shape through traits. Native IDs win over aliases; no alias may shadow another
   tool's native ID. The hook payload carries the declared name and the source
   tool's native ID as `tool_name` and `coagent_tool_name`, respectively.
6. Project `apply_patch` as one Edit call per existing-file hunk and one Write
   call per new file, with exact hunk sides or planned file content. A refusal of
   any projection refuses the whole patch. Fix the projection before execution
   once at the shared invocation boundary; hooks consume that captured projection
   for both events. Empty or failed projections use the native identity. A newly
   created file remains Write in PostToolUse; PostToolUse runs per hunk.
7. Attach tool hooks at the shared executor's invocation boundary, including nested
   batch calls. A PreToolUse refusal is a typed tool failure and stops later stages.
   PostToolUse runs only after successful execution. Commit operator feedback after
   truncation and the untrusted frame, preserving its authority in the stored row.
   Deduplicate exact feedback strings in first-occurrence order per committed row,
   join them with blank lines and cap the combined text at 10,000 Unicode
   characters, including the `\n[hook feedback truncated]` suffix when truncated.
8. Extend the completion check with root-only Stop hooks at publication boundaries,
   after any ordinary completion nudge. A block clears the candidate, sets the same
   durable nudge marker and appends host feedback; the replacement answer must stand
   alone. Allow at most eight continuations per activation, including additional
   context. Neither tool calls nor human input reset the count. At the cap, publish
   without launching another hook; `stop_hook_active` reports an earlier continuation.
9. Treat PreToolUse `ask` and `defer` decisions as refusals: an unattended session
   has nobody to ask and no deferred permission caller. Honour `onFailure: "block"`
   only for PreToolUse; other handler failures log and let the operation continue.
   Output without both outer braces is plain text: exit 0 succeeds, exit 2 blocks
   with stderr, and other exits log a failure. Braced output that is not one valid
   JSON object is a parse failure: exit 2 blocks with stderr; all other exits,
   including 0, warn with `invalid JSON output` and obey PreToolUse `onFailure`.
   A valid JSON decision applies on any exit, except
   that PreToolUse `onFailure: "block"` treats exits other than 0 or 2 as failures.
   Stop's missing-script exception requires empty stdout and one trimmed stderr
   line containing `No such file or directory`; multiline stderr still blocks.

## Consequences

Common Claude Code command hooks work unchanged, with declared tool names and
compatible payloads. With no configured handlers, assembly returns no hook
interfaces: no command starts and no hook text enters the prompt. Existing
completion-check recovery remains authoritative; no migration or parallel Stop
protocol is introduced. The check still adds at most one model call per input
generation, plus at most eight Stop-hook continuations per activation for a root
with Stop hooks. The cap bounds continuation loops that ADR-0068 previously
ruled out unconditionally. The accepted limits are:

- Argument/output rewrites (`updatedInput`, `updatedToolOutput`, `updatedMCPToolOutput`)
  are ignored with a warning; the original call and result remain.
- Plugin hooks, non-command handlers and events beyond PreToolUse, PostToolUse
  and Stop, including SubagentStop and PostToolUseFailure, never fire.
- `~/.claude/settings.json` and managed settings are unread; operators must copy
  the desired hooks into `~/.coagent/settings.json`.
- A project rooted at the user home loads no project hooks, including its local
  file; global coagent hooks still apply.
- There are no coagent tool classes: an `Edit|Write` guard does not see Bash edits.
- `if` is ignored with a warning; a matching handler runs unconditionally.
- JavaScript-only regular expressions never match and produce a load warning.
- `transcript_path` is absent; transcript-reading Stop hooks fail open and log,
  while hooks using `last_assistant_message` work.
- Bash's response `stderr` is empty because its text is combined into `stdout`.
- Failed and suspended calls receive no PostToolUse event.
- `systemMessage` and `stopReason` reach logs only.
- PreToolUse/PostToolUse `continue: false` is ignored; their own decision still applies.
- `async` and `asyncRewake` handlers run inline and add latency, but their decisions
  and output are ignored.
- The Stop cap survives tool calls and human input within one activation; after
  eight continuations publication proceeds even if a guard would block again.
- Every activation starts a fresh Stop continuation count, whether a wake or a
  daemon restart began it.
- `/stop` or a restart while a Stop hook runs ends the step like a cancelled model
  call: the answer the hook was judging is not committed.
- The Stop payload carries no `background_tasks`, so a hook cannot tell a root
  yielding to background work from a finished one and may send it back to work.
- Hook text reaches the model as operator authority, outside the untrusted frame,
  even when the hook echoes text from a web page or another untrusted source:
  what a hook prints is its author's responsibility.
- A repository's own `.claude/settings.json` runs with the session's confined
  authority; there is no per-repository trust prompt.
- Hook stdout past 64 KiB is cut, so an oversized JSON decision no longer parses
  and the exit code alone decides.
- PreToolUse `additionalContext` is dropped when the tool then fails without a
  result.
- Tools that declare no hook name (`todowrite`, `skill`, `ls`, `lsp` and the
  control-plane tools) match only under their native IDs; a Codex-style hook
  reading `tool_input.patch` sees `apply_patch` only as Edit and Write calls.
- A project settings file that is not valid JSON warns at every activation; a
  failed settings load drops every hook for that activation with one warning.
- Absolute `file_path` values are cleaned rather than symlink-canonicalized.
- Empty or failed projections expose the source tool under its native identity;
  an unparseable patch then fails in ordinary execution.
- Project-settings edits take effect at the next activation. With
  `sandbox.enabled: false`, a session can also rewrite the global file as the
  daemon user; the changed guard takes effect at its next activation. Project
  settings alone cannot disable global guards.
- A project rooted at `~` or `~/.coagent`, or an operator rule granting write access
  to the global file or its parent, makes that file writable inside confinement.
  Other projects see the global file read-only under the implicit defaults.
- Matching handlers run in parallel per event and within parallel tool stages;
  up to four read-class calls can run their hooks concurrently.

## Alternatives Considered

- **Native tool IDs only:** existing Claude Code matchers and scripts would not
  recognize common calls.
- **Matcher aliases only:** scripts rechecking `tool_name` would still fail open.
- **A central name table:** tool traits keep identity with its declaration, as
  [ADR-0085](0085-core-names-no-extensible-tool-by-literal.md) requires.
- **Coagent-specific tool classes:** they add a second operator vocabulary without
  making an existing Claude Code script compatible.
- **Session-level dispatch hooks:** nested batch execution would bypass guards.
- **Read the operator's Claude home settings:** interactive hooks would fire without
  any coagent configuration.
- **Let project settings disable global hooks:** a guarded session could remove
  global operator policy merely by writing project settings.
- **Durable per-session snapshots:** they need a migration and retain stale global
  guards across activations of long-lived sessions.
- **Project whole-file edits or join patch hunks:** whole-file old/new strings
  create false positives, while joining disjoint hunks invents contiguous fields.
- **A separate Stop continuation protocol:** it duplicates completion-check state
  and recovery instead of reusing the durable nudge marker.
- **Reset the Stop cap on tool calls:** a hook/model loop could continue indefinitely.
- **Reuse mutation-error output capture:** its fixed 8 KiB limit and display
  truncation are unsuitable for raw hook JSON; hook streams retain their own
  64 KiB bounds.
