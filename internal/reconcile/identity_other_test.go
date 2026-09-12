//go:build !unix

package reconcile

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLocalDeleteFailsClosedWithoutUnixIdentity(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "keep"), "keep")
	err := deleteLocalObject(context.Background(), root, Object{Path: "keep"}, nil)
	if err == nil || !strings.Contains(err.Error(), "requires Unix filesystem identity") {
		t.Fatalf("unsupported mutation = %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "keep")); err != nil {
		t.Fatal(err)
	}
}
