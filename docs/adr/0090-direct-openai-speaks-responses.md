# ADR-0090: The openai driver speaks the Responses API to api.openai.com

- **Status:** Accepted
- **Date:** 2026-11-10

## Context

The `openai` driver is one Chat Completions client for every OpenAI-compatible endpoint: api.openai.com,
DeepSeek, GLM, vLLM and local servers. Direct OpenAI models fail on their first turn because the client sends
`max_tokens`, which current OpenAI models reject. Renaming the field is not enough. OpenAI's Chat Completions
does not support tool calling with a reasoning effort other than `none` from GPT-5.4 on, and GPT-6.1 Sol needs
the Responses API for any tool call. Every coagent turn carries tools. The same models work through the
`openrouter` driver only because OpenRouter translates the request for us. Third-party endpoints have to keep
today's wire unchanged, and some older local servers reject fields they do not know.

## Decision

A provider on the `openai` driver whose `base_url` host is exactly `api.openai.com` gets a separate client that
talks to `POST {base_url}/responses`. Every other `openai` provider keeps the Chat Completions client byte for
byte. One predicate decides this for both client construction and the catalog effort allowlist. The driver name,
the config schema and model listing stay the same.

The client is stateless. It always sends `store: false`, it requests `reasoning.encrypted_content` for reasoning
models, and it resends the whole history from coagent's transcript. Reasoning continuity across tool turns
uses the existing sealed `ReasoningRaw` envelope. The envelope stores an ordered skeleton of the response output:
reasoning and message items verbatim, function calls as `{id, call_id}` references. Function calls are rebuilt
from the transcript's tool calls when the request is made. An envelope that does not fit the transcript is
dropped, and the turn is sent without reasoning or item ids. A reasoning item is never sent without the item that
followed it. The client does not stream, does not request reasoning summaries, and sends tools with
`strict: false`.

## Consequences

- Direct OpenAI models get tool calling, reasoning effort (offered in the picker from the catalog) and reasoning
  continuity. DeepSeek, GLM, local servers and OpenRouter are untouched.
- `driver: openai` now covers two wires. The glossary's "one protocol end to end" for a driver no longer holds
  literally.
- Detection is host-based. A proxy in front of OpenAI on a different host stays on Chat Completions. Azure
  OpenAI is not covered: it never worked with this client's auth.
- Every request resends the full history and the encrypted reasoning blobs. That costs bandwidth and storage, not
  tokens.
- Encrypted reasoning is bound to the OpenAI organization. Moving the API key to another org breaks replay for
  existing sessions, and only a new session recovers.
- Without streaming, a dead connection is detected only by the per-attempt deadline. A very long reasoning
  response can hit that deadline too. Per-model `timeout_sec` is the operator's lever, and streaming is follow-up
  work.

## Alternatives Considered

- **Rename the field to `max_completion_tokens` on Chat Completions.** It removes the visible 400. GPT-6 tool
  turns still fail.
- **Always use Responses in the `openai` driver.** Third-party endpoints that only implement Chat Completions
  would break.
- **A new driver name or a config key that selects the wire.** The operator's config already says "direct
  OpenAI". An extra knob nobody knows to set leaves the same 400 in place.
- **Retry with the other field on a 400.** It depends on error text and spends a request on every miss.
- **`store: true` with `previous_response_id`.** Compaction, daemon restarts, model switches and the server's
  retention window all break the chain, so the full resend has to exist anyway. Prior context is still billed as
  input, and the conversation gets a second source of truth outside SQLite.
- **Replay the raw `output` array instead of rebuilding calls from the transcript.** The output would be exact,
  but any divergence between that copy and the transcript produces a function call with no output, which is
  a 400.
