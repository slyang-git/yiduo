# Repository Guidelines

## Project Structure & Module Organization
This repository is a single-module Go CLI project.

- `main.go`: primary application entrypoint and all sync/daemon logic.
- `go.mod`: module definition (`go 1.22`).
- `README.md`: install and usage documentation.
- `.github/`: CI/workflow definitions (if present).

Keep new code grouped by feature area with clear section comments or helper functions. If the codebase grows, prefer moving source into internal packages (for example `internal/sync`, `internal/sources`) instead of adding more top-level files arbitrarily.

## Build, Test, and Development Commands
- `go build ./...`: compile all packages; use this before opening a PR.
- `go run . sync --source auto`: run a one-off sync locally.
- `go run . sync --daemon`: start background sync mode.
- `go run . sync status|stop|restart`: manage daemon lifecycle.
- `gofmt -w main.go`: format modified Go files.

If you add more files, format all touched `.go` files with `gofmt` before commit.

## Coding Style & Naming Conventions
- Follow standard Go formatting (`gofmt`), tabs for indentation.
- Use `camelCase` for local vars/functions and `PascalCase` for exported types.
- Prefer small, focused helpers over long inline blocks.
- Keep error messages actionable and include source context (for example `failed to load codex sessions`).
- CLI flags and user-facing command names should stay consistent with existing patterns (`sync`, `status`, `stop`, `restart`).

## Testing Guidelines
There are currently no committed `_test.go` files. For now:
- Run `go build ./...` as a required sanity check.
- Manually validate changed flows (for example daemon start/stop and sync output).

When adding tests, use Go’s `testing` package and name files `*_test.go` next to the code they cover.

## Commit & Pull Request Guidelines
Git history uses Conventional Commit style (examples: `feat: ...`, `feat(install): ...`).

- Commit format: `<type>(optional-scope): <short summary>`.
- Keep commits focused on one logical change.
- PRs should include:
  - what changed and why,
  - commands run for verification,
  - any config/env impacts (tokens, server URL, daemon behavior).

## Security & Configuration Tips
- Never commit real device tokens or secrets.
- Prefer environment variables for local credentials when possible.
- Validate server URL changes carefully; sync posts to `POST /v1/sync`.
