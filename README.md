# yiduo

Device sync agent for AI Wrapped.

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

Check daemon status or stop it:

```sh
yiduo sync status
yiduo sync stop
yiduo sync restart
```
