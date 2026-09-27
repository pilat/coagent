# Sandbox boundary

A sandbox-enabled session runs one compiled policy: an ordered list of
allow/deny rules. Evaluation walks the list and the **last matching rule
decides**. Coagent ships no rules of its own about the operator's machine — no
built-in credential list, no built-in writable directories, no per-tool
profiles. Every rule names a path on the operator's machine, which is
knowledge coagent does not have.

## The model

The list is not empty, though. It has implicit defaults, best understood as
invisible rules at the head of two sections:

```
1  allow  /                  ro     implicit default — heads the GLOBAL section
2  <the operator's global rules>     written under it
3  allow  <project root>      rw     implicit default — heads the PROJECT section
4  allow  <work directory>    rw     implicit default, only when it differs from the project root
5  allow  <worktree .git>     rw     implicit default, only for a worktree coagent created with /gwt
6  <that project's rules>            written under them
7  allow  <project process-output dir> ro   mandatory last entry of the PROJECT section
```

The whole host is readable by default and not writable. The session's project
is writable without being mentioned anywhere — the `projects:` map attaches
*rules* to a project, it does not grant the project.

Because the project's default sits after the global rules, a broad global rule
such as `deny: ~/` cannot make the project unwritable; it still denies
everything else under `~`. A rule inside the project's own section can make it
unwritable, and an explicit `deny` naming the project root exactly is honoured
as deliberate — but a broader rule in that section that buries the project
incidentally is refused as a configuration error.

A `/gwt` worktree inherits the rules of the project it was created from, and
may add its own.

Process output is daemon-owned: the project's section ends with mandatory
read-only access to `~/.coagent/processes/project-<id>/`, after its configured
rules. This applies to that project's sessions, including subagents, and keeps
`read`, `tail` and Bash able to inspect output even under
`deny: ~/.coagent`. It does not reopen secrets, the database or other projects'
output, and sessions cannot alter the captured files. The directory exists
before process launch so output created later remains visible. Telegram
attachments in the host temporary directory have no such exception.

## Grammar

```yaml
sandbox:
  enabled: true              # omitted means enabled; false is the explicit opt-out
  rules:
    - deny: ~/.ssh                    # no private key is readable, under any filename
    - allow: ~/.ssh/known_hosts       # host verification is not a credential
    - allow: ~/.ssh/config
    - deny: ~/.aws
    - deny: ~/.coagent                # the daemon's own secrets and session store
    - allow: ~/.cache
      mode: rw
    - allow: ~/go/pkg/mod
      mode: rw
  projects:
    /home/example/projects/service:
      rules:
        - allow: /run/example/agent.sock
```

- Exactly one of `allow` or `deny` per rule.
- `mode` applies only to `allow` and defaults to `ro`.
- Paths are absolute or start with `~/`. There is no environment-variable
  expansion: a variable is a value coagent would have to capture and trust, while
  the path is something you already know. Write it out, or use `~/`. A `..` component is refused.
- Paths and project keys resolve to canonical absolute paths before use;
  two project keys naming the same directory are rejected, including symlink aliases.
- A read-only `allow` can reopen reads after a `deny` or revoke writes from an
  earlier writable grant. With only the implicit host read-only default,
  `allow: ~/.ssh/config` adds no authority.
- An effective `deny` target must exist. A read-only exception beneath a writable
  grant must also exist. Missing targets fail policy compilation or process
  preparation; they are never silently skipped or created as deny placeholders.
- A denied directory becomes an empty directory inside the sandbox; a denied
  file reads as empty. It is a mount, not a deletion — the host objects are
  untouched.

## Giving the use without giving the secret

A private SSH key is not read in order to be used — it is used to *sign* —
and `ssh-agent` already separates those two rights. So `deny: ~/.ssh` keeps
every key unreadable while `git push` over SSH keeps working: the sandbox
reaches the agent socket, the agent signs outside the sandbox, and no key
material ever enters it. What ssh still needs from `~/.ssh` is host
verification and host aliases, neither of which is a credential — hence the
`known_hosts` and `config` carve-outs above.

A read-only mount does not prevent connecting to a Unix socket. Filesystem
permissions on the socket still apply, and access to it grants use of the
service behind it. If an earlier deny hides its directory, allow the socket's
actual absolute path after that deny; making its whole directory writable is
unnecessary.

Where a credential has no agent, there is no such split. A bearer token in a
file — a `gh` token, `~/.git-credentials`, an `~/.npmrc` auth line — is either
readable or it is not, so denying it means the tool that needs it stops
working. That is a judgement the operator makes; there is no clever answer
here.

## What the boundary is and is not

- It is an **integrity** boundary, not confidentiality. With no rules, a
  session reads every file the daemon's user can read — including the
  operator's keys and coagent's own secrets file. Writes are confined to the
  project.
- There is **no network boundary**. The LAN, host services and the public
  internet are all reachable. The built-in web fetch and REST search tools
  refuse link-local and cloud-metadata addresses at dial time, but that is a
  property of those tools, not of the sandbox, and Bash does not inherit it.
- An operator who wants a real boundary runs the whole daemon inside a
  container or VM they built. That is a deployment decision.

## Requirements

`bwrap` (Bubblewrap) on the host. No capabilities, no sysctl changes.

## A recommended starting block

Coagent ships nothing — this is a suggestion to copy into `config.yaml` and
adjust, not a default that exists without it. Keep deny entries only for paths
that exist on your machine; a missing deny target prevents sandbox startup.

```yaml
sandbox:
  rules:
    - deny: ~/.ssh                    # no private key readable under any filename
    - allow: ~/.ssh/known_hosts       # host verification, not a credential
    - allow: ~/.ssh/config            # host aliases, not a credential
    - deny: ~/.gnupg                  # signing/decryption keys
    - deny: ~/.aws                    # cloud credentials
    - deny: ~/.kube                   # cluster credentials
    - deny: ~/.config/gcloud          # cloud credentials
    - deny: ~/.netrc                  # plaintext host credentials
    - deny: ~/.git-credentials        # plaintext Git bearer tokens
    - deny: ~/.npmrc                  # may hold a registry auth token
    - deny: ~/.coagent                # the daemon's own secrets and session store
    - allow: ~/.cache                 # toolchains expect their cache writable
      mode: rw
    - allow: ~/go/pkg/mod             # Go module cache
      mode: rw
    - allow: ~/.npm                   # npm cache
      mode: rw
    - allow: ~/.cargo                 # Cargo registry and build cache
      mode: rw
```
