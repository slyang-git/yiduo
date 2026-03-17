# Changelog

All notable changes to this project will be documented in this file.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.0.0/) conventions.
Versions follow `v0.0.X` incremental tagging. Update this file when cutting a new release tag.

---

## [v0.0.47] - 2026-03-13

### Fixed
- Corrected build configuration for Windows release assets

---

## [v0.0.46] - 2026-03-13

### Added
- Windows platform release support (new OS target in CI)

---

## [v0.0.45] / [v0.0.44] - 2026-03-09

### Fixed
- Used `strings.CutPrefix` instead of `strings.TrimPrefix` for project key handling to avoid silent mismatch when prefix is absent

---

## [v0.0.43] - 2026-03-09

### Fixed
- Resolved merge conflict; added `gooseRoot` and `kimiRoot` to `baseParams` so Goose and Kimi Code paths are correctly passed through

---

## [v0.0.42] - 2026-03-08

### Added
- Sync now includes agent and tool version metadata per session

---

## [v0.0.41] - 2026-03-08

### Fixed
- `status` command now correctly reports installed sources discovered via local probes

---

## [v0.0.40] - 2026-03-05

### Fixed
- Normalized `tool_result` message roles consistently across all parsers

---

## [v0.0.39] - 2026-03-05

### Added
- New session source parser for **Qoder**

---

## [v0.0.38] - 2026-03-05

### Added
- Device metadata sync: kernel version, timezone, memory size, disk size, and daemon metadata

---

## [v0.0.37] - 2026-03-05

### Added
- Device metadata sync: OS version and CPU information

---

## [v0.0.36] - 2026-03-05

### Fixed
- Daemon now sends a heartbeat even when a sync cycle produces zero sessions, preventing stale last-seen timestamps

---

## [v0.0.35] - 2026-03-04

### Added
- `status` output now shows the last successful sync time

---

## [v0.0.34] - 2026-03-04

### Added
- Cursor transcript sessions support in sync
- Daemon hot-restart capability (reload config without full stop/start)

---

## [v0.0.33] - 2026-03-04

### Added
- New session source parser for **Crush**

---

## [v0.0.32] - 2026-03-03

### Changed
- Improved visual formatting of `status` and sync log CLI output

---

## [v0.0.31] - 2026-03-03

### Added
- `status` output now includes install path and config file path

---

## [v0.0.30] - 2026-03-03

### Changed
- `status` command promoted to a top-level command
- Enhanced daemon status output with richer detail

---

## [v0.0.29] - 2026-03-03

### Added
- `sync log` subcommand to display recent sync history
- Improved `--help` output across commands

---

## [v0.0.28] - 2026-03-02

### Added
- New session source: **OpenClaw**

---

## [v0.0.27] - 2026-02-28

### Added
- **Kilocode** integration: sync sessions from SQLite storage and select the dominant model per session

---

## [v0.0.26] - 2026-02-12

### Added
- Version string embedded in binary at build time
- `version` command to print current version

---

## [v0.0.25] - 2026-02-12

### Added
- Glob-based path discovery for VS Code variant installations (Insiders, Codium, etc.)

---

## [v0.0.24] - 2026-02-12

### Changed
- Removed install script from release artifacts (CI cleanup)

---

## [v0.0.23] - 2026-02-12

### Changed
- Migrated from device token to auth token with backward-compatible fallback

---

## [v0.0.22] - 2026-02-11

### Added
- Daemon restart command
- Improved `status` output formatting

---

## [v0.0.21] - 2026-02-08

### Added
- New session source: **PI**
- Improved session loading for **Qwen**, **Amp**, and **OpenCode**

---

## [v0.0.20] - 2026-02-03

### Added
- Support for loading **Cursor** chat sessions

---

## [v0.0.19] - 2026-01-30

### Added
- Structured tool-use parsing in message content (tool calls and results now captured as typed objects)

---

## [v0.0.18] - 2026-01-30

### Added
- Per-message token tracking for **Claude** and **Gemini** sessions

---

## [v0.0.17] - 2026-01-29

### Removed
- Screenshot gallery feature

---

## [v0.0.16] - 2026-01-29

### Added
- `start` command as an alias / entry point for daemon mode
- Sync start event logged on daemon startup

---

## [v0.0.15] - 2026-01-29

### Added
- Logging for Claude session sync operations
- Filtering to skip non-user-generated Claude sessions

---

## [v0.0.14] - 2026-01-29

### Fixed
- Filtered out synthetic (auto-generated) Claude sessions from sync payload

---

## [v0.0.13] - 2026-01-28

### Added
- Per-user sync token and device ID support
- Improved installation output with colored messages and clearer prompts

---

## [v0.0.12] - 2026-01-26

### Added
- Device ID generation and persistent tracking across syncs

---

## [v0.0.11] - 2026-01-25

### Added
- Daemon logging with daily log-file rotation for analytics

---

## [v0.0.10] - 2026-01-24

### Changed
- Added `cache-dependency-path` to CI workflow for proper Go module caching

---

## [v0.0.9] - 2026-01-24

### Changed
- Updated release workflow to use yiduo-specific artifact naming

---

## [v0.0.8] - 2026-01-24

### Changed
- Refactored release workflow to upload build artifacts for improved analytics tracking

---

## [v0.0.7] - 2026-01-24

### Added
- `daemon status` and `daemon stop` commands

---

## [v0.0.6] - 2026-01-24

### Added
- Daemon mode: periodic background sync with incremental (delta) updates

---

## [v0.0.5] - 2026-01-24

### Changed
- Installation switched from building from source to downloading precompiled binaries

---

## [v0.0.4] - 2026-01-24

### Added
- `sync` subcommand
- Config file support for persistent settings

---

## [v0.0.3] - 2026-01-23

### Added
- Shell install script (`install.sh`)

---

## [v0.0.2] - 2026-01-23

### Changed
- Minor GitHub Actions workflow adjustments

---

## [v0.0.1] - 2026-01-23

### Added
- Initial release workflow and installation script
