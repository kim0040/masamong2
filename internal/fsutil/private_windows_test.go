//go:build windows

package fsutil

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// looseACL gives Everyone access, like a folder created under a broad parent.
func looseACL(t *testing.T, path string) {
	t.Helper()
	if err := setLoose(path); err != nil {
		t.Fatal(err)
	}
}

func setLoose(path string) error {
	sd, err := windows.SecurityDescriptorFromString("D:P(A;OICI;FA;;;WD)")
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil)
}

func TestPrivateDirIsCheapOnceTheRootIsPrivate(t *testing.T) {
	root := filepath.Join(t.TempDir(), "data")
	for i := 0; i < 30; i++ {
		d := filepath.Join(root, fmt.Sprintf("b%02d", i))
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		for j := 0; j < 100; j++ {
			if err := os.WriteFile(filepath.Join(d, fmt.Sprintf("f%03d", j)), nil, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	loose := filepath.Join(root, "b00", "f000")
	looseACL(t, loose)
	if err := PrivateDir(root); err != nil {
		t.Fatal(err)
	}
	if err := VerifyPrivateFile(loose); err != nil {
		t.Fatalf("migration must protect the existing child: %v", err)
	}
	// a child that turns loose afterwards is not found by a normal start: the root is private, so
	// nothing walks the tree (that walk cost 20 s on a 3,000-file root)
	looseACL(t, loose)
	began := time.Now()
	for i := 0; i < 20; i++ {
		if err := PrivateDir(root); err != nil {
			t.Fatal(err)
		}
	}
	if took := time.Since(began); took > 2*time.Second {
		t.Fatalf("20 starts on a private 3,000-file root took %v", took)
	}
	if VerifyPrivateFile(loose) == nil {
		t.Fatal("a private root must not be walked again")
	}
}

func TestPrivateDirRetriesInterruptedMigrationOnEveryStart(t *testing.T) {
	root := filepath.Join(t.TempDir(), "data")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	child := filepath.Join(root, "legacy-memory.txt")
	if err := os.WriteFile(child, []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	looseACL(t, child)

	real := applyACL
	blocked := true
	applyACL = func(path string, dir bool) error {
		if blocked && path == child {
			return windows.ERROR_ACCESS_DENIED
		}
		return real(path, dir)
	}
	t.Cleanup(func() {
		applyACL = real
		ResetWarnings()
	})

	for startup := 1; startup <= 2; startup++ {
		ResetWarnings()
		if err := PrivateDir(root); err != nil {
			t.Fatalf("startup %d: %v", startup, err)
		}
		if len(Warnings()) == 0 {
			t.Fatalf("startup %d did not report the incomplete migration", startup)
		}
		if _, err := os.Stat(migrationMarkerPath(root)); !os.IsNotExist(err) {
			t.Fatalf("startup %d wrote a completion marker after a child failure: %v", startup, err)
		}
	}

	blocked = false
	ResetWarnings()
	if err := PrivateDir(root); err != nil {
		t.Fatal(err)
	}
	if err := VerifyPrivateFile(child); err != nil {
		t.Fatalf("the later startup did not repair the legacy child: %v", err)
	}
	if len(Warnings()) != 0 {
		t.Fatalf("repaired migration left warnings: %v", Warnings())
	}
	if !privateMigrationComplete(mustPrincipals(t), root) {
		t.Fatal("successful migration did not write a private completion marker")
	}
}

func TestPrivateDirStoresMigrationMarkerOutsideDirectoryEntries(t *testing.T) {
	root := filepath.Join(t.TempDir(), "imports")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	child := filepath.Join(root, "existing.csv")
	if err := os.WriteFile(child, []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	looseACL(t, child)

	if err := PrivateDir(root); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "existing.csv" {
		t.Fatalf("migration marker leaked into directory entries: %v", entries)
	}
	if !privateMigrationComplete(mustPrincipals(t), root) {
		t.Fatal("successful migration did not write its directory stream marker")
	}
}

func TestPrivateDirRetriesTransientAccessDenied(t *testing.T) {
	root := filepath.Join(t.TempDir(), "data")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	child := filepath.Join(root, "legacy.txt")
	if err := os.WriteFile(child, []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	looseACL(t, child)

	real := applyACL
	calls := 0
	applyACL = func(path string, dir bool) error {
		if path == child {
			calls++
			if calls < 3 {
				return windows.ERROR_ACCESS_DENIED
			}
		}
		return real(path, dir)
	}
	t.Cleanup(func() { applyACL = real; ResetWarnings() })
	ResetWarnings()
	if err := PrivateDir(root); err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Fatalf("expected two transient retries before success, got %d ACL calls", calls)
	}
	if err := VerifyPrivateFile(child); err != nil {
		t.Fatalf("transient access denied was not repaired: %v", err)
	}
	if warnings := Warnings(); len(warnings) != 0 {
		t.Fatalf("a transient access denied must not become a warning: %v", warnings)
	}
}

func mustPrincipals(t *testing.T) principals {
	t.Helper()
	p, err := currentPrincipals()
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPrivateDirIgnoresFilesThatVanishDuringTheWalk(t *testing.T) {
	root := t.TempDir()
	pr, err := currentPrincipals()
	if err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			p := filepath.Join(root, fmt.Sprintf("churn%d.tmp", i%7))
			_ = os.WriteFile(p, []byte("x"), 0o644)
			_ = setLoose(p)
			_ = os.Remove(p)
		}
	}()
	defer func() { close(stop); wg.Wait() }()
	ResetWarnings()
	for i := 0; i < 20; i++ {
		protectTree(pr, root)
	}
	for _, w := range Warnings() {
		t.Errorf("a vanished file must not be reported: %s", w)
	}
}

func TestPrivateDirSkipsJunctionsWithoutFollowingThem(t *testing.T) {
	base := t.TempDir()
	external := filepath.Join(base, "external")
	if err := os.MkdirAll(external, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(external, "keep")
	if err := os.WriteFile(outside, []byte("elsewhere"), 0o644); err != nil {
		t.Fatal(err)
	}
	looseACL(t, outside)
	root := filepath.Join(base, "data")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", filepath.Join(root, "backups"), external).CombinedOutput(); err != nil {
		t.Skipf("junctions unavailable: %v %s", err, out)
	}
	ResetWarnings()
	if err := PrivateDir(root); err != nil {
		t.Fatalf("a junction in the tree must not stop the start: %v", err)
	}
	if err := VerifyPrivateDir(root); err != nil {
		t.Fatal(err)
	}
	if VerifyPrivateFile(outside) == nil {
		t.Fatal("the junction target was changed")
	}
	if len(Warnings()) != 1 || !strings.Contains(Warnings()[0], "backups") {
		t.Fatalf("the skipped junction must be reported once: %v", Warnings())
	}
}

func TestPrivateDirTreatsAChildItCannotProtectAsAWarning(t *testing.T) {
	root := filepath.Join(t.TempDir(), "data")
	stuck := filepath.Join(root, "locked.db")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stuck, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	looseACL(t, stuck)
	real := applyACL
	applyACL = func(path string, dir bool) error {
		if path == stuck {
			return &os.PathError{Op: "SetNamedSecurityInfo", Path: path, Err: windows.ERROR_ACCESS_DENIED}
		}
		return real(path, dir)
	}
	t.Cleanup(func() { applyACL = real })
	ResetWarnings()
	if err := PrivateDir(root); err != nil {
		t.Fatalf("a child that cannot be protected must not fail the start: %v", err)
	}
	if err := VerifyPrivateDir(root); err != nil {
		t.Fatal(err)
	}
	if got := Warnings(); len(got) != 1 || !strings.Contains(got[0], "locked.db") {
		t.Fatalf("expected one warning naming the child, got %v", got)
	}
}

func TestPrivateDirRefusesASharedRootButOnlyWarnsForAnUnsharedOne(t *testing.T) {
	denied := func(path string, dir bool) error { return windows.ERROR_ACCESS_DENIED }
	real := applyACL
	t.Cleanup(func() { applyACL = real })

	shared := filepath.Join(t.TempDir(), "shared")
	if err := os.MkdirAll(shared, 0o755); err != nil {
		t.Fatal(err)
	}
	looseACL(t, shared)
	applyACL = denied
	ResetWarnings()
	err := PrivateDir(shared)
	if err == nil || !strings.Contains(err.Error(), "shared") {
		t.Fatalf("a root other accounts can write to must be refused, got %v", err)
	}

	// not protected, but nobody else can reach it: the same failure is only a warning
	quiet := filepath.Join(t.TempDir(), "quiet")
	if err := os.MkdirAll(quiet, 0o755); err != nil {
		t.Fatal(err)
	}
	pr, err := currentPrincipals()
	if err != nil {
		t.Fatal(err)
	}
	// only the user, without the inheritance flags PrivateDir writes
	userOnly, err := windows.SecurityDescriptorFromString("D:P(A;;FA;;;" + pr.user.String() + ")")
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, _ := userOnly.DACL()
	if err := windows.SetNamedSecurityInfo(quiet, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
	ResetWarnings()
	if err := PrivateDir(quiet); err != nil {
		t.Fatalf("an unshared root must start with a warning: %v", err)
	}
	if got := Warnings(); len(got) != 1 || !strings.Contains(got[0], "quiet") {
		t.Fatalf("expected one warning for the root, got %v", got)
	}
}

func TestPrivateFileLeavesAnInheritedPrivateFileAlone(t *testing.T) {
	root := filepath.Join(t.TempDir(), "data")
	if err := PrivateDir(root); err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(root, "db")
	if err := os.WriteFile(f, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	real := applyACL
	calls := 0
	applyACL = func(path string, dir bool) error { calls++; return real(path, dir) }
	t.Cleanup(func() { applyACL = real })
	if err := PrivateFile(f); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatalf("a file that inherited the private ACL was rewritten %d times", calls)
	}
}

// Windows renders SYSTEM and service accounts with aliases (SY, NS), and the user can be one of the
// built-in principals: the policy has to compare SIDs, and it must not list an account twice.
func TestPrivatePolicyComparesSIDsForServiceAccounts(t *testing.T) {
	for _, tc := range []struct {
		name string
		sid  windows.WELL_KNOWN_SID_TYPE
		sddl string
	}{
		{"SYSTEM", windows.WinLocalSystemSid, "O:SYD:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)"},
		{"NETWORK SERVICE", windows.WinNetworkServiceSid, "O:NSD:P(A;OICI;FA;;;NS)(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)"},
		{"LOCAL SERVICE", windows.WinLocalServiceSid, "O:LSD:P(A;OICI;FA;;;LS)(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			user, err := windows.CreateWellKnownSid(tc.sid)
			if err != nil {
				t.Fatal(err)
			}
			system, _ := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
			admins, _ := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
			pr := principals{user: user}
			for _, sid := range []*windows.SID{user, system, admins} {
				if !pr.has(sid) {
					pr.list = append(pr.list, sid)
				}
			}
			seen := map[string]bool{}
			for _, sid := range pr.list {
				if seen[sid.String()] {
					t.Fatalf("%s listed twice", sid)
				}
				seen[sid.String()] = true
			}
			if tc.name == "SYSTEM" && strings.Count(pr.sddl(true), "(A;") != 2 {
				t.Fatalf("SYSTEM must get one ACE, not two: %s", pr.sddl(true))
			}
			sd, err := windows.SecurityDescriptorFromString(tc.sddl)
			if err != nil {
				t.Fatal(err)
			}
			control, _, _ := sd.Control()
			owner, _, _ := sd.Owner()
			pol := daclPolicy{protected: control&windows.SE_DACL_PROTECTED != 0, hasDACL: true, owner: owner}
			dacl, _, _ := sd.DACL()
			for i := uint32(0); i < uint32(dacl.AceCount); i++ {
				var raw *windows.ACCESS_ALLOWED_ACE
				if err := windows.GetAce(dacl, i, &raw); err != nil {
					t.Fatal(err)
				}
				sid, _ := (*windows.SID)(unsafe.Pointer(&raw.SidStart)).Copy()
				pol.aces = append(pol.aces, ace{allow: true, flags: raw.Header.AceFlags, mask: raw.Mask, sid: sid})
			}
			if err := pr.private(pol, true); err != nil {
				t.Fatalf("a private descriptor of %s was rejected: %v", tc.name, err)
			}
		})
	}
}

func TestPrivatePolicyRejectsBroadAndSharedACLs(t *testing.T) {
	pr, err := currentPrincipals()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	looseACL(t, dir)
	pol, err := readPolicy(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := pr.private(pol, true); err == nil {
		t.Fatal("Everyone:Full is not private")
	}
	if sid, shared := pr.otherWriters(pol); !shared || sid != "S-1-1-0" {
		t.Fatalf("Everyone can write: %q %v", sid, shared)
	}
	readOnly, _ := windows.SecurityDescriptorFromString("D:P(A;OICI;FR;;;WD)")
	dacl, _, _ := readOnly.DACL()
	if err := windows.SetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
	pol, _ = readPolicy(dir)
	if _, shared := pr.otherWriters(pol); shared {
		t.Fatal("read-only exposure is a warning, not a shared root")
	}
	if !errors.Is(windows.ERROR_FILE_NOT_FOUND, os.ErrNotExist) {
		t.Fatal("vanished files are recognised through ErrNotExist")
	}
}

// A data root that is itself a junction (a data folder moved to another drive with mklink /J) is
// protected through the junction: the folder it leads to becomes private.
func TestPrivateDirProtectsTheFolderARootJunctionLeadsTo(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "moved")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "Masamong")
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, real).CombinedOutput(); err != nil {
		t.Skipf("junctions unavailable: %v %s", err, out)
	}
	ResetWarnings()
	if err := PrivateDir(link); err != nil {
		t.Fatalf("a root reached through a junction must start: %v", err)
	}
	if err := VerifyPrivateDir(real); err != nil {
		t.Fatalf("the folder behind the junction is not private: %v", err)
	}
}
