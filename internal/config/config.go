package config

import "time"

// DownloadStartWindowConfig gates when new local downloads may begin.
// It only affects the start of local downloads, not ongoing transfers.
type DownloadStartWindowConfig struct {
	Enabled bool
	Start   string
	End     string
}

// Config holds the runtime configuration
type Config struct {
	// TargetDir is where completed downloads will be stored
	TargetDir string

	// PutioFolder is the name of the folder in Put.io
	PutioFolder string

	// FolderID is the Put.io folder ID (set after creation/lookup)
	FolderID int64

	// OAuthToken is the Put.io OAuth token
	OAuthToken string

	// ListenAddr is the address to listen for transmission-rpc requests
	ListenAddr string

	// WorkerCount is the number of concurrent download workers (default: 4)
	WorkerCount int

	// StalledTransferTimeout is how long a downloading Put.io transfer may make
	// no byte progress before it is reported as stalled. Zero disables detection.
	StalledTransferTimeout time.Duration

	// DownloadStartWindow optionally restricts when new local downloads may start.
	DownloadStartWindow DownloadStartWindowConfig

	// UseCategoriesTarget, when true, places local downloads into a per-category
	// subfolder of TargetDir derived from the *arr "download-dir" (e.g. a
	// download-dir of "/downloads/tv" with TargetDir "/downloads" lands files in
	// "/downloads/tv"). When false (the default), all files go directly into
	// TargetDir regardless of the requested category.
	UseCategoriesTarget bool

	// UseCategoriesPutio, when true, creates a per-category subfolder under the
	// configured Put.io folder and uploads transfers into it (e.g. category "tv"
	// uploads into "<folder>/tv"). When false (the default), all transfers are
	// added directly to the configured folder.
	UseCategoriesPutio bool

	// If KeepSeeding is true, plundrio leaves the source file on Put.io after
	// the local download, so Put.io keeps seeding. Plundrio then reports a
	// transfer as removable to the *arr apps only when Put.io reports it
	// COMPLETED (seeding finished). The torrent-remove call of the *arr app
	// deletes the Put.io file and the transfer. If it is false (the default),
	// plundrio deletes the source file when the local copy is complete, and
	// seeding stops at that time.
	KeepSeeding bool

	// KeepSeedingTimeout limits how long a transfer seeds on Put.io after it
	// finished, when KeepSeeding is on. After that time, plundrio reports the
	// transfer as removable even if Put.io still seeds it. The *arr app then
	// sends torrent-remove. Zero disables the limit. It has no effect when
	// KeepSeeding is off.
	KeepSeedingTimeout time.Duration
}
