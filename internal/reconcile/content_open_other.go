//go:build !unix

package reconcile

import (
	"io/fs"
	"os"
)

func localContentFS(root *os.Root) fs.FS { return root.FS() }
