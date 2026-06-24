# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What is plundrio?

plundrio is a put.io download client that integrates with the *arr stack (Sonarr, Radarr, Lidarr) by implementing the Transmission RPC protocol. It acts as a bridge: *arr apps send download requests via Transmission RPC, plundrio uploads torrents/magnets to put.io, monitors transfers, downloads completed files locally, and reports progress back to the *arr apps.

## Build & CI

This project uses **Nix flakes** exclusively for building — there is no Makefile or goreleaser.

```bash
# Build native binary
nix build .#plundrio

# Build for aarch64
nix build .#plundrio-aarch64

# Build Docker images
nix build .#plundrio-docker
nix build .#plundrio-docker-aarch64

# Enter dev shell (Go, gopls, golangci-lint)
nix develop

# Run directly with Go (during development)
go build ./cmd/plundrio && ./plundrio run --help
```

**CI** (`.github/workflows/build.yml`) runs `nix build` for all four targets (native, aarch64, docker, docker-aarch64) on every push/PR.

**Important**: When Go dependencies change (`go.mod`/`go.sum`), the `vendorHash` in `flake.nix` (line 169) must be updated. Build the project with Nix; the error message will contain the correct hash.

## Architecture

```
cmd/plundrio/main.go    Entry point, CLI (cobra + viper), wires everything together
internal/
  config/               Config struct (TargetDir, FolderID, OAuthToken, ListenAddr, WorkerCount, UseCategoriesTarget, UseCategoriesPutio)
  api/                  Put.io API client wrapper (uploads, transfers, files, auth)
  server/               Transmission RPC server (HTTP on :9091)
  download/             Download manager, transfer coordinator, worker pool
  log/                  Zerolog wrapper with component-based logging
```

### Request Flow

1. **Inbound**: *arr app sends Transmission RPC to `server/handlers.go` which routes to `torrent.go` handlers
2. **torrent-add**: Uploads `.torrent` or adds magnet to put.io folder (`cfg.FolderID`)
3. **Monitoring**: `Manager.monitorTransfers()` polls put.io every 30s, `TransferProcessor.checkTransfers()` categorizes transfers by status
4. **Download**: Ready transfers get files queued as `downloadJob`s, processed by worker pool via `grab` library
5. **Coordination**: `TransferCoordinator` tracks lifecycle states (Initial -> Downloading -> Completed -> Processed), `TransferContext` holds per-transfer state
6. **Cleanup**: On completion, cleanup hook deletes source file from put.io but keeps transfer record for *arr visibility
7. **torrent-remove**: *arr app requests removal; plundrio deletes put.io file + transfer

### Category Subfolders

*arr apps send the requested path in the Transmission `torrent-add` argument `download-dir` (kebab-case — note `torrent-get` *responses* use camelCase `downloadDir`). `extractCategory` derives the category as the path of `download-dir` relative to `TargetDir` (e.g. `/downloads/tv` → `tv`). The category is stored in `CategoryStore` keyed by the **put.io transfer ID** (not the hash, which is empty for freshly-added magnets). Two independent opt-in config flags, both default off:

- `UseCategoriesTarget`: local downloads go to `TargetDir/<category>/...` (via `Manager.localCategory`).
- `UseCategoriesPutio`: transfers are uploaded into a `<folder>/<category>` subfolder on put.io (`Server.putioFolderForCategory`, cached in `Server.catFolders`). The monitor then treats the configured folder *and* its direct subfolders as managed (`TransferProcessor.managedFolders`). Single-level categories only; empty subfolders are left in place.

### Progress Reporting

Progress is split 50/50: put.io download (0-50%) + local download (50-100%). This is calculated in `handleTorrentGet` and reported via standard Transmission fields.

### Transfer Lifecycle States

`TransferLifecycleState` in `types.go`: Initial -> Downloading -> Completed -> Processed (or Failed/Cancelled). The "Processed" state means files are downloaded and put.io source cleaned up; the transfer record stays for *arr to query until `torrent-remove`.

### Key Types

- `Manager` (`manager.go`): Orchestrates workers, monitor loop, coordinator
- `TransferCoordinator` (`coordinator.go`): State machine for transfer lifecycle, cleanup hooks
- `TransferProcessor` (`transfers.go`): Categorizes and processes put.io transfers, handles retries
- `TransferContext` (`types.go`): Per-transfer state (files, progress, bytes)
- `Server` (`server.go`): HTTP server implementing Transmission RPC subset

## Configuration

Environment prefix: `PLDR_` (e.g., `PLDR_TOKEN`, `PLDR_TARGET`, `PLDR_FOLDER`). Config file via `--config`. Flags override env vars override config file.

## Testing

Run tests with `go test ./...`. Existing coverage: `internal/download` (category store, transfer monitor, errors, window) and `internal/server` (torrent-add category handling, local-data deletion, progress, extract-category).

## Known Issues

- `GetTransfers()` filters transfers by managed folder. With `UseCategoriesPutio` off it accepts only `SaveParentID == folderID`, so externally-added transfers in other folders are invisible (#17). With `UseCategoriesPutio` on it also accepts the folder's direct subfolders (single level only).
