package download

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"

	"github.com/elsbrock/plundrio/internal/log"
)

const stateFileName = ".plundrio-state.json"

// CategoryStore persists a put.io transfer ID → category mapping so that
// downloads land in the correct sub-directory (e.g. "tv", "movies") even across
// restarts. The transfer ID is used as the key (rather than the torrent hash)
// because it is assigned by put.io immediately when a transfer is added, while
// the hash may still be empty for freshly-added magnet links.
type CategoryStore struct {
	mu        sync.RWMutex
	mapping   map[int64]string
	stateFile string
}

func newCategoryStore(targetDir string) *CategoryStore {
	return &CategoryStore{
		mapping:   make(map[int64]string),
		stateFile: filepath.Join(targetDir, stateFileName),
	}
}

// Load reads persisted categories from disk. A missing file is not an error.
func (cs *CategoryStore) Load() {
	data, err := os.ReadFile(cs.stateFile)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Error("categories").Err(err).Msg("Failed to load category state")
		}
		return
	}

	cs.mu.Lock()
	defer cs.mu.Unlock()

	if err := json.Unmarshal(data, &cs.mapping); err != nil {
		log.Error("categories").Err(err).Msg("Failed to parse category state")
	}
}

// Set stores a category for the given transfer ID and persists to disk.
func (cs *CategoryStore) Set(transferID int64, category string) {
	if transferID == 0 || category == "" {
		return
	}

	cs.mu.Lock()
	cs.mapping[transferID] = category
	cs.mu.Unlock()

	cs.save()
}

// Get returns the category for a transfer ID, or "" if none is stored.
func (cs *CategoryStore) Get(transferID int64) string {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	return cs.mapping[transferID]
}

// Remove deletes the category for a transfer ID and persists to disk.
func (cs *CategoryStore) Remove(transferID int64) {
	cs.mu.Lock()
	delete(cs.mapping, transferID)
	cs.mu.Unlock()

	cs.save()
}

func (cs *CategoryStore) save() {
	cs.mu.RLock()
	data, err := json.Marshal(cs.mapping)
	cs.mu.RUnlock()

	if err != nil {
		log.Error("categories").Err(err).Msg("Failed to marshal category state")
		return
	}

	if err := os.WriteFile(cs.stateFile, data, 0644); err != nil {
		log.Error("categories").Err(err).Msg("Failed to save category state")
	}
}
