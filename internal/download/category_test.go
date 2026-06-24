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
	if got := cs.Get(123); got != "" {
		t.Errorf("Get on empty store = %q, want %q", got, "")
	}

	// Set and Get
	cs.Set(123, "tv")
	if got := cs.Get(123); got != "tv" {
		t.Errorf("Get after Set = %q, want %q", got, "tv")
	}

	// Overwrite
	cs.Set(123, "movies")
	if got := cs.Get(123); got != "movies" {
		t.Errorf("Get after overwrite = %q, want %q", got, "movies")
	}

	// Remove
	cs.Remove(123)
	if got := cs.Get(123); got != "" {
		t.Errorf("Get after Remove = %q, want %q", got, "")
	}
}

func TestCategoryStore_SetIgnoresEmpty(t *testing.T) {
	dir := t.TempDir()
	cs := newCategoryStore(dir)

	cs.Set(0, "tv") // zero transfer ID
	cs.Set(123, "") // empty category

	// State file should not exist since nothing was persisted
	if _, err := os.Stat(filepath.Join(dir, stateFileName)); !os.IsNotExist(err) {
		t.Error("expected no state file for empty id/category Set calls")
	}
}

func TestCategoryStore_Persistence(t *testing.T) {
	dir := t.TempDir()

	// Create and populate a store
	cs1 := newCategoryStore(dir)
	cs1.Set(1, "tv")
	cs1.Set(2, "movies")

	// Create a new store pointing at the same dir and load
	cs2 := newCategoryStore(dir)
	cs2.Load()

	if got := cs2.Get(1); got != "tv" {
		t.Errorf("After reload Get(1) = %q, want %q", got, "tv")
	}
	if got := cs2.Get(2); got != "movies" {
		t.Errorf("After reload Get(2) = %q, want %q", got, "movies")
	}
}

func TestCategoryStore_LoadMissingFile(t *testing.T) {
	dir := t.TempDir()
	cs := newCategoryStore(dir)

	// Load on missing file should not panic or error
	cs.Load()

	if got := cs.Get(999); got != "" {
		t.Errorf("Get after Load of missing file = %q, want %q", got, "")
	}
}

func TestCategoryStore_RemovePersists(t *testing.T) {
	dir := t.TempDir()

	cs1 := newCategoryStore(dir)
	cs1.Set(1, "tv")
	cs1.Set(2, "movies")
	cs1.Remove(1)

	cs2 := newCategoryStore(dir)
	cs2.Load()

	if got := cs2.Get(1); got != "" {
		t.Errorf("After reload removed id 1 = %q, want %q", got, "")
	}
	if got := cs2.Get(2); got != "movies" {
		t.Errorf("After reload id 2 = %q, want %q", got, "movies")
	}
}
