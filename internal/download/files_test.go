package download

import (
	"os"
	"reflect"
	"testing"
)

func TestTransferFileStorePersists(t *testing.T) {
	dir := t.TempDir()
	store := newTransferFileStore(dir)
	want := []TransferFile{{Name: "Book/book.m4b", Length: 42}}
	if err := store.Set(101, want); err != nil {
		t.Fatal(err)
	}

	got, ok := newTransferFileStore(dir).Get(101)
	if !ok {
		t.Fatal("persisted transfer file manifest was not loaded")
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("manifest = %+v, want %+v", got, want)
	}
}

func TestTransferFileStoreRemovePersists(t *testing.T) {
	dir := t.TempDir()
	store := newTransferFileStore(dir)
	if err := store.Set(101, []TransferFile{{Name: "Book/book.m4b", Length: 42}}); err != nil {
		t.Fatal(err)
	}
	if err := store.Remove(101); err != nil {
		t.Fatal(err)
	}

	if _, ok := newTransferFileStore(dir).Get(101); ok {
		t.Fatal("removed transfer file manifest was restored")
	}
}

func TestTransferFileStoreRejectsEmptyManifest(t *testing.T) {
	store := newTransferFileStore(t.TempDir())
	if err := store.Set(101, nil); err == nil {
		t.Fatal("expected empty manifest to fail")
	}
	if _, err := os.Stat(store.path(101)); !os.IsNotExist(err) {
		t.Fatalf("empty manifest created state file: %v", err)
	}
}
