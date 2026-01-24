# yiduo

Device sync agent for AI Wrapped.

## Install

```sh
curl -fsSL https://github.com/slyang-git/yiduo/releases/latest/download/install.sh | bash -s -- --token <device-token> --server http://localhost:8000
```

The binary is installed to `~/.local/bin` by default. Make sure `~/.local/bin` is in your `PATH`.

## Usage

Install via the one-line curl command will automatically sync sessions from all supported tools and save credentials to `~/.yiduo/config.json`.
Later, to sync again, just run:

```sh
yiduo sync
```

Run a background daemon that syncs every minute:

```sh
yiduo sync --daemon
```
