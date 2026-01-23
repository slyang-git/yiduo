# yiduo

Device sync agent for AI Wrapped.

## Install

```sh
curl -fsSL https://github.com/slyang-git/yiduo/releases/latest/download/install.sh | bash
```

Install a specific version:

```sh
YIDUO_TAG=v0.1.0 curl -fsSL https://github.com/slyang-git/yiduo/releases/latest/download/install.sh | bash
```

## Usage

Build and run:

```sh
go run . --source auto --server http://localhost:8000
```

Environment:

- `AI_WRAPPED_DEVICE_TOKEN`: device token for sync
- `AI_WRAPPED_SERVER`: API base URL

Example:

```sh
AI_WRAPPED_DEVICE_TOKEN=... go run . --source auto --server http://localhost:8000
```
