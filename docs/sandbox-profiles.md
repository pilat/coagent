# Sandbox profiles

A sandbox-enabled session starts from an allowlisted filesystem. The base grants
are fixed: the canonical project read-write, a private per-project `/tmp` and
`/var/tmp`, the read-only system execution substrate, and — for a linked `/gwt`
worktree — the main repository's `.git`. Everything else a tool needs is
described by a **profile**.

Profiles are operator configuration, never repository content. A model tool
call, a project file, an MCP registration or a shell variable cannot grant
authority; only `~/.coagent/config.yaml` can, through `/config` and its
restart/rollback protocol.

## Configuration

```yaml
sandbox:
  enabled: true                 # omitted means enabled; false is the explicit opt-out
  escalated: [ssh]              # global escalation, applies to every project
  projects:
    /home/example/projects/service:
      escalated: [docker]       # this project only; cannot remove a global grant
  profiles:
    docker:                     # same name replaces the built-in definition in full
      mounts:
        - path: /usr/bin/docker
          mode: ro
          type: basic
        - path: ~/.docker/buildx
          mode: rw
          type: escalated
      sockets:
        - path: /var/run/docker.sock
          type: escalated
      network: []
```

Every entry carries `type: basic` or `type: escalated`.

- **basic** entries of every known profile are active whenever the sandbox is
  enabled. Tool detection never decides authority.
- **escalated** entries apply only when the operator names that profile in
  `sandbox.escalated` or in the matching `sandbox.projects` entry. The effective
  set is the union; a project cannot subtract a global grant.
- Mounts additionally carry `mode: ro` or `mode: rw`. Sockets have no mode:
  socket permissions are the socket's own. A socket entry exposes one exact
  pathname socket; a directory mount already includes the sockets inside it.
- Network entries name an IP, a CIDR prefix or the reserved `host-loopback`
  target, with `protocol: tcp|udp` and a non-empty `ports` list.

`projects` keys are absolute or `~/` paths and match exactly. A `/gwt` worktree
created by coagent inherits the escalation of its source repository, then adds
any entry configured for the worktree's own path. Other nested directories and
worktrees opened as ordinary projects do not inherit it. Inherited grants can
include credential files and sockets; raising session shields removes them.
Worktrees created before this rule keep their existing Git access but do not
inherit source-project escalation; recreate them to opt in.

### Path rules

Mounts and sockets accept absolute paths or `~/`. The shipped catalog may also
reference a fixed set of daemon-start environment names — `SSH_AUTH_SOCK`,
`GH_CONFIG_DIR`, `XDG_CONFIG_HOME`, `XDG_DATA_HOME`, `XDG_CACHE_HOME`,
`XDG_STATE_HOME`, and the five `MISE_*_DIR` paths — with documented defaults;
an unset name with no default drops its entry. There is no shell evaluation,
command substitution, `PATH`-derived mount or grant taken from a captured
environment.

A definition is refused when it grants the filesystem root, the home root, the
whole of `/tmp`, `/run`, `/proc`, `/sys` or `/dev`, the coagent control
directory, or repeats the same path with contradictory modes in one level.
A basic read-only mount may be promoted to read-write at escalation.
Contradictory duplicates inside one definition, unknown profiles, unknown enum
values and malformed addresses or ports fail while the candidate config is
staged, so the live configuration is never replaced by an unusable one.

### Deprecated `writable_paths`

`sandbox.writable_paths` still works as an explicit operator grant: each entry
becomes a read-write grant labelled `legacy`, suppressed by raised shields, and
subject to the same broad-root validation. It no longer implies host temporary
storage or the whole user cache. Migrate those entries to profile mounts.

## Session shields

Raised shields remove every profile entry — basic and escalated, mounts, sockets
and network exceptions. They keep the base filesystem, the linked-worktree Git
metadata and the private temporary storage. `/shieldsdown` restores the ordinary
profile set.

## Shipped profiles

Paths come from each tool's official documentation. Where a tool documents an
environment override, the catalog uses it and names the default here.

### `shell`

Bash startup files, granted as individual files: `~/.bashrc`, `~/.bash_profile`,
`~/.profile`, `~/.bash_aliases` — all read-only, basic. Home and `.config` are
never mounted wholesale; a script sourced from elsewhere needs its own grant.
Shell activation runs inside the session's confinement. Raised shields bypass
capture and replay instead of reading these files.

Smoke: with a `~/.bashrc` exporting a variable, an ordinary session's Bash tool
sees it; a raised session does not.

### `mise`

[mise directories](https://mise.jdx.dev/directories.html): data
`$XDG_DATA_HOME/mise` (`~/.local/share/mise`), config `$XDG_CONFIG_HOME/mise`
(`~/.config/mise`), cache `$XDG_CACHE_HOME/mise` (`~/.cache/mise`), state
`$XDG_STATE_HOME/mise` (`~/.local/state/mise`). The matching `MISE_*_DIR`
overrides take precedence; `MISE_INSTALLS_DIR` may move installs separately.
The [mise installer](https://mise.jdx.dev/installing-mise.html) puts the CLI
at `~/.local/bin/mise` by default.

- basic read-only: `~/.local/bin/mise`, the selected installs and config directories
- basic read-write: the selected cache and state directories
- escalated read-write: the selected data and installs directories — installing
  or updating toolchains changes the shared installation, even when
  `MISE_INSTALLS_DIR` points outside `MISE_DATA_DIR`.

Smoke: in a project with a pinned toolchain, `mise exec -- <tool> --version`
runs the pinned binary from the install directory, and the same selection is
visible to a stdio MCP server launched in that project.

### `git`

[git config locations](https://git-scm.com/docs/git-config): `~/.gitconfig`,
`$XDG_CONFIG_HOME/git/config` (`~/.config/git/config`), plus `ignore` and
`attributes`.

- basic read-only: `~/.gitconfig`, `$XDG_CONFIG_HOME/git/{config,ignore,attributes}`
- basic read-write: `$XDG_CACHE_HOME/git-lfs`
- escalated read-only: `~/.git-credentials`, `$XDG_CONFIG_HOME/git/credentials`

Credential files are separate child grants: the directory that mixes them with
configuration is never granted as a whole.

Smoke: `git log`, `git status` and `git fetch` inside the project work; reading
`~/.git-credentials` fails at basic and succeeds with `git` escalation.

### `ssh`

[OpenSSH](https://man.openbsd.org/ssh_config) reads `~/.ssh/config` and writes
`~/.ssh/known_hosts`.

- basic read-only: `~/.ssh/config`
- basic read-write: `~/.ssh/known_hosts`
- escalated socket: `$SSH_AUTH_SOCK`, the daemon-start agent socket, bound as
  one exact pathname socket — its containing directory and host temporary
  storage are never exposed.

Private keys are deliberately not granted: the agent-backed flow authenticates
without a readable key file.

Smoke: `git fetch` over SSH works with `ssh` escalation and a test-owned agent,
while `~/.ssh/id_ed25519` stays unreadable.

### `gh`

[gh environment](https://cli.github.com/manual/gh_help_environment) and
[gh authentication](https://cli.github.com/manual/gh_auth_login): configuration
lives in `$GH_CONFIG_DIR` (otherwise `$XDG_CONFIG_HOME/gh`, then
`~/.config/gh`), cache in `$XDG_CACHE_HOME/gh`. The profile also admits the
exact user-local `~/.local/bin/gh` executable; system-installed binaries come
from the base runtime.

- basic read-only: `~/.local/bin/gh`, `$GH_CONFIG_DIR/config.yml`
- basic read-write: `$XDG_CACHE_HOME/gh`
- escalated read-only: `$GH_CONFIG_DIR/hosts.yml` — file-stored auth tokens

Smoke: `gh api user` fails at basic and succeeds with `gh` escalation; a
credential-helper socket, when configured, needs its own escalated socket grant.
The SSH agent authenticates Git transport only. `gh api`, `gh auth status` and
other API commands need a gh token, so enabling `ssh` does not replace `gh`
escalation. Git operations that use SSH need both `gh` and `ssh` escalation.

## Coverage matrix status

The following built-ins are deliberately dormant: every listed entry is
`escalated`, so each profile needs an explicit operator opt-in. They grant no
network exceptions. Tool-specific relocation flags and environment variables
are not inferred from a shell environment; replace the built-in profile in
`config.yaml` when a tool uses a non-default location.

- `npm`: [cache](https://docs.npmjs.com/cli/v8/commands/npm-cache/) `~/.npm`
  (read-write) and exact user config `~/.npmrc` (read-only).
- `pnpm`: the standalone [installer](https://github.com/pnpm/get.pnpm.io/blob/main/install.sh)
  puts the CLI at `~/.local/share/pnpm/pnpm` (read-only); the default Linux
  [store](https://pnpm.io/settings#storedir) is read-write.
- `yarn-berry`: the Yarn 2+ [global folder](https://yarnpkg.com/configuration/yarnrc#globalfolder)
  `~/.yarn/berry` (read-write). Project `.yarn` stays inside the project grant.
- `uv`: [cache and storage](https://docs.astral.sh/uv/reference/storage/)
  `~/.cache/uv`, managed Pythons, and persistent `tools/` data (read-write),
  plus exact `uv`/`uvx`, `uv.toml`, and credential files (read-only). This
  supports `uv tool run` and existing installed tools; `uv tool install` needs
  a replacement profile for its dynamically named executables.
- `pip`: cache `~/.cache/pip` (read-write) and exact user configuration files
  `~/.config/pip/pip.conf` and `~/.pip/pip.conf` (read-only), per [pip configuration](https://pip.pypa.io/en/stable/topics/configuration/).
- `docker`: exact sensitive [CLI config](https://docs.docker.com/reference/cli/docker/)
  `~/.docker/config.json` (read-only), Buildx state `~/.docker/buildx`
  (read-write), and exact `/var/run/docker.sock` socket.
- `kubectl`: exact credential-bearing `~/.kube/config` (read-only) and default
  [cache directory](https://kubernetes.io/docs/reference/kubectl/generated/kubectl_config/)
  `~/.kube/cache` (read-write). Operators can replace the profile when context
  mutation is required.
- `gradle`: [Gradle User Home](https://docs.gradle.org/current/userguide/directory_layout.html)
  caches, daemon state, wrapper distributions, downloaded JDKs (read-write), initialization
  scripts and exact `gradle.properties` (read-only).
- `maven`: [local repository and user settings](https://maven.apache.org/settings.html)
  `~/.m2/repository` and [Maven Wrapper distributions](https://maven.apache.org/tools/wrapper/maven-wrapper/)
  (read-write), plus exact `~/.m2/settings.xml` (read-only).
- `cargo`: [Cargo home](https://doc.rust-lang.org/cargo/guide/cargo-home.html)
  binaries, Git and registry caches (read-write); exact config and both current
  and legacy credential files (read-only).
- `rustup`: [Cargo bin proxies and toolchain state](https://rust-lang.github.io/rustup/installation/)
  `~/.cargo/bin` and the whole rustup-specific `~/.rustup` home are read-write
  after opt-in, so first-use settings and toolchain updates can be created.
  The Cargo and rustup profiles intentionally share the same bin directory.
- `go`: default build cache `~/.cache/go-build`, module cache `~/go/pkg/mod`,
  install directory `~/go/bin`, and scoped `~/.config/go` environment directory
  (read-write, so `go env -w` can create its file on first use),
  as described by [`go env`](https://go.dev/cmd/go/) and the [module cache](https://go.dev/ref/mod).
- `dotnet`: the [user-local SDK and global tools](https://learn.microsoft.com/en-us/dotnet/core/tools/dotnet-install-script)
  under `~/.dotnet`, plus [NuGet global packages, HTTP cache, and plugin cache](https://learn.microsoft.com/en-us/nuget/consume-packages/managing-the-global-packages-and-cache-folders),
  are read-write after opt-in; exact alternate user `NuGet.Config` files remain
  read-only.
- `terraform`: exact [CLI config](https://developer.hashicorp.com/terraform/cli/config/config-file)
  and credential file (read-only), plus the optional plugin cache (read-write).
- `helm`: [XDG cache, data, and configuration](https://helm.sh/docs/intro/install/)
  directories (read-write). The scoped configuration directory is needed for
  first-use repository and registry files, including credential-bearing state.
- `playwright`: [browser cache](https://playwright.dev/docs/browsers) at
  `~/.cache/ms-playwright` (read-write). A `PLAYWRIGHT_BROWSERS_PATH` override
  needs an operator replacement profile.
- `ccache`: [default and legacy cache directories](https://ccache.dev/manual/latest.html)
  `~/.cache/ccache` and `~/.ccache` (read-write).
- `sccache`: [default cache directory](https://github.com/mozilla/sccache#configuration)
  `~/.cache/sccache` (read-write). A configured `SCCACHE_DIR` needs a replacement.
- `conan-2`: the documented [Conan home](https://docs.conan.io/2/reference/environment.html)
  is `~/.conan2`. The profile grants package, extension and download-cache data
  read-write and individual configuration paths read-only; it deliberately does
  not mount the home wholesale because it can contain credentials.
- `composer`: [XDG cache and data directories](https://getcomposer.org/doc/06-config.md)
  (read-write), plus exact global `config.json` and credential-bearing
  [`auth.json`](https://getcomposer.org/doc/articles/authentication-for-private-packages.md)
  (read-only).
- `bundler-rubygems`: Bundler's [user cache and global config](https://bundler.io/v2.3/man/bundle-config.1.html),
  plus RubyGems cache and specs below `~/.gem` (read-write) and exact
  [`~/.gem/credentials`](https://guides.rubygems.org/command-reference/#gem-credentials)
  (read-only).
- `dart-pub` and `flutter`: Dart's [global pub cache](https://dart.dev/tools/pub/package-layout)
  `~/.pub-cache` (read-write). Flutter SDK and project `.dart_tool` state use
  explicit installation and project paths, respectively.
- `android-sdk-adb`: default Linux `~/Android/Sdk` (read-write) and the exact
  documented Android user files. The [SDK variables](https://developer.android.com/tools/variables)
  put user state in `~/.android`; the ADB private key is a separate read-only
  file grant. Existing configuration files are also read-only; first-time ADB
  key/config creation needs an operator replacement profile. ADB's server socket
  is dynamic, so no broad temporary-directory or socket grant is shipped.
- `deno`: the default [standalone CLI](https://docs.deno.com/runtime/getting_started/installation/)
  `~/.deno/bin/deno` is read-only and `~/.cache/deno` is read-write. `DENO_DIR`
  overrides need an operator replacement.
- `bun`: the default [standalone CLI](https://bun.com/docs/installation)
  `~/.bun/bin/bun` is read-only and its [package cache](https://bun.com/docs/pm/cli/install)
  is read-write; global installs and overrides need a replacement profile.
- `poetry`: [cache and configuration](https://python-poetry.org/docs/configuration/)
  `~/.cache/pypoetry` (read-write), and exact `config.toml` and
  [`auth.toml`](https://python-poetry.org/docs/repositories/#configuring-credentials)
  below `~/.config/pypoetry` (read-only).
- `conda-mamba`: the documented user fallback package and environment paths
  [under `~/.conda`](https://docs.conda.io/projects/conda/en/stable/user-guide/configuration/admin-multi-user-install.html)
  (read-write), plus exact user [`.condarc`](https://docs.conda.io/projects/conda/en/stable/commands/config.html)
  (read-only). The installation root and configured `pkgs_dirs`/`envs_dirs`
  vary, so they require an operator replacement profile.
- `jupyter`: [Jupyter data and configuration paths](https://jupyter.readthedocs.io/en/latest/use/jupyter-directories.html)
  `~/.local/share/jupyter` (read-write) and `~/.jupyter` (read-only). The
  runtime directory is intentionally absent: it follows `JUPYTER_RUNTIME_DIR`,
  then `$XDG_RUNTIME_DIR`, then a user-specific temporary path, so an operator
  must supply an exact replacement profile if a local runtime socket is needed.
- `sbt-coursier`: sbt's documented [boot](https://www.scala-sbt.org/1.x/docs/Launcher-Configuration.html)
  and Ivy cache directories (`~/.sbt/boot`, `~/.ivy2/cache`) and Coursier's
  default Linux [cache](https://get-coursier.io/docs/cache) (read-write), plus
  the exact Coursier [credential file](https://get-coursier.io/docs/other-credentials.html)
  (read-only). Global sbt plugins and repository settings need an operator
  replacement because they are executable or may contain credentials.
- `mix-hex`: Mix's [global home](https://hexdocs.pm/mix/Mix.html) `~/.mix`
  is read-write after opt-in so archives, scripts and first-use `rebar3` can be
  created; Hex package caches are read-write and exact `~/.hex/hex.config` is
  read-only. Hex documents that
  [`HEX_HOME`](https://hex.hexdocs.pm/Mix.Tasks.Hex.Config.html) contains both
  cache and configuration; the profile deliberately avoids mounting that home
  wholesale because its configuration can hold an API key.
- `cabal`: Cabal's Unix [XDG cache and state directories](https://cabal.readthedocs.io/en/stable/config.html)
  (read-write), plus exact `~/.config/cabal/config` (read-only). The legacy
  `~/.cabal` layout and `CABAL_DIR` overrides need an operator replacement.
- `stack`: selected default Stack-root package state — `programs`, `snapshots`,
  and `pantry` — is read-write, while exact `~/.stack/config.yaml` is read-only,
  according to [Stack root](https://docs.haskellstack.org/en/stable/topics/stack_root/).
  `STACK_ROOT`, `STACK_XDG`, and executable global-project state need an
  operator replacement profile.
- `swiftpm`: the documented Linux global cache
  [`.cache/org.swift.swiftpm`](https://developer.apple.com/documentation/swift/swiftpm-support-for-compilation-caching)
  is read-write. Package checkouts and `.build` stay within the project grant;
  custom cache paths need an operator replacement.
- `nix`: read-only `/nix/store`, exact user `nix.conf`, XDG state and cache
  directories, and the exact default daemon socket are all escalated. The
  [Nix manual](https://nix.dev/manual/nix/latest/command-ref/conf-file.html)
  defines those paths and the [daemon store](https://nix.dev/manual/nix/latest/store/types/local-daemon-store.html)
  uses `/nix/var/nix/daemon-socket/socket`. That socket can authorize builds and
  store changes outside the project, so enable it only for projects that need
  Nix. Legacy dotfile layouts, profiles, and alternate daemon sockets require
  an operator replacement.
- `podman`: rootless image storage defaults to
  [`~/.local/share/containers/storage`](https://docs.podman.io/en/latest/markdown/podman.1.html)
  (read-write), with exact user configuration files read-only. The profile
  includes only the rootful API socket `/run/podman/podman.sock`; it can control
  containers and therefore has host-level authority. Podman's rootless API
  socket is `$XDG_RUNTIME_DIR/podman/podman.sock`, per
  [podman-system-service](https://docs.podman.io/en/latest/markdown/podman-system-service.1.html),
  so it is intentionally not hardcoded: add its exact current pathname in an
  operator replacement profile.
- `ansible`: default Galaxy collection and role directories are read-write,
  while `~/.ansible.cfg` and the exact default
  [`galaxy_token`](https://docs.ansible.com/projects/ansible/latest/reference_appendices/config.html)
  file are read-only. The configuration can hold Galaxy credentials, as can the
  token; both remain dormant until explicit escalation.
- `pulumi`: documented plugin and workspace directories are read-write, while
  exact `~/.pulumi/credentials.json` is read-only. Pulumi's
  [`PULUMI_HOME`](https://www.pulumi.com/docs/iac/cli/environment-variables/)
  also stores credentials and plugins; state from the local backend or other
  custom homes is deliberately excluded and needs an operator replacement.
- `aws-cli`: exact `~/.aws/config` and `~/.aws/credentials` are read-only.
  [AWS documents](https://docs.aws.amazon.com/cli/latest/userguide/cli-configure-files.html)
  temporary role credentials in `~/.aws/cli/cache` and IAM Identity Center
  tokens in `~/.aws/sso/cache`, so both dynamic caches are read-write but remain
  escalated. Alternate paths selected through AWS CLI environment variables
  require an operator replacement.
- `gcloud`: the [Google Cloud CLI's documented](https://docs.cloud.google.com/sdk/docs/configurations)
  Linux global configuration
  directory is `~/.config/gcloud`; it holds mutable configuration and
  credentials, so the profile grants the directory read-write only after
  escalation. `CLOUDSDK_CONFIG` overrides require an operator replacement.
- `azure-cli`: [Azure CLI](https://learn.microsoft.com/en-us/cli/azure/install-azure-cli/)
  keeps user configuration, authentication state, and extensions under
  `~/.azure` on Linux. Its `AZURE_CONFIG_DIR` override needs an operator
  replacement; the default directory is read-write and escalated.
- `vault`: exact `~/.vault` configuration and the
  [default cached](https://developer.hashicorp.com/vault/docs/commands/token-helper)
  `~/.vault-token` are read-only, escalated files. A custom token helper or
  alternate token location is outside the catalog and needs an operator
  replacement.
- `postgresql-psql`: exact [psql startup](https://www.postgresql.org/docs/current/app-psql.html),
  [password](https://www.postgresql.org/docs/current/libpq-pgpass.html) and
  [service](https://www.postgresql.org/docs/current/libpq-pgservice.html) files
  (`~/.psqlrc`, `~/.pgpass`, `~/.pg_service.conf`) and default user TLS files
  under `~/.postgresql` are read-only. They
  are all escalated because connection and client-certificate material can
  authenticate to databases; `PGPASSFILE`, `PGSERVICEFILE`, and other custom
  paths require an operator replacement.
- `mysql-mariadb`: exact `~/.my.cnf` and encrypted
  [`.mylogin.cnf`](https://dev.mysql.com/doc/refman/en/option-files.html) are
  read-only. Headless sessions do not mount client history files.
  [MariaDB's option-file search](https://mariadb.com/docs/server/server-management/install-and-upgrade-mariadb/installing-mariadb-binary-tarballs)
  also includes `~/.my.cnf`. Option-file includes and alternate defaults are
  intentionally excluded: the operator must name their exact paths in a
  replacement profile.
- `mongosh`: [MongoDB Shell's documented](https://www.mongodb.com/docs/mongodb-shell/logs/view/)
  Linux log and history directory,
  `~/.mongodb/mongosh`, is read-write and escalated. The log filename changes
  per session, so an exact-file grant is not practical.
- `httpie`: [HTTPie's default](https://httpie.io/docs/cli) `~/.config/httpie`
  holds dynamic session files. Sessions persist authentication, cookies, and
  headers in plain text, so this read-write directory is escalated.
  `HTTPIE_CONFIG_DIR` needs an operator replacement.
- `cypress`: the [Cypress binary cache](https://docs.cypress.io/app/references/advanced-installation)
  defaults to `~/.cache/Cypress` on Linux and is read-write.
  `CYPRESS_CACHE_FOLDER` overrides require an operator replacement.
- `selenium-manager`: [Selenium Manager's documented](https://www.selenium.dev/documentation/selenium_manager/)
  Linux cache,
  `~/.cache/selenium`, holds downloaded drivers, browsers, metadata, and its
  optional configuration file. It is a read-write escalated directory; custom
  `SE_CACHE_PATH` values need an operator replacement.

The catalog is a practical composite of common developer workflows informed by
the [Stack Overflow 2025 Technology survey](https://survey.stackoverflow.co/2025/technology),
[JetBrains 2025 Developer Ecosystem survey](https://blog.jetbrains.com/research/2025/10/state-of-developer-ecosystem-2025/),
and [GitHub Octoverse 2025](https://github.blog/octoverse/). It is not an
official ranked top-50 claim; official tool documentation determines every
individual path and grant.
Of 50 researched tool entries, three need no built-in host grant; the combined
Cypress/Selenium entry uses two profiles, leaving 48 dormant profiles in the
catalog.

`kotlin` has no separate shipped profile. Kotlin command-line tooling normally
uses Gradle and project-local state, so the `gradle` profile covers its documented
shared default. Add a separate operator profile only if the selected Kotlin tool
has a verified, non-project default home resource.

`cmake` and `vcpkg` have no shipped host profile. CMake builds in the project
or an explicit build directory; its user package registry is opt-in for export
since [CMake 3.15](https://cmake.org/cmake/help/latest/command/export.html).
vcpkg's root and binary cache are explicitly configured through
[environment variables](https://learn.microsoft.com/vcpkg/users/config-environment).
Grant those non-project paths only in an operator-defined profile.

Several listed configuration and credential files may contain secrets. Exact
files are preferred; tools with dynamic credential state have a scoped directory
grant. All remain absent until the profile is explicitly escalated.

## Network

Non-public destinations — host addresses, host loopback, LAN, link-local,
metadata and special-use ranges — require explicit profile network grants;
public egress is allowed by default. Each sandbox tree has a private network
namespace, routed by the kernel under an nftables ruleset compiled from those
grants. Decisions use the actual destination IP and protocol/port. ICMP is
granted per network in its own right, so a grant for it opens no transport port.
Built-in web fetch and REST search open connections through that same namespace.
`localhost` belongs to the tree; an admitted host-loopback service is reached
through `host.coagent.internal`. The generation stays alive while a tool stack
or background process uses it and retires after ten idle minutes. Raised shields
remove profile exceptions but retain public DNS.

Routing needs the host to have IPv4 and IPv6 forwarding enabled, and the daemon
to run with `CAP_NET_ADMIN` and `CAP_SYS_ADMIN` — the service unit `coagent
install` writes grants both. Missing either one fails sandbox startup rather
than falling back to host networking. The capabilities stay with the daemon:
every process it launches is re-exec'd without them.
