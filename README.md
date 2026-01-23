# yiduo

Device sync agent for AI Wrapped.

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
