package download

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCategoryStore_SetGetRemove(t *testing.T) {
	dir := t.TempDir()
	cs := newCategoryStore(dir)

	// Get on empty store returns ""
	if got := cs.Get("abc123"); got != "" {
		t.Errorf("Get on empty store = %q, want %q", got, "")
	}

	// Set and Get
	cs.Set("abc123", "tv")
	if got := cs.Get("abc123"); got != "tv" {
		t.Errorf("Get after Set = %q, want %q", got, "tv")
	}

	// Overwrite
	cs.Set("abc123", "movies")
	if got := cs.Get("abc123"); got != "movies" {
		t.Errorf("Get after overwrite = %q, want %q", got, "movies")
	}

	// Remove
	cs.Remove("abc123")
	if got := cs.Get("abc123"); got != "" {
		t.Errorf("Get after Remove = %q, want %q", got, "")
	}
}

func TestCategoryStore_SetIgnoresEmpty(t *testing.T) {
	dir := t.TempDir()
	cs := newCategoryStore(dir)

	cs.Set("", "tv")
	cs.Set("abc", "")

	// State file should not exist since nothing was persisted
	if _, err := os.Stat(filepath.Join(dir, stateFileName)); !os.IsNotExist(err) {
		t.Error("expected no state file for empty hash/category Set calls")
	}
}

func TestCategoryStore_Persistence(t *testing.T) {
	dir := t.TempDir()

	// Create and populate a store
	cs1 := newCategoryStore(dir)
	cs1.Set("hash1", "tv")
	cs1.Set("hash2", "movies")

	// Create a new store pointing at the same dir and load
	cs2 := newCategoryStore(dir)
	cs2.Load()

	if got := cs2.Get("hash1"); got != "tv" {
		t.Errorf("After reload Get(hash1) = %q, want %q", got, "tv")
	}
	if got := cs2.Get("hash2"); got != "movies" {
		t.Errorf("After reload Get(hash2) = %q, want %q", got, "movies")
	}
}

func TestCategoryStore_LoadMissingFile(t *testing.T) {
	dir := t.TempDir()
	cs := newCategoryStore(dir)

	// Load on missing file should not panic or error
	cs.Load()

	if got := cs.Get("anything"); got != "" {
		t.Errorf("Get after Load of missing file = %q, want %q", got, "")
	}
}

func TestCategoryStore_RemovePersists(t *testing.T) {
	dir := t.TempDir()

	cs1 := newCategoryStore(dir)
	cs1.Set("hash1", "tv")
	cs1.Set("hash2", "movies")
	cs1.Remove("hash1")

	cs2 := newCategoryStore(dir)
	cs2.Load()

	if got := cs2.Get("hash1"); got != "" {
		t.Errorf("After reload removed hash1 = %q, want %q", got, "")
	}
	if got := cs2.Get("hash2"); got != "movies" {
		t.Errorf("After reload hash2 = %q, want %q", got, "movies")
	}
}
