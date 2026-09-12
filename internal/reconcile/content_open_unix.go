//go:build unix

package reconcile

import (
	"io/fs"
	"os"
	"syscall"
)

type contentRoot struct{ *os.Root }

func (r contentRoot) ReadLink(name string) (string, error) { return r.Readlink(name) }

func (r contentRoot) Open(name string) (fs.File, error) {
	// A regular file may have become a FIFO or symlink since enumeration.
	// Never block opening it; contentLocalID checks the opened descriptor type.
	return r.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
}

func localContentFS(root *os.Root) fs.FS { return contentRoot{root} }
