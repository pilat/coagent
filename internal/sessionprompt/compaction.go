package sessionprompt

// CompactionSummaryPrompt requests continuation context without imposing a Markdown schema.
const CompactionSummaryPrompt = `You are writing a continuation checkpoint for a coding agent. The older history is the conversation above this instruction; the newer verbatim messages are not part of this request and will follow your checkpoint instead, so whatever you leave out of the history above is lost. The agent will read your summary as one block, followed by those newer messages.

Summarize the older history above, so work can continue without rediscovery:
- The task and why it is being done this way, including decisions already made and why alternatives were rejected.
- What has succeeded so far — completed mutations, verification that already passed, commands that were run — so it is not repeated.
- What is still open: current state, the next action, anything unresolved.
- Errors already hit and how they were resolved, so they are not retried as new.
- Active background work (advertised processes and pending subagents) is recorded separately by the host; do not restate it.

Preserve technical specifics exactly as written: file paths, line numbers, commands, error messages, and every opaque identifier (UUIDs, hashes, commit SHAs, URLs, branch names) verbatim — never shorten or paraphrase them.

Do not invent completed work, successful verification, decisions, or blockers. When the source is uncertain, preserve that uncertainty.

Write plain prose or bullet points; no fixed headings are required. Be concise and complete; do not include tool-call syntax or chat filler. Do not use any tools. Answer with the summary text only.`
