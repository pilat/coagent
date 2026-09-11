package builtin

const bashDescription = `Executes a given bash command in a shell session with optional timeout.

IMPORTANT: This tool is for terminal operations like git, npm, docker, build commands, etc. DO NOT use it for file operations (reading, writing, editing, searching, finding files) - use the dedicated tools for this instead.

Avoid using Bash with find, grep, cat, head, tail, sed, awk, or echo commands, unless explicitly instructed. Instead, always prefer using the dedicated tools:
- File search: Use Glob (NOT find or ls)
- Content search: Use Grep (NOT grep or rg)
- Read files: Use Read (NOT cat/head/tail)
- Edit files: Use Edit (NOT sed/awk)
- Write files: Use Write (NOT echo >/cat <<EOF)

Command Execution:
- Always quote file paths that contain spaces with double quotes (e.g., rm "path with spaces/file.txt")
- The command argument is required
- Both stdout and stderr are captured into one combined stream

Parallel vs Sequential Commands:
- If the commands are independent and can run in parallel, make multiple Bash tool calls in a single response
- Overlapping builds, test suites, or verification commands for the same goal are not independent; run one canonical command and wait for its result
- If the commands depend on each other and must run sequentially, use a single Bash call with '&&' to chain them together
- Use ';' only when you need to run commands sequentially but don't care if earlier commands fail
- DO NOT use newlines to separate commands (newlines are ok in quoted strings)
- AVOID using 'cd <directory> && <command>'. Use the work_dir parameter to change directories instead

Limits:
- Output is truncated at 100KB inline; larger output stays in the reported output file
- Foreground failures (nonzero exit, timeout, output overflow) are reported as errors

Git Safety Protocol:
- NEVER update the git config
- NEVER run destructive/irreversible git commands (like push --force, hard reset, etc.) unless explicitly requested
- NEVER skip hooks (--no-verify, --no-gpg-sign, etc.) unless explicitly requested
- NEVER run force push to main/master, warn the user if they request it
- Avoid git commit --amend. ONLY use --amend when ALL conditions are met:
  (1) User explicitly requested amend, OR commit SUCCEEDED but pre-commit hook auto-modified files that need including
  (2) HEAD commit was created by you in this conversation
  (3) Commit has NOT been pushed to remote
- CRITICAL: If commit FAILED or was REJECTED by hook, NEVER amend - fix the issue and create a NEW commit
- CRITICAL: If you already pushed to remote, NEVER amend unless user explicitly requests it (requires force push)
- NEVER commit changes unless the user explicitly asks you to
- If there are no changes to commit, do not create an empty commit

Pull Requests:
- Use gh command via Bash tool for ALL GitHub-related tasks
- When creating PRs, analyze ALL commits that will be included (not just the latest)
- Return the PR URL when done so the user can see it

Examples:
- "git status"
- "npm install"
- "go build ./..."`

// Repetition is deliberate: weaker models often ignore a single no-poll instruction.
const backgroundDescriptionSuffix = `

Background Execution:
- A command without background=true stays in the foreground for 10 seconds; only if it is still running then is it moved to the background
- Set "background": true to background immediately without the 10-second wait
- A backgrounded command returns a process ID and an absolute output path; its final result arrives automatically in a new turn - do NOT poll
- Do NOT poll background processes with Bash, ps, sleep, schedule, Read, or Tail; keep working on independent tasks instead
- Do NOT poll background processes: their completion arrives as a user turn automatically
- When background work is your only remaining action, reply with a standalone <WAITING/> line and no tool calls
- "timeout" is the absolute process deadline in milliseconds (default 600000, max 1800000); a deadline below 10000 expires in the foreground
- Only finite commands are supported; a command reaching its deadline is killed as a complete process group`
