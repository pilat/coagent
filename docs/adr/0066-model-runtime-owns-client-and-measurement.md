# ADR-0066: Model runtime owns client lifetime and measurement validity

- **Status:** Accepted
- **Date:** 2026-09-28

## Context

The session receiver combined transcript execution with model configuration,
client replacement and provider-measured context size. Model switching already
used one mutex to protect its client, identity, generation and measurement.
Compaction and status consumed that state through methods on the entire session.
Separating only configuration or only context measurements would distribute the
invariant that a measurement belongs to the current model generation.

## Decision

A private model runtime in the session package owns client construction/setup,
lease-protected calls and close, model switching and generation-tagged context
measurements. It receives configuration, prompt/search projection, registry,
image-read authority and a narrow measurement-persistence capability explicitly.
It receives no session receiver or transcript.

The model lock continues to cover client replacement and measurement acceptance.
Calls hold their client lease throughout provider I/O. Model consumers request
operations and scalar metadata rather than obtaining the raw client. Ordinary
request measurements remain distinct from summarizer usage. Transcript reset
and successful compaction explicitly invalidate the owner's measurement after
replacement; persistence remains best-effort.

Close is terminal. A replacement can finish construction after session teardown
has closed the current client; it must be closed without installation. The
losing switch returns an error before daemon persists a changed model identity.

## Consequences

Model changes can be tested without unrelated session state. Session remains
responsible for transcript execution and coordinates model close before tool
resource release. The model owner must not acquire the transcript lock: the
existing compaction path already calls it while holding that lock.

Compaction and tool execution still require separate evaluation. This boundary
gives them a model capability without predetermining their eventual design.

## Alternatives Considered

- **Group fields but leave methods on session.** Retains access to unrelated
  state and leaves the same distributed mutation responsibility.
- **Separate measurement from model lifetime.** Introduces coordination between
  two locks for an invariant already protected by one.
- **Move the complete session into a new runtime object.** Preserves the same
  concentration under a new name and provides no independent capability.
