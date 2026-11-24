# ADR-0108: An app acts with delegated authority

- **Status:** Accepted
- **Date:** 2026-11-24
- **Amends:** [ADR-0097](0097-orchestrator-mode-is-a-human-granted-capability-on-an-ordinary-root.md)
- **Amends:** [ADR-0101](0101-an-orchestrator-ends-only-sessions-it-spawned.md)
- **Amends:** [ADR-0104](0104-the-local-api-is-the-two-durable-ledgers.md)
- **Amends:** [ADR-0106](0106-one-operator-every-surface-reaches-every-root.md)

## Context

ADR-0104 already names the app author: the same authority an operator delegates to an orchestrator session,
given to a program. It deferred serving that author. An external program now needs to create and drive roots,
follow their durable output, learn reliably when a turn is over, and stop or kill work it started without gaining
the operator's configuration or session-setting authority. Some delegations must reach only named projects.
ADR-0106's keys shared across every client also let two independent callers suppress each other's input.

## Decision

An API client has role `operator` or `app`. The composition root binds an operator controller or an app controller
before passing it to `localapi`; both use the same `coagent.v1` RPCs. The transport authenticates the author,
holds no role table, reads no role and inspects no input text. The app controller has a named operator-controller
field and explicitly implements every capability method. Interface growth therefore fails to compile until its
app authority is decided; it cannot silently inherit operator authority.

An app's optional `projects` lists unique, clean absolute project work directories. An empty list reaches every
project; paths need not exist when config loads. Project scope is applied to all calls naming a project or root,
including reads, and filters lists. It covers a root by its registered work directory or by a listed `repo_root`
when `coagent_worktree_origin` attests `controller-created-v1`. A root outside the project scope reads exactly as
a missing root, `session N not found`. This limits a delegation in a single-tenant daemon and adds no session
ownership or per-surface visibility rule. Managers and operator clients still reach every root.

App creation shares the orchestrator spawn flow: an existing visible registered project whose directory exists
and is not itself a coagent worktree, capacity headroom, optional fresh worktree, and the default or a tagged
model at that model's default effort. The root is created empty. An unknown or out-of-project-scope directory
returns `no project at "<dir>"`, revealing no project list. Its host-written `spawned_by_app` string client id is
reserved from request attributes and survives `/clear`, alongside the existing `spawned_by` session id contract.

The shared daemon send stamps `source=agent` and host-authored `sender_app` and `sender_name` attributes. It arms
no watch and publishes no `NotifyInputReceived`, since an app follows output rather than keeping a chat or inbox.
The model sees `[Message from the program "<id>" — sent through the local API, not by the human]` followed by
the message; the persist-only chat row is `↪ from app <id>` followed by the message. Commands, skill expansion,
activation grants and budget release remain unavailable in app text. An inserted sent message from the creating
app names an untitled root in the enqueue transaction when its text yields a title. A message beginning with `/`
leaves it untitled, so a later message can name it; a `/clear` successor is eligible again. Duplicate keys write
nothing, and existing titles are preserved. App-started roots are ordinary roots in `/sessions`.

A project-scoped app cannot send to a root whose newest `/clear` replacement has orchestrator mode on; reads
stay allowed, and an unscoped app may send. Apps cannot set existing models, titles or orchestrator mode, create
projects or call `ChatService`. The app controller explicitly refuses the seven chat-state methods.

`StopSession`, `KillSession` and `GetTurnOutcome` are additive `SessionService` RPCs for both authors. Operator
stop and kill enqueue human `/stop` and `/kill` input. The app shares the orchestrator's end flow, requiring the
newest target's `spawned_by_app` to equal its client id. Only the host writes these two fixed `source=user`
commands, carrying app provenance. Acknowledgements append `— asked by app <id>`; watcher signals say
`was stopped by app <id> with StopSession` or `was killed by app <id> with KillSession`, with chat notices
`was stopped by app <id>` or `was killed by app <id>`. Session actors' behavior is unchanged.

`GetTurnOutcome` returns the newest root id and `over`, derived on demand from durable rows and runner state
using watcher outcome semantics together with the admission queue. Running, waiting or stopping work is not over;
parked, killed and terminating roots are over. An errored root with no active runner and no admission-queue entry
is over even if input from the failed run remains pending. Agent or schedule input can wake an errored root;
while it waits for admission, pending input keeps `over` false. Other roots are over when they have no pending
input and no background wake source, including a root that has never had input. No state string, output-log row
type or migration is added.

Idempotency keys are per root and author. `managercontrol` prefixes a nonempty key with `<author>:`; the daemon
prefixes the input class, storing `user:<author>:<key>` or `app:<author>:<key>`. Empty keys stay unkeyed, including
stop and kill. Same-author retries append nothing; two authors using the same key append twice. API client ids
exclude colons, so colons inside a key do not blur its author. Telegram still generates a fresh random key per
input. Stored pre-upgrade keys remain untouched, and the retried-`/clear` behavior from ADR-0104 is unchanged.

## Consequences

- Programs can drive sessions with the operator's delegated authority through the existing durable input and
  output ledgers. No second session protocol, tenant rule, migration or table is introduced.
- Wake hints are not filtered by project scope: `StreamSessionEvents` exposes every root's activity ids, no content.
- A listed project that is a repository subdirectory does not cover that repository's worktrees. The app can
  create a worktree root there and then receive `session N not found` when addressing it.
- A symlinked path must be listed as the daemon resolved it: `filepath.Abs` of the registered work directory and
  git's `repo_root` for a worktree.
- Sessions keep the host-readable sandbox default. Project scope limits API reach, not what session tools can
  read on the host, including another client's plaintext token file as ADR-0104 already accepts.
- The app controller's project-scope and orchestrator-mode checks precede the daemon enqueue. Turning mode on
  in the same instant after the check can let one message through; this sub-second window is accepted.
- In the sub-second window between enqueue and the wake goroutine reaching `start()`, an errored root may report over before entering the admission queue.
- An external operator client retrying a pre-upgrade `user:<key>` across the upgrade can append once more; old keys remain unchanged.
- Config validation is the only guard preventing an unknown role from binding operator capability in `cmd/coagent/local_api.go`.
- Manager ids are not format-validated. A colon in one could make its stored author/key prefix ambiguous with
  another manager's; configured ids are short words.
- A sleep interrupted by an app message retains the `source=agent` notice, "another session sent a message".
- Shared verb refusals retain orchestrator wording, such as `kill_session` and `only the human can resume it`.
  For an app, `kill_session` means `KillSession`.
- `ListModels` exposes every configured model to an app; a `CreateSession` model refusal lists tagged candidates.
- App stop and kill responses report only the resolved root, without distinguishing "nothing to stop" or
  "already killed". The output tail and `GetTurnOutcome` show the effect.

## Alternatives Considered

- **A separate `OrchestratorService`.** It would duplicate create, send, list and read paths while an app still
  needs `OutputService` for tails; it adds nothing for operators.
- **A role switch inside `localapi`.** That gives transport a second authority policy. Binding explicit
  controller capabilities keeps the authority decision with application use cases.
- **Project scope by project name or id.** Names can be ambiguous and ids are installation-specific; work
  directories are the existing project key and make the worktree rule explicit.
- **A durable outcome row.** It needs a migration for the output type CHECK and every chat must skip it.
  Reading the existing durable obligations and runner state suffices.
- **`GetSessionState`.** Session state already names the running/idle/error runtime value; turn outcome is the
  glossary's term for the end of a root's work for now.
- **Allow project-scoped apps to send to orchestrators.** An orchestrator reaches every project, widening that
  delegation indirectly; the newest-root mode check refuses the send instead.
- **Leave app-started roots untitled.** An empty create has no prompt, so those roots remain anonymous in chats.
  Naming from an inserted message by the creating app while untitled matches spawn naming without overwriting human titles.
