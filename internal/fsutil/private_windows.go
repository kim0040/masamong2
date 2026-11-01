//go:build windows

package fsutil

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	privateDACLAccess = windows.DACL_SECURITY_INFORMATION | windows.PROTECTED_DACL_SECURITY_INFORMATION
	migrationMarker   = ".masamong-private-tree-v1"
	markerBody        = "masamong-private-tree-v1\n"
	aclRetryCount     = 3

	fileAllAccess   = 0x1F01FF
	inheritOnlyACE  = 0x08
	inheritBothACEs = windows.OBJECT_INHERIT_ACE | windows.CONTAINER_INHERIT_ACE
	// Rights that let another account change or remove data: write data, append, delete child, write
	// attributes, DELETE, WRITE_DAC, WRITE_OWNER and the generic write/all bits.
	writeRights = 0x2 | 0x4 | 0x40 | 0x100 | 0x10000 | 0x40000 | 0x80000 | 0x10000000 | 0x40000000
)

var (
	aclVolumes sync.Map // volume root -> bool: keeps persistent ACLs

	// applyACL is replaced in tests to simulate objects the account cannot protect.
	applyACL = setPrivateACL
)

// principals are the only accounts that may touch private data: the current user, SYSTEM and
// Administrators. Accounts such as SYSTEM or a service account are their own "current user", so the
// list holds no duplicates (a duplicate ACE is harmless but untidy).
type principals struct {
	user *windows.SID
	list []*windows.SID
}

func currentPrincipals() (principals, error) {
	token, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return principals{}, fmt.Errorf("get current user SID: %w", err)
	}
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		return principals{}, err
	}
	admins, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return principals{}, err
	}
	p := principals{user: token.User.Sid}
	for _, sid := range []*windows.SID{token.User.Sid, system, admins} {
		if !p.has(sid) {
			p.list = append(p.list, sid)
		}
	}
	return p, nil
}

func (p principals) has(sid *windows.SID) bool {
	for _, known := range p.list {
		if known.Equals(sid) {
			return true
		}
	}
	return false
}

// sddl is the private descriptor: owner-only, with the DACL protected from the parent. Directories
// pass their ACEs on to new children, so a protected root keeps everything created later private.
func (p principals) sddl(dir bool) string {
	flags := ""
	if dir {
		flags = "OICI"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "O:%sD:P", p.user.String())
	for _, sid := range p.list {
		fmt.Fprintf(&b, "(A;%s;FA;;;%s)", flags, sid.String())
	}
	return b.String()
}

type ace struct {
	allow bool
	flags uint8
	mask  windows.ACCESS_MASK
	sid   *windows.SID
}

// daclPolicy is what a DACL grants. ACEs are read as SIDs, never compared as SDDL text, because
// Windows renders well-known accounts with aliases (SY, BA, NS) and the text differs by account.
type daclPolicy struct {
	protected bool
	hasDACL   bool
	owner     *windows.SID
	aces      []ace
}

func readPolicy(path string) (daclPolicy, error) {
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return daclPolicy{}, err
	}
	if sd == nil {
		return daclPolicy{}, fmt.Errorf("%q has no security descriptor", path)
	}
	control, _, err := sd.Control()
	if err != nil {
		return daclPolicy{}, err
	}
	out := daclPolicy{protected: control&windows.SE_DACL_PROTECTED != 0}
	out.owner, _, _ = sd.Owner()
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil {
		return out, nil // a NULL DACL grants everyone everything; the zero policy is never private
	}
	out.hasDACL = true
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var raw *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &raw); err != nil {
			return daclPolicy{}, err
		}
		// the ACE memory belongs to the descriptor; keep a copy of the SID
		sid, err := (*windows.SID)(unsafe.Pointer(&raw.SidStart)).Copy()
		if err != nil {
			return daclPolicy{}, err
		}
		out.aces = append(out.aces, ace{
			allow: raw.Header.AceType == windows.ACCESS_ALLOWED_ACE_TYPE,
			flags: raw.Header.AceFlags,
			mask:  raw.Mask,
			sid:   sid,
		})
	}
	return out, nil
}

// private reports whether only the current user, SYSTEM and Administrators can reach the object. dir
// asks for the stricter shape PrivateDir writes: a protected DACL whose ACEs reach new children.
func (p principals) private(pol daclPolicy, dir bool) error {
	if !pol.hasDACL {
		return errors.New("no DACL, which grants everyone access")
	}
	if pol.owner == nil || !p.has(pol.owner) {
		return errors.New("owner is not the current user, SYSTEM or Administrators")
	}
	userFull := false
	for _, a := range pol.aces {
		if a.flags&inheritOnlyACE != 0 || !a.allow {
			continue // inherit-only ACEs do not apply here; deny ACEs only restrict
		}
		if !p.has(a.sid) {
			return fmt.Errorf("grants access to %s", a.sid.String())
		}
		if p.user.Equals(a.sid) && a.mask&fileAllAccess == fileAllAccess && (!dir || a.flags&inheritBothACEs == inheritBothACEs) {
			userFull = true
		}
	}
	if !userFull {
		return errors.New("the current user has no full-control entry")
	}
	if dir && !pol.protected {
		return errors.New("DACL still inherits from its parent")
	}
	return nil
}

// otherWriters finds an account outside the private set that may change data in the folder. Read
// access alone is not "shared" here: such a folder gets a warning, not a refused start.
func (p principals) otherWriters(pol daclPolicy) (string, bool) {
	for _, a := range pol.aces {
		if a.allow && a.flags&inheritOnlyACE == 0 && a.mask&writeRights != 0 && !p.has(a.sid) {
			return a.sid.String(), true
		}
	}
	return "", false
}

func (p principals) check(path string, dir bool) error {
	pol, err := readPolicy(path)
	if err != nil {
		return err
	}
	return p.private(pol, dir)
}

// PrivateDir keeps a data tree away from other accounts. The root gets a protected DACL that its
// children inherit. A private root with a completion marker costs only a few descriptor and file reads;
// an older or interrupted migration is walked again until every child is protected.
// Reparse points are skipped (never followed), files that vanish meanwhile are ignored, and a child
// that cannot be protected is a warning (listed by Warnings and doctor), not an error.
//
// The error is reserved for a root that cannot be protected AND is shared, that is, another account
// has write access to it. A root that cannot be protected but is not shared (a drive without ACLs, a
// folder owned by someone else with read-only exposure) starts with a warning.
func PrivateDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	// A root reached through a link or junction (a data folder moved to another drive with mklink /J)
	// is the operator's choice: protect the folder it leads to. Links inside the tree stay skipped.
	for hops := 0; info.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0; hops++ {
		// filepath.EvalSymlinks leaves junctions alone, so follow each link with Readlink
		target, err := os.Readlink(dir)
		if err != nil || hops == 8 {
			return fmt.Errorf("protect %q: resolve link: %v", dir, err)
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(dir), target)
		}
		dir = filepath.Clean(target)
		if info, err = os.Lstat(dir); err != nil {
			return err
		}
	}
	if !info.IsDir() {
		return fmt.Errorf("private directory %q is not a real directory", dir)
	}
	root, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("protect %q: resolve path: %w", dir, err)
	}
	if !volumeKeepsACLs(root) {
		Warn("volume:"+volumeKey(root), fmt.Sprintf("the drive of %s does not keep Windows permissions (FAT or exFAT?), so the data folder cannot be made private to your account; use an NTFS folder for the data root", root))
		return nil
	}
	pr, err := currentPrincipals()
	if err != nil {
		return fmt.Errorf("protect %q: %w", dir, err)
	}
	rootPrivate := pr.check(root, true) == nil
	if rootPrivate && privateMigrationComplete(pr, root) {
		return nil
	}
	result := protectTree(pr, root)
	var setErr error
	if !rootPrivate {
		setErr = applyACL(root, true)
		if setErr == nil {
			setErr = pr.check(root, true)
		}
	}
	if setErr != nil {
		pol, readErr := readPolicy(root)
		if readErr == nil && pr.private(pol, true) == nil {
			setErr = nil // the DACL was set; only its propagation to some child failed
		}
	}
	if setErr != nil {
		pol, readErr := readPolicy(root)
		if readErr == nil {
			if sid, shared := pr.otherWriters(pol); shared {
				return fmt.Errorf("data folder %q is shared (%s can change it) and cannot be made private: %w", dir, sid, setErr)
			}
		}
		Warn("root:"+root, fmt.Sprintf("the data folder %s could not be made private to your account (%v); other accounts may be able to read it", root, setErr))
	}
	if result.failed > 0 {
		Warn("children:"+root, fmt.Sprintf("%d item(s) in the data folder %s could not be made private (first: %s)", result.failed, root, result.first))
		return nil
	}
	if setErr == nil {
		if err := writeMigrationMarker(root); err == nil {
			return nil
		} else {
			Warn("children:"+root, fmt.Sprintf("the data tree in %s is private, but migration completion could not be recorded (%v); it will be checked again on the next start", root, err))
			return nil
		}
	}
	return nil
}

type treeProtectionResult struct {
	failed int
	first  string
}

func privateMigrationComplete(pr principals, root string) bool {
	// Store the marker in an NTFS alternate data stream on the directory itself. A normal
	// child file would appear in application directories such as imports and look user-owned.
	if err := pr.check(root, true); err != nil {
		return false
	}
	marker := migrationMarkerPath(root)
	info, err := os.Lstat(marker)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	body, err := os.ReadFile(marker)
	return err == nil && string(body) == markerBody
}

func writeMigrationMarker(root string) error {
	f, err := os.OpenFile(migrationMarkerPath(root), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write([]byte(markerBody)); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func migrationMarkerPath(root string) string {
	return root + ":" + migrationMarker
}

// protectTree gives every child of root that is not private a private DACL. It never fails: whatever
// cannot be protected is returned so the caller can leave migration incomplete and retry next start.
func protectTree(pr principals, root string) treeProtectionResult {
	var result treeProtectionResult
	note := func(path string, err error) {
		if result.failed == 0 {
			result.first = fmt.Sprintf("%s: %v", path, err)
		}
		result.failed++
	}
	_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if path == root && walkErr == nil {
			return nil
		}
		if walkErr != nil {
			if !errors.Is(walkErr, fs.ErrNotExist) {
				note(path, walkErr)
			}
			if entry != nil && entry.IsDir() && path != root {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Type()&(os.ModeSymlink|os.ModeIrregular) != 0 {
			Warn("link:"+path, fmt.Sprintf("skipping ACL update for %s: it is a link or mount point, so its target keeps its own permissions", path))
			return nil
		}
		dir := entry.IsDir()
		if !dir && !entry.Type().IsRegular() {
			return nil
		}
		if pr.check(path, false) == nil {
			return nil
		}
		if err := protectChild(pr, path, dir); err != nil {
			note(path, err)
		}
		return nil
	})
	return result
}

func protectChild(pr principals, path string, dir bool) error {
	var err error
	for attempt := 0; attempt <= aclRetryCount; attempt++ {
		err = applyACL(path, dir)
		if err == nil || errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if !pathExists(path) {
			return nil
		}
		if pr.check(path, false) == nil {
			return nil
		}
		if !errors.Is(err, windows.ERROR_ACCESS_DENIED) || attempt == aclRetryCount {
			break
		}
		time.Sleep(time.Duration(attempt+1) * 5 * time.Millisecond)
	}
	if !pathExists(path) || pr.check(path, false) == nil {
		return nil
	}
	return err
}

func pathExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil || !errors.Is(err, fs.ErrNotExist)
}

// PrivateFile protects an existing file from inherited and explicit broad access. A missing file is
// not an error so callers can secure optional state before it is first written. A file that already
// grants access to no one else (typically one that inherited the private folder's ACL) is left alone.
func PrivateFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("private file %q is not a regular file", path)
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("protect %q: resolve path: %w", path, err)
	}
	if !volumeKeepsACLs(absolute) {
		Warn("volume:"+volumeKey(absolute), fmt.Sprintf("the drive of %s does not keep Windows permissions (FAT or exFAT?), so files there cannot be made private to your account", absolute))
		return nil
	}
	pr, err := currentPrincipals()
	if err != nil {
		return fmt.Errorf("protect %q: %w", path, err)
	}
	if pr.check(absolute, false) == nil {
		return nil
	}
	if err := applyACL(absolute, false); err != nil {
		return err
	}
	if err := pr.check(absolute, false); err != nil {
		return fmt.Errorf("protect %q: verify Windows ACL: %w", path, err)
	}
	return nil
}

// VerifyPrivateDir and VerifyPrivateFile check that only the current user, SYSTEM and Administrators
// have access. A directory must also block inheritance and pass its ACEs to new children.
func VerifyPrivateDir(path string) error  { return verifyPrivate(path, true) }
func VerifyPrivateFile(path string) error { return verifyPrivate(path, false) }

func verifyPrivate(path string, dir bool) error {
	pr, err := currentPrincipals()
	if err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || dir && !info.IsDir() || !dir && !info.Mode().IsRegular() {
		return fmt.Errorf("%q has unexpected file type", path)
	}
	if err := pr.check(path, dir); err != nil {
		return fmt.Errorf("%q does not match private policy: %w", path, err)
	}
	return nil
}

// setPrivateACL writes the private descriptor. The owner is set to the current user when Windows
// allows it; a refusal (no take-ownership right) keeps the owner and still replaces the DACL.
func setPrivateACL(path string, dir bool) error {
	pr, err := currentPrincipals()
	if err != nil {
		return fmt.Errorf("protect %q: %w", path, err)
	}
	sd, err := windows.SecurityDescriptorFromString(pr.sddl(dir))
	if err != nil {
		return fmt.Errorf("protect %q: build private DACL: %w", path, err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return fmt.Errorf("protect %q: read private DACL: %w", path, err)
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("protect %q: resolve path: %w", path, err)
	}
	err = windows.SetNamedSecurityInfo(absolute, windows.SE_FILE_OBJECT,
		privateDACLAccess|windows.OWNER_SECURITY_INFORMATION, pr.user, nil, dacl, nil)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		err = windows.SetNamedSecurityInfo(absolute, windows.SE_FILE_OBJECT, privateDACLAccess, nil, nil, dacl, nil)
	}
	if err != nil {
		return fmt.Errorf("protect %q: set owner-only Windows ACL: %w", path, err)
	}
	return nil
}

func volumeKey(path string) string {
	buf := make([]uint16, windows.MAX_PATH+1)
	name, err := windows.UTF16PtrFromString(path)
	if err != nil || windows.GetVolumePathName(name, &buf[0], uint32(len(buf))) != nil {
		return strings.ToUpper(filepath.VolumeName(path))
	}
	return strings.ToUpper(windows.UTF16ToString(buf))
}

// volumeKeepsACLs is false for FAT/exFAT and other volumes without persistent ACLs; asking them to
// set one fails (or is silently dropped), which is not the user's fault and not worth stopping for.
func volumeKeepsACLs(path string) bool {
	key := volumeKey(path)
	if cached, ok := aclVolumes.Load(key); ok {
		return cached.(bool)
	}
	keeps := true
	if root, err := windows.UTF16PtrFromString(key); err == nil && key != "" {
		var flags uint32
		if err := windows.GetVolumeInformation(root, nil, 0, nil, nil, &flags, nil, 0); err == nil {
			keeps = flags&windows.FILE_PERSISTENT_ACLS != 0
		}
	}
	aclVolumes.Store(key, keeps)
	return keeps
}
