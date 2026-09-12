package download

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/elsbrock/plundrio/internal/log"
)

const transferFilesStateDirName = ".plundrio-files"

// IsReservedTransferName reports whether name would occupy Plundrio's
// manifest directory in the download root.
func IsReservedTransferName(name string) bool {
	return strings.EqualFold(filepath.Clean(name), transferFilesStateDirName)
}

// TransferFile describes one local file belonging to a Put.io transfer. Name
// is relative to the transfer's reported Transmission downloadDir.
type TransferFile struct {
	Name   string `json:"name"`
	Length int64  `json:"length"`
}

// TransferFileStore preserves one authoritative manifest per transfer after
// Plundrio deletes the source file from Put.io.
type TransferFileStore struct {
	stateDir string
}

func newTransferFileStore(targetDir string) *TransferFileStore {
	return &TransferFileStore{stateDir: filepath.Join(targetDir, transferFilesStateDirName)}
}

func (fs *TransferFileStore) path(transferID int64) string {
	return filepath.Join(fs.stateDir, strconv.FormatInt(transferID, 10)+".json")
}

// Set stores a transfer's complete expected file list. If interrupted, Put.io
// still owns the source and the next poll rewrites the manifest before cleanup.
func (fs *TransferFileStore) Set(transferID int64, files []TransferFile) error {
	if transferID <= 0 || len(files) == 0 {
		return fmt.Errorf("transfer file manifest requires a transfer ID and at least one file")
	}
	if err := os.MkdirAll(fs.stateDir, 0700); err != nil {
		return fmt.Errorf("create transfer file state: %w", err)
	}
	data, err := json.Marshal(files)
	if err != nil {
		return fmt.Errorf("marshal transfer file state: %w", err)
	}
	if err := os.WriteFile(fs.path(transferID), data, 0600); err != nil {
		return fmt.Errorf("write transfer file state: %w", err)
	}
	return nil
}

func (fs *TransferFileStore) Get(transferID int64) ([]TransferFile, bool) {
	files, err := fs.load(transferID)
	if err != nil {
		log.Error("files").Err(err).Msg("Failed to load transfer file state")
		return nil, false
	}
	return files, len(files) > 0
}

// load returns nil only for genuinely absent legacy state. Existing but
// unreadable, malformed, or empty manifests must not authorize legacy completion.
func (fs *TransferFileStore) load(transferID int64) ([]TransferFile, error) {
	data, err := os.ReadFile(fs.path(transferID))
	if err != nil {
		if os.IsNotExist(err) {
			// A dangling symlink is existing but unreadable evidence, not an
			// absent legacy manifest. Only genuine path absence allows review.
			if _, statErr := os.Lstat(fs.path(transferID)); os.IsNotExist(statErr) {
				return nil, nil
			}
		}
		return nil, fmt.Errorf("read transfer file state: %w", err)
	}
	var files []TransferFile
	if err := json.Unmarshal(data, &files); err != nil {
		return nil, fmt.Errorf("parse transfer file state: %w", err)
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("transfer file manifest is empty")
	}
	return files, nil
}

// Remove deletes a transfer's manifest after the Transmission client removes it.
func (fs *TransferFileStore) Remove(transferID int64) error {
	err := os.Remove(fs.path(transferID))
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove transfer file state: %w", err)
	}
	return nil
}
