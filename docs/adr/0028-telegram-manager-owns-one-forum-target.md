# ADR-0028: One Telegram manager owns one bot-controlled forum target

- **Status:** Accepted
- **Date:** 2026-08-23

## Context

The Telegram manager currently uses one topic-enabled supergroup: one service
topic is the control surface and each root session receives its own topic.
Telegram now also supports forum topics in a private chat with a bot. Both
topologies use `message_thread_id` and the same topic-management methods, but
their setup and authority differ: a group requires an administrator bot, while a
private bot forum requires Threaded Mode and has exactly one user peer.

Manager ID is already the durable session-routing identity (ADR-0023). Repointing
that ID at another chat or bot would make existing sessions appear in a new
transport target. Allowing people to create private topics would invert the same
relationship: a Telegram topic would exist before coagent had a project or
session to bind to it. Reusing one bot token across configured managers is also
not independent operation because Telegram exposes one long-poll update stream
per bot.

We need group and private forums to coexist without duplicating Telegram manager
logic, silently transferring sessions, or introducing a shared bot runtime.

## Decision

One configured Telegram manager owns exactly one Telegram bot account, one
polling instance, and one immutable forum target.

- A present `target_chat_id` selects a group forum. An absent `target_chat_id`
  selects a bot forum whose effective chat ID is its single positive allowed
  user ID. No separate mode field is stored.
- Group and bot forums share the same service-topic, session-topic, and
  `message_thread_id` routing protocol after startup verifies their respective
  capabilities.
- Coagent controls topic lifecycle. Both topologies use a dedicated service
  topic; private users are configured not to create or delete topics. Telegram's
  General view is bootstrap guidance, never an implicit session.
- Manager ID, Telegram bot user ID, topology, and effective chat ID form the
  durable forum identity. A mismatch fails that manager before topic mutation or
  session reconciliation. Changing any identity member requires a new manager
  ID; topics and sessions are never moved between targets.
- Enabled managers must have distinct resolved bot tokens. Every member of a
  duplicate-token conflict remains stopped with a sanitized per-manager startup
  error; unrelated managers and the daemon continue.
- Onboarding recommends a private bot forum for one person and retains a group
  forum for shared use or as a compatibility fallback.

## Consequences

- A group manager and private manager can run simultaneously when they use
  distinct bots. Existing group configurations retain their behavior.
- Token rotation remains possible when `getMe` identifies the same bot account.
  A token for another bot does not move topics; the manager remains down until
  configuration is repaired or a new manager ID is created.
- The service-topic binding must persist manager and target identity, not only a
  chat-scoped topic ID. Legacy records need a crash-resumable one-time claim into
  the manager namespace.
- Native private `New Chat` cannot start work. A user first selects a project via
  the service topic; coagent creates the session and then its topic.
- Different Telegram managers do not share HTTP clients, offsets, backoff, bot
  commands, or polling lifecycle. This keeps failure and ownership boundaries
  aligned, at the cost of requiring another BotFather bot for another manager.
- Manager startup failure remains repairable through local chat and does not
  fail daemon startup. This decision does not add durable replay for session
  events published before a manager subscribes.

## Alternatives Considered

- **Keep group forums only.** Rejected because a private bot forum is a cleaner
  single-user surface and now exposes the topic operations coagent already uses.
- **Add an explicit `mode: group|private`.** Rejected because target presence
  already expresses the distinction without a second field that can disagree.
- **Pool one bot token across several managers.** Rejected because it creates a
  second bot-level runtime, dispatcher and shared failure boundary solely to
  preserve token reuse. Distinct managers intentionally remain distinct bot
  instances.
- **Let users create private topics and infer sessions from them.** Rejected
  because a topic contains no project identity; implicit creation would either
  guess a project or add another onboarding protocol inside every new topic.
- **Move existing sessions when a manager target changes.** Rejected because it
  violates durable manager routing identity and turns a configuration edit into
  remote topic migration and cleanup.
- **Use Telegram General as the service topic.** Rejected because its thread
  representation and UI differ by topology. A dedicated topic keeps command and
  session routing uniform.
