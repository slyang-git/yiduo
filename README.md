# yiduo

Device sync agent for AI Wrapped.

## Brand Assets

Project-local tool brand icons live under `assets/brands/`.

- Goose: `assets/brands/goose.png`
- Kimi Code: `assets/brands/kimi.ico`

Canonical source URLs are recorded in `assets/brands/manifest.json`.

## Frontend

Static frontend files live under `web/`.

For local preview:

```sh
cd web
python3 -m http.server 4173
```

Then open `http://127.0.0.1:4173/`.

## Install

Installer script is hosted and maintained externally at `https://yiduo.one/install.sh` (not in this repository).

```sh
curl -fsSL https://yiduo.one/install.sh | bash
```

The installer will prompt for your device token.  
For non-interactive environments, pass token explicitly:

```sh
curl -fsSL https://yiduo.one/install.sh | bash -s -- --token <device-token>
```

The binary is installed to `~/.local/bin` by default. Make sure `~/.local/bin` is in your `PATH`.

## Usage

Install via the one-line curl command will save credentials to `~/.yiduo/config.json` and automatically start the background sync daemon.
Later, to sync again, just run:

```sh
yiduo sync
```

Run a background daemon that syncs every minute:

```sh
yiduo sync --daemon
```

If a daemon is already running, this command will restart it so the latest installed binary takes effect immediately.

Check daemon status or stop it:

```sh
yiduo status
yiduo sync stop
yiduo sync restart
yiduo sync log
```

Sync a specific source (for example OpenClaw sessions from `~/.openclaw/agents/main/sessions`):

```sh
yiduo sync --source openclaw
```
