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

Run the binary:

```sh
yiduo --source auto --server http://localhost:8000
```

Environment:

- `AI_WRAPPED_DEVICE_TOKEN`: device token for sync
- `AI_WRAPPED_SERVER`: API base URL

Example:

```sh
AI_WRAPPED_DEVICE_TOKEN=... yiduo --source auto --server http://localhost:8000
```
