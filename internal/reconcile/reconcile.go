package reconcile

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/elsbrock/go-putio"
	"github.com/elsbrock/plundrio/internal/download"
)

const (
	SchemaVersion = 1
)

// Client is the read-only Put.io surface needed to reconcile download roots.
type Client interface {
	GetTransfers(context.Context) ([]*putio.Transfer, error)
	GetFiles(context.Context, int64) ([]*putio.File, error)
}

// Object is one independently classifiable entry in a download root.
type Object struct {
	ID         string `json:"id"`
	Source     string `json:"source"`
	ObjectID   *int64 `json:"object_id,omitempty"`
	Path       string `json:"path"`
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	Size       int64  `json:"size"`
	ModifiedAt string `json:"modified_at"`
}

// Roots identifies the exact roots covered by a report.
type Roots struct {
	PutioFolderID int64  `json:"putio_folder_id"`
	LocalPath     string `json:"local_path"`
}

// Summary makes scheduled count and size checks possible without reparsing objects.
type Summary struct {
	ActiveCount    int   `json:"active_count"`
	ActiveBytes    int64 `json:"active_bytes"`
	UnmanagedCount int   `json:"unmanaged_count"`
	UnmanagedBytes int64 `json:"unmanaged_bytes"`
}

// Report is deterministic for an unchanged remote and local filesystem state.
type Report struct {
	SchemaVersion int      `json:"schema_version"`
	Roots         Roots    `json:"roots"`
	Active        []Object `json:"active"`
	Unmanaged     []Object `json:"unmanaged"`
	Summary       Summary  `json:"summary"`
}

// Service compares Put.io and local download-root objects with current transfers.
type Service struct {
	client             Client
	putioFolderID      int64
	localRoot          string
	useCategoriesPutio bool
}

// New creates a read-only reconciliation service.
func New(client Client, putioFolderID int64, localRoot string, useCategoriesPutio bool) *Service {
	return &Service{client: client, putioFolderID: putioFolderID, localRoot: localRoot, useCategoriesPutio: useCategoriesPutio}
}

// Reconcile returns active and unmanaged objects without mutating either root.
func (s *Service) Reconcile(ctx context.Context) (Report, error) {
	return s.snapshot(ctx, nil)
}

// A scoped snapshot refreshes only a selected object's branch. The root-level
// Put.io listing still discovers managed category folders; unrelated subtrees
// and the other source's payloads are never walked. Only the selected ID may be
// used from this partial report.
func (s *Service) snapshot(ctx context.Context, selected *Object) (Report, error) {
	localRoot, err := filepath.Abs(s.localRoot)
	if err != nil {
		return Report{}, fmt.Errorf("resolve local root: %w", err)
	}

	remoteTree, err := s.remoteTree(ctx, selected)
	if err != nil {
		return Report{}, err
	}
	localTree, err := buildLocalTree(localRoot, selected)
	if err != nil {
		return Report{}, err
	}
	transfers, err := s.client.GetTransfers(ctx)
	if err != nil {
		return Report{}, fmt.Errorf("list transfers: %w", err)
	}
	transfers = transfersInRoot(transfers, s.putioFolderID, remoteTree, s.useCategoriesPutio)

	remoteActive := make(map[int64]struct{}, len(transfers))
	remoteActiveParents := make(map[int64]struct{}, len(transfers))
	for _, transfer := range transfers {
		if transfer.FileID != 0 {
			remoteActive[transfer.FileID] = struct{}{}
		}
		if transfer.SaveParentID != s.putioFolderID {
			remoteActiveParents[transfer.SaveParentID] = struct{}{}
		}
	}
	localActive, err := activeLocalPaths(localRoot, transfers, localTree)
	if err != nil {
		return Report{}, err
	}

	if err := fingerprintUnmanaged(ctx, localRoot, localTree, localActive); err != nil {
		return Report{}, fmt.Errorf("fingerprint unmanaged local objects: %w", err)
	}
	active, unmanaged := classifyRemote(remoteTree, remoteActive, remoteActiveParents)
	localManaged, localUnmanaged := classifyLocal(localTree, localActive)
	active = append(active, localManaged...)
	unmanaged = append(unmanaged, localUnmanaged...)
	if active == nil {
		active = []Object{}
	}
	if unmanaged == nil {
		unmanaged = []Object{}
	}
	sortObjects(active)
	sortObjects(unmanaged)

	report := Report{
		SchemaVersion: SchemaVersion,
		Roots: Roots{
			PutioFolderID: s.putioFolderID,
			LocalPath:     localRoot,
		},
		Active:    active,
		Unmanaged: unmanaged,
	}
	for _, object := range report.Active {
		report.Summary.ActiveCount++
		report.Summary.ActiveBytes += object.Size
	}
	for _, object := range report.Unmanaged {
		report.Summary.UnmanagedCount++
		report.Summary.UnmanagedBytes += object.Size
	}
	return report, nil
}

type remoteNode struct {
	object   Object
	fileID   int64
	children []*remoteNode
}

func (s *Service) remoteTree(ctx context.Context, selected *Object) ([]*remoteNode, error) {
	visited := map[int64]struct{}{s.putioFolderID: {}}
	return s.remoteChildren(ctx, s.putioFolderID, "", visited, selected)
}

func (s *Service) remoteChildren(ctx context.Context, parentID int64, parentPath string, visited map[int64]struct{}, selected *Object) ([]*remoteNode, error) {
	files, err := s.client.GetFiles(ctx, parentID)
	if err != nil {
		return nil, fmt.Errorf("list Put.io folder %d: %w", parentID, err)
	}
	sort.Slice(files, func(i, j int) bool {
		if files[i].Name == files[j].Name {
			return files[i].ID < files[j].ID
		}
		return files[i].Name < files[j].Name
	})

	nodes := make([]*remoteNode, 0, len(files))
	for _, file := range files {
		if _, ok := visited[file.ID]; ok {
			return nil, fmt.Errorf("Put.io folder tree contains repeated object ID %d", file.ID)
		}
		visited[file.ID] = struct{}{}

		remotePath := path.Join(parentPath, file.Name)
		id := file.ID
		node := &remoteNode{
			fileID: file.ID,
			object: Object{
				ID:         fmt.Sprintf("putio:%d", file.ID),
				Source:     "putio",
				ObjectID:   &id,
				Path:       remotePath,
				Name:       file.Name,
				Kind:       remoteKind(file),
				Size:       file.Size,
				ModifiedAt: remoteModifiedAt(file),
			},
		}
		if file.IsDir() && includesBranch(selected, "putio", remotePath) {
			node.children, err = s.remoteChildren(ctx, file.ID, remotePath, visited, selected)
			if err != nil {
				return nil, err
			}
			if node.object.Size == 0 {
				node.object.Size = totalRemoteBytes(node.children)
			}
		}
		nodes = append(nodes, node)
	}
	return nodes, nil
}

func remoteKind(file *putio.File) string {
	if file.IsDir() {
		return "directory"
	}
	return "file"
}

func remoteModifiedAt(file *putio.File) string {
	if file.UpdatedAt != nil {
		return file.UpdatedAt.UTC().Format(time.RFC3339Nano)
	}
	if file.CreatedAt != nil {
		return file.CreatedAt.UTC().Format(time.RFC3339Nano)
	}
	return ""
}

func totalRemoteBytes(nodes []*remoteNode) int64 {
	var total int64
	for _, node := range nodes {
		total += node.object.Size
	}
	return total
}

func classifyRemote(nodes []*remoteNode, activeIDs, activeParents map[int64]struct{}) (active, unmanaged []Object) {
	for _, node := range nodes {
		if _, ok := activeIDs[node.fileID]; ok {
			active = append(active, node.object)
			continue
		}
		_, isActiveParent := activeParents[node.fileID]
		if isActiveParent || remoteTreeContainsActive(node.children, activeIDs, activeParents) {
			childActive, childUnmanaged := classifyRemote(node.children, activeIDs, activeParents)
			active = append(active, childActive...)
			unmanaged = append(unmanaged, childUnmanaged...)
			continue
		}
		unmanaged = append(unmanaged, node.object)
	}
	return active, unmanaged
}

func remoteTreeContainsActive(nodes []*remoteNode, activeIDs, activeParents map[int64]struct{}) bool {
	for _, node := range nodes {
		_, active := activeIDs[node.fileID]
		_, activeParent := activeParents[node.fileID]
		if active || activeParent || remoteTreeContainsActive(node.children, activeIDs, activeParents) {
			return true
		}
	}
	return false
}

type localNode struct {
	object   Object
	relPath  string
	info     os.FileInfo
	children []*localNode
}

func buildLocalTree(root string, selected *Object) ([]*localNode, error) {
	if selected != nil && selected.Source != "local" {
		return nil, nil
	}
	info, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("stat local root: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("local root %q is not a directory", root)
	}
	return localChildren(root, "", selected)
}

func localChildren(root, parentRel string, selected *Object) ([]*localNode, error) {
	directory := filepath.Join(root, parentRel)
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, fmt.Errorf("read local directory %q: %w", directory, err)
	}

	nodes := make([]*localNode, 0, len(entries))
	for _, entry := range entries {
		rel := filepath.Join(parentRel, entry.Name())
		if !includesBranch(selected, "local", filepath.ToSlash(rel)) {
			continue
		}
		if parentRel == "" && reservedLocalName(entry.Name()) {
			continue
		}
		info, err := os.Lstat(filepath.Join(root, rel))
		if err != nil {
			return nil, fmt.Errorf("stat local object %q: %w", rel, err)
		}
		node := &localNode{
			relPath: rel,
			info:    info,
			object: Object{
				Source:     "local",
				Path:       filepath.ToSlash(rel),
				Name:       entry.Name(),
				Kind:       localKind(info),
				Size:       info.Size(),
				ModifiedAt: info.ModTime().UTC().Format(time.RFC3339Nano),
			},
		}
		if info.IsDir() {
			node.children, err = localChildren(root, rel, selected)
			if err != nil {
				return nil, err
			}
			node.object.Size = totalLocalBytes(node.children)
		}
		node.object.ID = localID(rel, info, node.children)
		nodes = append(nodes, node)
	}
	return nodes, nil
}

func localKind(info os.FileInfo) string {
	if info.Mode()&os.ModeSymlink != 0 {
		return "symlink"
	}
	if info.IsDir() {
		return "directory"
	}
	return "file"
}

func localID(rel string, info os.FileInfo, children []*localNode) string {
	parts := []string{
		"local",
		filepath.ToSlash(rel),
		strconv.FormatInt(info.Size(), 10),
		strconv.FormatInt(info.ModTime().UnixNano(), 10),
		strconv.FormatUint(uint64(info.Mode()), 10),
		systemIdentity(info),
	}
	for _, child := range children {
		parts = append(parts, child.object.ID)
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return "local:" + hex.EncodeToString(sum[:])
}

func totalLocalBytes(nodes []*localNode) int64 {
	var total int64
	for _, node := range nodes {
		total += node.object.Size
	}
	return total
}

func activeLocalPaths(root string, transfers []*putio.Transfer, nodes []*localNode) (map[string]struct{}, error) {
	categories, err := download.LoadCategories(root)
	if err != nil {
		return nil, err
	}

	active := make(map[string]struct{}, len(transfers)*2)
	pending, err := download.PendingRemovalPaths(root)
	if err != nil {
		return nil, err
	}
	for _, rel := range pending {
		active[rel] = struct{}{}
	}
	for _, transfer := range transfers {
		category := categories[transfer.ID]
		paths := []string{transfer.Name}
		if category != "" {
			paths = append(paths, filepath.Join(category, transfer.Name))
		}
		for _, rel := range paths {
			clean, err := confinedRelativePath(root, rel)
			if err != nil {
				return nil, fmt.Errorf("unsafe local path for transfer %d: %w", transfer.ID, err)
			}
			active[clean] = struct{}{}
			if info, err := os.Stat(filepath.Join(root, clean)); err == nil {
				collectSameLocalFiles(nodes, info, active)
			} else if !os.IsNotExist(err) {
				return nil, fmt.Errorf("resolve active local path for transfer %d: %w", transfer.ID, err)
			}
		}
	}
	return active, nil
}

func collectSameLocalFiles(nodes []*localNode, target os.FileInfo, active map[string]struct{}) {
	for _, node := range nodes {
		if os.SameFile(node.info, target) {
			active[node.relPath] = struct{}{}
		}
		collectSameLocalFiles(node.children, target, active)
	}
}

func reservedLocalName(name string) bool {
	return strings.EqualFold(name, download.CategoryStateFileName) || download.IsReservedTransferName(name)
}

func includesBranch(selected *Object, source, objectPath string) bool {
	return selected == nil || selected.Source == source && (selected.Path == objectPath ||
		strings.HasPrefix(selected.Path, objectPath+"/") || strings.HasPrefix(objectPath, selected.Path+"/"))
}

func confinedRelativePath(root, rel string) (string, error) {
	if rel == "" || filepath.IsAbs(rel) {
		return "", fmt.Errorf("path %q is not a relative object path", rel)
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	absPath, err := filepath.Abs(filepath.Join(absRoot, rel))
	if err != nil {
		return "", err
	}
	confined, err := filepath.Rel(absRoot, absPath)
	if err != nil || confined == "." || confined == ".." || strings.HasPrefix(confined, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("path %q is outside root %q", rel, absRoot)
	}
	return confined, nil
}

func classifyLocal(nodes []*localNode, activePaths map[string]struct{}) (active, unmanaged []Object) {
	for _, node := range nodes {
		if _, ok := activePaths[node.relPath]; ok {
			active = append(active, node.object)
			continue
		}
		if localTreeContainsActive(node.relPath, activePaths) {
			childActive, childUnmanaged := classifyLocal(node.children, activePaths)
			active = append(active, childActive...)
			unmanaged = append(unmanaged, childUnmanaged...)
			continue
		}
		unmanaged = append(unmanaged, node.object)
	}
	return active, unmanaged
}

func localTreeContainsActive(rel string, activePaths map[string]struct{}) bool {
	for active := range activePaths {
		inside, err := filepath.Rel(rel, active)
		if err == nil && inside != "." && inside != ".." && !strings.HasPrefix(inside, ".."+string(os.PathSeparator)) {
			return true
		}
	}
	return false
}

func transfersInRoot(transfers []*putio.Transfer, folderID int64, nodes []*remoteNode, useCategoriesPutio bool) []*putio.Transfer {
	managedFolders := map[int64]struct{}{folderID: {}}
	if useCategoriesPutio {
		for _, node := range nodes {
			if node.object.Kind == "directory" {
				managedFolders[node.fileID] = struct{}{}
			}
		}
	}
	result := make([]*putio.Transfer, 0, len(transfers))
	for _, transfer := range transfers {
		_, parentInRoot := managedFolders[transfer.SaveParentID]
		if parentInRoot {
			result = append(result, transfer)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}

func sortObjects(objects []Object) {
	sort.Slice(objects, func(i, j int) bool {
		if objects[i].Source == objects[j].Source {
			if objects[i].Path == objects[j].Path {
				return objects[i].ID < objects[j].ID
			}
			return objects[i].Path < objects[j].Path
		}
		return objects[i].Source < objects[j].Source
	})
}
