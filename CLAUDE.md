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

**Important**: Dependencies are pinned by `gomod2nix.toml`, not a `vendorHash`. When `go.mod`/`go.sum` change, regenerate it with `nix develop --command gomod2nix generate` and commit the result, or the Nix build will use stale dependencies.

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
7. **torrent-remove**: *arr app requests removal; plundrio deletes put.io file + transfer, optionally deletes local files, and drops all local tracking for that transfer

### Concurrency Model

Three goroutine roles, and shutdown drains them in dependency order:

1. the **monitor** (one goroutine, `monitorWg`) polls put.io and spawns
2. **transfer processors** (one per ready transfer, `processorWg`), which queue jobs for
3. **download workers** (`WorkerCount`, `workerWg`), which consume `m.jobs`

`Manager.Stop()` cancels the context, closes `stopChan`, then waits
monitor → processors → workers. Waiting in any other order lets a
`WaitGroup.Add` race a `Wait`. The `jobs` channel is deliberately never
closed — a send on a closed channel panics even when the `stopChan` case of a
`select` is also ready. Nothing may block on a channel send while holding
`m.mu`, since `Stop` needs that mutex.

### Category Subfolders

*arr apps send the requested path in the Transmission `torrent-add` argument `download-dir` (kebab-case — note `torrent-get` *responses* use camelCase `downloadDir`). `extractCategory` derives the category as the path of `download-dir` relative to `TargetDir` (e.g. `/downloads/tv` → `tv`). The category is stored in `CategoryStore` keyed by the **put.io transfer ID** (not the hash, which is empty for freshly-added magnets). Two independent opt-in config flags, both default off:

- `UseCategoriesTarget`: local downloads go to `TargetDir/<category>/...` (via `Manager.localCategory`).
- `UseCategoriesPutio`: transfers are uploaded into a `<folder>/<category>` subfolder on put.io (`Server.putioFolderForCategory`, cached in `Server.catFolders`). The monitor then treats the configured folder *and* its direct subfolders as managed (`TransferProcessor.managedFolders`). Single-level categories only; empty subfolders are left in place.

### Progress Reporting

Progress is split 50/50: put.io download (0-50%) + local download (50-100%). This is calculated in `handleTorrentGet` and reported via standard Transmission fields.

### Transfer Lifecycle States

`TransferLifecycleState` in `types.go`: Initial -> Downloading -> Completed -> Processed (or Failed/Cancelled). The "Processed" state means files are downloaded and put.io source cleaned up; the transfer record stays for *arr to query until `torrent-remove`, which is what finally removes it via `Manager.RemoveTransfer`.

A `Failed` transfer keeps its context only until the next poll: `shouldProcess`
drops it so the transfer is retried, bounded by `maxReprocessAttempts` and
deferred until no files are still in flight. Contexts are otherwise never
removed, so `torrent-remove` is what keeps tracking state from growing for the
process lifetime.

`NeedsReview` means source absence without a manifest, never verified completion
or download failure. `.plundrio-files/<id>.review.json` holds original identity
across restarts and remote status changes; load holds before status dispatch.
Do not auto-retry, complete, infer ownership, or ordinarily remove held records.
Only explicit exact-ID reviewed retirement with retained-copy acknowledgment
may remove the transfer record (never file data), after revalidation. Preserve
the review marker through failed retirement and remove it with bookkeeping only
after confirmed remote deletion. Missing child-listing404 is not root absence.

`torrent-remove` first persists a disk-only `.plundrio-files/<id>.removing.json`
marker and releases the coordinator, retry, and active category records. Marked
IDs are excluded from processing after restart and remain visible as stopped
with an actionable removal error. The marker stores the category while the
existing manifest preserves file ownership. Remote deletion has three attempts
per request; failures retain disk evidence, not an in-memory tombstone map.
Markers/manifests are reclaimed on deletion success or confirmed absence in a
successful full account listing, after active workers drain. Delayed retries
must still reference the same coordinator context before enqueueing, since a
marker can be reclaimed before a retry timer fires.

### Key Types

- `Manager` (`manager.go`): Orchestrates workers, monitor loop, coordinator
- `TransferCoordinator` (`coordinator.go`): State machine for transfer lifecycle, cleanup hooks
- `TransferProcessor` (`transfers.go`): Categorizes and processes put.io transfers, handles retries
- `TransferContext` (`types.go`): Per-transfer state (files, progress, bytes)
- `Server` (`server.go`): HTTP server implementing Transmission RPC subset

## Configuration

Environment prefix: `PLDR_` (e.g., `PLDR_TOKEN`, `PLDR_TARGET`, `PLDR_FOLDER`). Config file via `--config`. Flags override env vars override config file.

## Testing

```bash
go test ./...          # unit tests
go test -race ./...    # required: most defects here are concurrency bugs
```

Coverage: `internal/download` (category store, transfer monitor, shutdown,
byte accounting, errors, window) and `internal/server` (torrent-add category
handling, local-data deletion, progress, extract-category).

Concurrency fixes carry regression tests that were each confirmed to fail
against the previous implementation — keep that habit, since a test that
passes either way proves nothing. See
`internal/download/download_integration_test.go` for the pattern: handlers
pace their writes so the progress ticker genuinely fires, and
`requireProgressWasReported` guards against a vacuously passing test.

## Known Issues

- `GetTransfers()` filters transfers by managed folder. With `UseCategoriesPutio` off it accepts only `SaveParentID == folderID`, so externally-added transfers in other folders are invisible (#17). With `UseCategoriesPutio` on it also accepts the folder's direct subfolders (single level only).
- The RPC server has no authentication and reports a static session ID; it assumes a trusted network
- Files from nested put.io folders are flattened into `TargetDir/<category>/<transfer>/`, so identically named files in different subfolders collide
