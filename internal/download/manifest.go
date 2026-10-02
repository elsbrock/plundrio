package download

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/elsbrock/go-putio"
)

// LocalManifest records immutable, ID-keyed ownership. Legacy arrays use their
// single top-level directory; new manifests record the processing-time root
// explicitly. Neither is authority to adopt other files found in that
// directory, and neither records a remote display name: the latest poll is the
// only source for that.
type LocalManifest struct {
	TransferID int64          `json:"transferId"`
	LocalRoot  string         `json:"localRoot"`
	Files      []TransferFile `json:"files"`
}

func (manifest LocalManifest) resolve() (LocalManifest, error) {
	if err := manifest.validateFiles(); err != nil {
		return manifest, err
	}
	if manifest.LocalRoot == "" && len(manifest.Files) > 0 {
		root, err := legacyLocalRoot(manifest.Files)
		if err != nil {
			return manifest, err
		}
		manifest.LocalRoot = root
	}
	return manifest, manifest.validateRoot()
}

// validate accepts a stored record whose legacy root cannot be resolved: its
// widest claim still guards other transfers even while its own boundary is
// unknown, and only the transfer itself is refused.
func (manifest LocalManifest) validate() (LocalManifest, error) {
	if err := manifest.validateFiles(); err != nil {
		return manifest, err
	}
	return manifest, manifest.validateRoot()
}

func (manifest LocalManifest) validateFiles() error {
	if manifest.TransferID <= 0 {
		return fmt.Errorf("invalid manifest transfer ID %d", manifest.TransferID)
	}
	seen := make(map[string]bool, len(manifest.Files))
	shared := ""
	for i, file := range manifest.Files {
		name := filepath.FromSlash(file.Name)
		if !safeManifestPath(name) || file.Length < 0 {
			return fmt.Errorf("unsafe manifest entry %q (length %d)", file.Name, file.Length)
		}
		root, _, hasFile := strings.Cut(name, string(filepath.Separator))
		if !hasFile || IsReservedTransferName(root) {
			return fmt.Errorf("unsafe manifest root in %q", file.Name)
		}
		// One manifest owns one local root. A record listing several is
		// structurally invalid, not ambiguous: its widest claim cannot be
		// expressed, so it must fail closed rather than reserve one root and
		// leave the rest open for another transfer to claim and delete.
		if i == 0 {
			shared = root
		} else if root != shared {
			return fmt.Errorf("manifest files do not share one local root (%q and %q)", shared, root)
		}
		if seen[name] {
			return fmt.Errorf("duplicate manifest file %q", file.Name)
		}
		seen[name] = true
	}
	return nil
}

func (manifest LocalManifest) validateRoot() error {
	if manifest.LocalRoot == "" {
		return nil
	}
	if !safeManifestPath(manifest.LocalRoot) || IsReservedTransferName(strings.Split(manifest.LocalRoot, string(filepath.Separator))[0]) {
		return fmt.Errorf("unsafe manifest root %q", manifest.LocalRoot)
	}
	for _, file := range manifest.Files {
		name := filepath.FromSlash(file.Name)
		rel, err := filepath.Rel(manifest.LocalRoot, name)
		if err != nil || !safeManifestPath(rel) {
			return fmt.Errorf("manifest file %q is outside local root %q", file.Name, manifest.LocalRoot)
		}
	}
	return nil
}

func safeManifestPath(path string) bool {
	return path != "." && filepath.IsLocal(path) && filepath.Clean(path) == path && !strings.ContainsRune(path, 0)
}

// A legacy array records only file paths, never how many of their components
// formed the transfer root. An entry sitting directly inside the root proves
// where it ends; below that the boundary is unknowable, and the mutable remote
// display name is not evidence, so ownership is refused instead of inferred.
func legacyLocalRoot(files []TransferFile) (string, error) {
	root, _, _ := strings.Cut(filepath.FromSlash(files[0].Name), string(filepath.Separator))
	for _, file := range files {
		if strings.Count(filepath.FromSlash(file.Name), string(filepath.Separator)) == 1 {
			return root, nil
		}
	}
	return "", fmt.Errorf("ambiguous legacy manifest root below %q", root)
}

// claimedRoot is the widest directory a manifest could own: its recorded root,
// or the shared first component of a legacy array whose root was never written
// down. Collision detection uses it so a narrower reading of one record can
// never hide an overlap with another.
func (manifest LocalManifest) claimedRoot() string {
	if manifest.LocalRoot != "" || len(manifest.Files) == 0 {
		return manifest.LocalRoot
	}
	root, _, _ := strings.Cut(filepath.FromSlash(manifest.Files[0].Name), string(filepath.Separator))
	return root
}

// ManifestCheck selects how much local evidence a manifest read requires.
// Ownership, confinement and symlink rejection are enforced in every mode.
type ManifestCheck int

const (
	// ManifestCheckPending tolerates a transfer whose local copy is still
	// being written, including a root that does not exist yet.
	ManifestCheckPending ManifestCheck = iota
	// ManifestCheckProcessed reports historical metadata for a transfer this
	// instance already downloaded. The payload may have been imported, renamed
	// or resized, and its root may be gone altogether, whatever the remote name.
	ManifestCheckProcessed
	// ManifestCheckComplete requires every manifest file at its exact length.
	ManifestCheckComplete
)

// GetTransferManifest is read-only: an absent manifest never authorizes a scan
// or adoption, and a corrupt manifest is an error rather than absence.
func (m *Manager) GetTransferManifest(transfer *putio.Transfer, check ManifestCheck) (LocalManifest, error) {
	return m.TransferFileReader().GetTransferManifest(transfer, check)
}

// restorationManifest reports the transfer's own persisted ownership when the
// remote source is already gone. Absence of a record for this ID is reported as
// an empty manifest, since nothing local is claimed and no deletion candidate
// is involved; an unreadable state directory or a malformed record for this ID
// is still an error. Records that exist are validated in full.
func (m *Manager) restorationManifest(transfer *putio.Transfer) (LocalManifest, error) {
	m.transferFiles.mu.RLock()
	snapshot := m.readManifests()
	m.transferFiles.mu.RUnlock()
	if snapshot.scanErr != nil {
		return LocalManifest{}, snapshot.scanErr
	}
	if err := snapshot.errors[transfer.ID]; err != nil {
		return LocalManifest{}, err
	}
	if _, exists := snapshot.manifests[transfer.ID]; !exists {
		return LocalManifest{}, nil
	}
	return snapshot.GetTransferManifest(transfer, ManifestCheckComplete)
}

// TransferFileReader keeps one ownership snapshot for a files-inclusive RPC,
// avoiding a full disk scan for every torrent in the response.
type TransferFileReader interface {
	GetTransferManifest(*putio.Transfer, ManifestCheck) (LocalManifest, error)
}

type manifestSnapshot struct {
	targetDir string
	manifests map[int64]LocalManifest
	category  func(int64) (string, error)
	claims    map[int64]manifestClaim
	errors    map[int64]error
	scanErr   error
}

// manifestClaim is every category-qualified root one stored record could own,
// resolved once per snapshot. err is set when that area cannot be bounded.
type manifestClaim struct {
	roots []string
	err   error
}

func (m *Manager) TransferFileReader() TransferFileReader {
	m.transferFiles.mu.RLock()
	defer m.transferFiles.mu.RUnlock()
	return m.readManifests()
}

// Caller holds transferFiles.mu, including across publication of a new claim.
func (m *Manager) readManifests() *manifestSnapshot {
	snapshot := &manifestSnapshot{targetDir: m.cfg.TargetDir, manifests: make(map[int64]LocalManifest), category: m.localCategory, claims: make(map[int64]manifestClaim), errors: make(map[int64]error)}
	root, err := os.OpenRoot(m.cfg.TargetDir)
	if os.IsNotExist(err) {
		return snapshot
	}
	if err != nil {
		snapshot.scanErr = err
		return snapshot
	}
	defer func() { _ = root.Close() }()
	info, err := root.Lstat(transferFilesStateDirName)
	if os.IsNotExist(err) {
		return snapshot
	}
	if err != nil {
		snapshot.scanErr = err
		return snapshot
	}
	if info.Mode()&os.ModeSymlink != 0 {
		snapshot.scanErr = fmt.Errorf("symlink in transfer file state directory")
		return snapshot
	}
	dir, err := root.Open(transferFilesStateDirName)
	if err != nil {
		snapshot.scanErr = err
		return snapshot
	}
	defer func() { _ = dir.Close() }()
	entries, err := dir.ReadDir(-1)
	if err != nil {
		snapshot.scanErr = fmt.Errorf("read manifest ownership: %w", err)
	}
	for _, entry := range entries {
		id, err := strconv.ParseInt(strings.TrimSuffix(entry.Name(), ".json"), 10, 64)
		if err != nil || id <= 0 || entry.Name() != strconv.FormatInt(id, 10)+".json" {
			continue
		}
		manifest, err := m.transferFiles.loadManifest(id)
		if err != nil {
			snapshot.errors[id] = err
			if !m.confirmedAbsent(id) {
				snapshot.claims[id] = manifestClaim{err: fmt.Errorf("manifest %d: %w", id, err)}
			}
			continue
		}
		if _, err := manifest.validate(); err != nil {
			snapshot.errors[id] = err
			roots := manifest.possibleRoots()
			if roots != nil || !m.confirmedAbsent(id) {
				snapshot.claims[id] = snapshot.resolveClaim(id, roots, fmt.Errorf("manifest %d: %w", id, err))
			}
			continue
		}
		snapshot.manifests[id] = manifest
		if len(manifest.Files) > 0 {
			snapshot.claims[id] = snapshot.resolveClaim(id, []string{manifest.claimedRoot()}, nil)
		}
	}
	return snapshot
}

// resolveClaim qualifies roots with the record's category. No roots means the
// record's area is unknown, so unknownErr then blocks every transfer unless a
// successful account-wide listing confirmed the record's transfer is gone.
func (s *manifestSnapshot) resolveClaim(id int64, roots []string, unknownErr error) manifestClaim {
	if len(roots) == 0 {
		return manifestClaim{err: unknownErr}
	}
	category, err := s.category(id)
	if err != nil {
		return manifestClaim{err: fmt.Errorf("category for manifest %d: %w", id, err)}
	}
	if category != "" && !safeManifestPath(category) {
		return manifestClaim{err: fmt.Errorf("unsafe category for manifest %d", id)}
	}
	claim := manifestClaim{roots: make([]string, len(roots))}
	for i, root := range roots {
		claim.roots[i] = filepath.Join(category, root)
	}
	return claim
}

// possibleRoots lists the first component of every root and entry a decoded
// but invalid record names, or nil when any full path is not a safe local path
// and so cannot bound what the record might claim.
func (manifest LocalManifest) possibleRoots() []string {
	names := make([]string, 0, len(manifest.Files)+1)
	if manifest.LocalRoot != "" {
		names = append(names, manifest.LocalRoot)
	}
	for _, file := range manifest.Files {
		names = append(names, file.Name)
	}
	roots := make([]string, 0, len(names))
	for _, name := range names {
		name = filepath.FromSlash(name)
		if !safeManifestPath(name) {
			return nil
		}
		root, _, _ := strings.Cut(name, string(filepath.Separator))
		roots = append(roots, root)
	}
	return roots
}

func (s *manifestSnapshot) GetTransferManifest(transfer *putio.Transfer, check ManifestCheck) (LocalManifest, error) {
	if err := s.errors[transfer.ID]; err != nil {
		return LocalManifest{}, err
	}
	manifest, exists := s.manifests[transfer.ID]
	if !exists {
		manifest.TransferID = transfer.ID
	}
	return s.validateManifest(transfer, manifest, check)
}

// deletionCandidate is the directory a manifest-less removal would delete: the
// transfer's remote name resolved under the download root, exactly as
// deleteLocalData resolves it. A name that cannot land there is refused rather
// than passed through, so ownership never reads clean for a name nothing can
// establish a boundary for.
func (s *manifestSnapshot) deletionCandidate(name string) (string, error) {
	rel, err := filepath.Rel(s.targetDir, filepath.Join(s.targetDir, filepath.FromSlash(name)))
	if err != nil || !safeManifestPath(rel) || IsReservedTransferName(strings.Split(rel, string(filepath.Separator))[0]) {
		return "", fmt.Errorf("unsafe transfer name %q", name)
	}
	return rel, nil
}

func (s *manifestSnapshot) validateManifest(transfer *putio.Transfer, manifest LocalManifest, check ManifestCheck) (LocalManifest, error) {
	if s.scanErr != nil {
		return manifest, s.scanErr
	}
	claim := manifest.claimedRoot()
	manifest, err := manifest.resolve()
	if err != nil {
		return manifest, err
	}
	category, err := s.category(transfer.ID)
	if err != nil {
		return manifest, fmt.Errorf("cannot establish ownership: category for transfer %d: %w", transfer.ID, err)
	}
	if category != "" && !safeManifestPath(category) {
		return manifest, fmt.Errorf("unsafe manifest category %q", category)
	}
	owned := len(manifest.Files) > 0
	localRoot := filepath.Join(category, manifest.LocalRoot)
	// A transfer without a manifest owns nothing, yet removal still falls back
	// to deleting the directory its remote name points at. That candidate is
	// not ownership evidence, so it must clear every other record's claim and
	// the same confinement and symlink checks before it can be used.
	if !owned {
		candidate, err := s.deletionCandidate(transfer.Name)
		if err != nil {
			return manifest, err
		}
		claim, localRoot = candidate, filepath.Join(category, candidate)
	}
	if err := s.checkManifestCollision(transfer.ID, filepath.Join(category, claim)); err != nil {
		return manifest, err
	}
	root, err := os.OpenRoot(s.targetDir)
	if os.IsNotExist(err) && (!owned || (check == ManifestCheckPending && manifest.LocalRoot == transfer.Name)) {
		return manifest, nil // The ordinary first download creates the target.
	}
	if err != nil {
		return manifest, fmt.Errorf("open download root: %w", err)
	}
	defer func() { _ = root.Close() }()
	// An unchanged-name transfer may not have created its root yet, and an
	// importer may have moved the finished payload out and removed it again.
	// A pending download after name drift requires the root; never create a
	// replacement for it. A processed root that is gone has nothing to delete.
	rootRequired := owned && (check == ManifestCheckComplete || (check == ManifestCheckPending && manifest.LocalRoot != transfer.Name))
	if err := checkManifestPath(root, localRoot, true, rootRequired, 0, check); err != nil {
		return manifest, err
	}
	for _, file := range manifest.Files {
		required := check == ManifestCheckComplete
		if err := checkManifestPath(root, filepath.Join(category, filepath.FromSlash(file.Name)), false, required, file.Length, check); err != nil {
			return manifest, err
		}
	}
	return manifest, nil
}

// Reject symlinks in every component, including category and transfer roots.
// os.Root also confines each lookup if a component changes during validation.
func checkManifestPath(root *os.Root, path string, directory, required bool, length int64, check ManifestCheck) error {
	parts := strings.Split(path, string(filepath.Separator))
	for i := range parts {
		component := filepath.Join(parts[:i+1]...)
		info, err := root.Lstat(component)
		if os.IsNotExist(err) && !required {
			return nil
		}
		if err != nil {
			return fmt.Errorf("stat manifest path %q: %w", component, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlink in manifest path %q", component)
		}
		if i < len(parts)-1 || directory {
			if !info.IsDir() {
				return fmt.Errorf("manifest root %q is not a directory", component)
			}
		} else if !info.Mode().IsRegular() {
			return fmt.Errorf("manifest file %q is not a regular file", path)
		} else if check != ManifestCheckProcessed && (info.Size() > length || (check == ManifestCheckComplete && info.Size() != length)) {
			return fmt.Errorf("manifest file %q does not match expected length %d", path, length)
		}
	}
	return nil
}

func (s *manifestSnapshot) checkManifestCollision(id int64, root string) error {
	for _, otherID := range sortedManifestIDs(s.claims) {
		if otherID == id {
			continue
		}
		claim := s.claims[otherID]
		if claim.err != nil {
			return fmt.Errorf("cannot establish ownership: %w", claim.err)
		}
		for _, otherRoot := range claim.roots {
			if rootsOverlap(root, otherRoot) {
				if s.errors[otherID] != nil {
					return fmt.Errorf("cannot establish ownership: local root %q overlaps unreadable manifest %d root %q: %w", root, otherID, otherRoot, s.errors[otherID])
				}
				return fmt.Errorf("local root %q collides with transfer %d root %q", root, otherID, otherRoot)
			}
		}
	}
	return nil
}

// Conservatively reject case-only aliases on both case-sensitive and
// case-insensitive volumes. Ancestor claims also collide across categories.
func rootsOverlap(root, other string) bool {
	a, b := strings.ToLower(root), strings.ToLower(other)
	return a == b || strings.HasPrefix(a, b+string(filepath.Separator)) || strings.HasPrefix(b, a+string(filepath.Separator))
}

// Ownership diagnostics must name the same competing record on every read,
// so an operator can resolve a blocking manifest instead of chasing whichever
// one map iteration happened to surface.
func sortedManifestIDs[V any](m map[int64]V) []int64 {
	ids := make([]int64, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

// ManifestLocalRoot reports the local root one transfer's persisted manifest
// owns. Reconciliation needs it to keep a transfer whose remote name has
// drifted from being classified as unmanaged local data. Unreadable ownership
// evidence fails closed rather than silently shrinking the protected set.
func ManifestLocalRoot(targetDir string, transfer *putio.Transfer) (string, error) {
	manifest, err := newTransferFileStore(targetDir).loadManifest(transfer.ID)
	if err == nil {
		manifest, err = manifest.resolve()
	}
	if err != nil {
		return "", fmt.Errorf("manifest ownership for transfer %d: %w", transfer.ID, err)
	}
	return manifest.LocalRoot, nil
}

// prepareManifest retains an existing claim across retries/restarts. A changed
// remote file list is not permission to overwrite local ownership evidence.
func (m *Manager) prepareManifest(transfer *putio.Transfer, files []*putio.File) (*putio.Transfer, error) {
	m.transferFiles.mu.Lock()
	defer m.transferFiles.mu.Unlock()
	stored, err := m.transferFiles.loadManifest(transfer.ID)
	if err != nil {
		return nil, err
	}
	local := *transfer
	snapshot := m.readManifests()
	if len(stored.Files) > 0 {
		manifest, err := snapshot.validateManifest(transfer, stored, ManifestCheckPending)
		if err != nil {
			return nil, err
		}
		local.Name = manifest.LocalRoot
	}
	expected, err := buildTransferFileManifest(&local, files)
	if err != nil {
		return nil, err
	}
	if len(stored.Files) > 0 {
		lengths := make(map[string]int64, len(stored.Files))
		for _, file := range stored.Files {
			lengths[file.Name] = file.Length
		}
		if len(stored.Files) != len(expected) {
			return nil, fmt.Errorf("remote file list differs from persisted manifest for transfer %d", transfer.ID)
		}
		for _, file := range expected {
			if length, ok := lengths[file.Name]; !ok || length != file.Length {
				return nil, fmt.Errorf("remote file %q differs from persisted manifest for transfer %d", file.Name, transfer.ID)
			}
		}
	} else {
		manifest := LocalManifest{TransferID: transfer.ID, LocalRoot: filepath.Clean(transfer.Name), Files: expected}
		if _, err := snapshot.validateManifest(&local, manifest, ManifestCheckPending); err != nil {
			return nil, err
		}
		if err := m.transferFiles.setManifest(manifest); err != nil {
			return nil, err
		}
	}
	return &local, nil
}
