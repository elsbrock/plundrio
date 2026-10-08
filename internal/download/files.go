package download

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
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
	mu       sync.RWMutex
}

func newTransferFileStore(targetDir string) *TransferFileStore {
	return &TransferFileStore{stateDir: filepath.Join(targetDir, transferFilesStateDirName)}
}

func (fs *TransferFileStore) path(transferID int64) string {
	return filepath.Join(fs.stateDir, strconv.FormatInt(transferID, 10)+".json")
}

// New downloads record their actual root explicitly. Existing arrays are read
// in place and are never rewritten just because a remote name changed.
func (fs *TransferFileStore) setManifest(manifest LocalManifest) error {
	data, err := json.Marshal(struct {
		Version int `json:"version"`
		LocalManifest
	}{Version: 1, LocalManifest: manifest})
	if err != nil {
		return err
	}
	return fs.write(manifest.TransferID, data)
}

// Ownership evidence is published by rename, never by truncating the record in
// place: a reader or a crash during the write must still find a complete
// manifest, since a partial one reads as corrupt and blocks every transfer.
func (fs *TransferFileStore) write(transferID int64, data []byte) error {
	if err := os.MkdirAll(fs.stateDir, 0700); err != nil {
		return fmt.Errorf("create transfer file state: %w", err)
	}
	file, err := os.CreateTemp(fs.stateDir, ".manifest-*")
	if err != nil {
		return fmt.Errorf("write transfer file state: %w", err)
	}
	defer func() { _ = os.Remove(file.Name()) }()
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return fmt.Errorf("write transfer file state: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("write transfer file state: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("write transfer file state: %w", err)
	}
	if err := os.Rename(file.Name(), fs.path(transferID)); err != nil {
		return fmt.Errorf("write transfer file state: %w", err)
	}
	return nil
}

func (fs *TransferFileStore) loadManifest(transferID int64) (LocalManifest, error) {
	manifest := LocalManifest{TransferID: transferID}
	if transferID <= 0 {
		return manifest, fmt.Errorf("invalid manifest transfer ID %d", transferID)
	}
	root, err := os.OpenRoot(filepath.Dir(fs.stateDir))
	if os.IsNotExist(err) {
		return manifest, nil
	}
	if err != nil {
		return manifest, fmt.Errorf("open transfer file state root: %w", err)
	}
	defer func() { _ = root.Close() }()
	path := filepath.Join(transferFilesStateDirName, strconv.FormatInt(transferID, 10)+".json")
	// The state directory and manifest must themselves be real local entries.
	for _, component := range []string{transferFilesStateDirName, path} {
		info, err := root.Lstat(component)
		if os.IsNotExist(err) {
			return manifest, nil
		}
		if err != nil {
			return manifest, fmt.Errorf("stat transfer file state: %w", err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return manifest, fmt.Errorf("symlink in transfer file state %q", component)
		}
	}
	data, err := root.ReadFile(path)
	if err != nil {
		return manifest, fmt.Errorf("read transfer file state: %w", err)
	}
	if bytes.HasPrefix(bytes.TrimSpace(data), []byte("[")) {
		err = json.Unmarshal(data, &manifest.Files)
	} else {
		var stored struct {
			Version int `json:"version"`
			LocalManifest
		}
		err = json.Unmarshal(data, &stored)
		if err == nil && (stored.Version != 1 || stored.TransferID != transferID || stored.LocalRoot == "") {
			err = fmt.Errorf("invalid manifest version, transfer ID, or local root")
		}
		manifest = stored.LocalManifest
	}
	if err != nil {
		return manifest, fmt.Errorf("parse transfer file state: %w", err)
	}
	if len(manifest.Files) == 0 {
		return manifest, fmt.Errorf("transfer file manifest is empty")
	}
	return manifest, nil
}

// Remove deletes a transfer's manifest after the Transmission client removes it.
func (fs *TransferFileStore) Remove(transferID int64) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	err := os.Remove(fs.path(transferID))
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove transfer file state: %w", err)
	}
	return nil
}
