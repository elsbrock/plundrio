package reconcile

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// Only unmanaged branches are read. Active downloads may be changing and must
// not turn a report into a full read of in-flight payloads.
func fingerprintUnmanaged(ctx context.Context, rootPath string, nodes []*localNode, active map[string]struct{}) error {
	if len(nodes) == 0 {
		return nil
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return err
	}
	defer root.Close()
	var visit func([]*localNode) error
	visit = func(nodes []*localNode) error {
		for _, node := range nodes {
			if _, ok := active[node.relPath]; ok {
				continue
			}
			if localTreeContainsActive(node.relPath, active) {
				if err := visit(node.children); err != nil {
					return err
				}
			} else if err := fingerprintLocalTree(ctx, localContentFS(root), node); err != nil {
				return err
			}
		}
		return nil
	}
	return visit(nodes)
}

func fingerprintLocalTree(ctx context.Context, root fs.FS, node *localNode) error {
	for _, child := range node.children {
		if err := fingerprintLocalTree(ctx, root, child); err != nil {
			return err
		}
	}
	id, err := contentLocalID(ctx, root, node)
	if err != nil {
		return err
	}
	node.object.ID = id
	return nil
}

// Stat identity alone is insufficient on filesystems that reuse an inode and
// timestamps for a same-size replacement. Bind regular files to their bytes;
// directory IDs then bind the whole tree through their children's IDs.
func contentLocalID(ctx context.Context, root fs.FS, node *localNode) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	metadataID := localID(node.relPath, node.info, node.children)
	hash := sha256.New()
	fmt.Fprintf(hash, "local-content-v1\x00%s\x00", metadataID)
	if node.info.Mode().IsRegular() {
		rel := filepath.ToSlash(node.relPath)
		file, err := root.Open(rel)
		if err != nil {
			return "", err
		}
		defer file.Close()
		check := func(info fs.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if !info.Mode().IsRegular() || localID(node.relPath, info, nil) != metadataID {
				return fmt.Errorf("local object %q changed while fingerprinting", rel)
			}
			return nil
		}
		if err := check(file.Stat()); err != nil {
			return "", err
		}
		// Bound reads to the snapshot size; a growing file cannot prolong the
		// scan indefinitely. Check cancellation between reads of large files.
		if _, err := io.CopyN(hash, contextReader{ctx, file}, node.info.Size()); err != nil {
			return "", fmt.Errorf("read local object %q: %w", rel, err)
		}
		var extra [1]byte
		if n, err := file.Read(extra[:]); n != 0 || err != io.EOF {
			return "", fmt.Errorf("local object %q changed or could not be fully read", rel)
		}
		if err := check(file.Stat()); err != nil {
			return "", err
		}
		if err := check(fs.Lstat(root, rel)); err != nil {
			return "", err
		}
	} else if node.info.IsDir() {
		// Child reads can take a long time. Do not accept a directory assembled
		// from an old listing if new, unselected entries appeared meanwhile.
		rel := filepath.ToSlash(node.relPath)
		entries, err := fs.ReadDir(root, rel)
		if err != nil {
			return "", err
		}
		if len(entries) != len(node.children) {
			return "", fmt.Errorf("local directory %q changed while fingerprinting", rel)
		}
		for i, entry := range entries {
			if entry.Name() != filepath.Base(node.children[i].relPath) {
				return "", fmt.Errorf("local directory %q changed while fingerprinting", rel)
			}
		}
		info, err := fs.Lstat(root, rel)
		if err != nil {
			return "", err
		}
		if localID(node.relPath, info, node.children) != metadataID {
			return "", fmt.Errorf("local directory %q changed while fingerprinting", rel)
		}
	}
	return fmt.Sprintf("local:%x", hash.Sum(nil)), nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}
