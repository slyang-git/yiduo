# yiduo

YiDuo device sync CLI. It discovers local AI coding-agent sessions, sends them
to the YiDuo API, manages the background sync daemon, and backs up personal
agent skills across devices.

This repository contains only the Go CLI. The dashboard frontend and Go API
server live in the separate
[`ai-wrapped`](https://github.com/slyang-git/ai-wrapped) repository under
`dashboard/` and `backend/` respectively.

## Install

On macOS or Linux, use the installer published by the dashboard:

```sh
curl -fsSL https://yiduo.one/install.sh | bash
```

The installer prompts for an auth token. For a non-interactive install:

```sh
curl -fsSL https://yiduo.one/install.sh | bash -s -- --token <auth-token>
```

It installs `yiduo` to `~/.local/bin`, writes configuration to
`~/.yiduo/config.json`, starts the one-minute sync daemon, and restores the
latest personal skill snapshot when available. Keep `~/.local/bin` in `PATH`.

Windows installers are also published as `https://yiduo.one/install.cmd` and
`https://yiduo.one/install.ps1`; the dashboard shows the appropriate command.

## Session sync

Run a one-off automatic sync:

```sh
yiduo sync
```

Select one or more sources with a comma-separated list:

```sh
yiduo sync --source codex,claude,cursor
```

Supported sources are Codex, Claude, Gemini, Qwen, Cline, Continue, Kilo Code,
Cursor, Amp, OpenCode, Pi, OpenClaw, Crush, Antigravity, Droid, Qoder, Goose,
Kimi Code, Warp, and Blackbox Code. Use `--source auto` for detected tools or
`--source all` to scan every supported source.

Manage the background daemon:

```sh
yiduo sync start
yiduo status
yiduo sync restart
yiduo sync log
yiduo sync stop
```

Running `yiduo sync --daemon` is equivalent to starting daemon mode. Starting
again restarts an existing daemon so the installed binary takes effect.

## Skill backup and restore

Back up personal, user-level agent skills:

```sh
yiduo skills sync
```

Install the latest immutable snapshot on another device without overwriting
unmanaged local conflicts:

```sh
yiduo skills restore
```

Inspect local and remote state:

```sh
yiduo skills status
yiduo skills list
yiduo skills diff
```

`skills sync` and `skills push` back up local changes without propagating
deletions. `skills pull` and `skills restore` install the latest remote
snapshot. Project, bundled, admin, plugin-cache, secret, and special files are
excluded. Daemon mode uploads local skill changes after a five-minute debounce;
remote changes are pulled only by an explicit pull or restore command.

## Update

```sh
yiduo update
```

`yiduo upgrade` is an alias. If the daemon is running, the updater stops it,
installs the latest release, and starts it again.

## Configuration

Command-line flags override environment variables and values saved in
`~/.yiduo/config.json`.

- `--server` / `AI_WRAPPED_SERVER`: API base URL; defaults to `https://yiduo.one/`
- `--auth-token`: sync auth token
- `--source`: source name, `auto`, `all`, or a comma-separated list
- source-specific `--*-root` flags: override local discovery paths

Run `yiduo --help` and `yiduo skills --help` for the complete command reference.

## Development

Requires Go 1.22 or newer.

```sh
go test ./...
go build ./...
go run . sync --source auto
```

Release tags use `v0.0.X`. Pushing a tag builds Linux, macOS, and Windows
archives for AMD64 and ARM64.

## Brand assets

Project-local tool icons live under `assets/brands/`; canonical source URLs are
recorded in `assets/brands/manifest.json`.
