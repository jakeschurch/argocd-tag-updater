package writeback

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPrepareWritebackTempDirRemovesOnlyOwnedCloneDirectories(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	release, err := PrepareWritebackTempDir()
	if err != nil {
		t.Fatal(err)
	}
	release()

	root := filepath.Join(os.TempDir(), writebackTempDirName)
	staleClone := filepath.Join(root, writebackClonePrefix+"stale")
	if err := os.Mkdir(staleClone, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staleClone, "payload"), []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	foreignDirectory := filepath.Join(root, "foreign-data")
	if err := os.Mkdir(foreignDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	linkTarget := filepath.Join(t.TempDir(), "outside")
	if err := os.Mkdir(linkTarget, 0o700); err != nil {
		t.Fatal(err)
	}
	cloneLink := filepath.Join(root, writebackClonePrefix+"link")
	if err := os.Symlink(linkTarget, cloneLink); err != nil {
		t.Fatal(err)
	}

	release, err = PrepareWritebackTempDir()
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if _, err := os.Lstat(staleClone); !os.IsNotExist(err) {
		t.Fatalf("stale clone remains or cannot be inspected: %v", err)
	}
	if _, err := os.Stat(foreignDirectory); err != nil {
		t.Fatalf("foreign directory was removed: %v", err)
	}
	if info, err := os.Lstat(cloneLink); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("clone symlink was removed or replaced: info=%v err=%v", info, err)
	}
	if info, err := os.Stat(linkTarget); err != nil || !info.IsDir() {
		t.Fatalf("symlink target was changed: info=%v err=%v", info, err)
	}
}

func TestPrepareWritebackTempDirLockPreventsConcurrentCleanup(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	firstRelease, err := PrepareWritebackTempDir()
	if err != nil {
		t.Fatal(err)
	}
	defer firstRelease()

	root := filepath.Join(os.TempDir(), writebackTempDirName)
	activeClone := filepath.Join(root, writebackClonePrefix+"active")
	if err := os.Mkdir(activeClone, 0o700); err != nil {
		t.Fatal(err)
	}

	if secondRelease, err := PrepareWritebackTempDir(); err == nil {
		secondRelease()
		t.Fatal("second process acquired the active temp-root lock")
	}
	if info, err := os.Stat(activeClone); err != nil || !info.IsDir() {
		t.Fatalf("active clone was removed: info=%v err=%v", info, err)
	}
}

func TestPrepareWritebackTempDirReleaseIsIdempotent(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	firstRelease, err := PrepareWritebackTempDir()
	if err != nil {
		t.Fatal(err)
	}
	firstRelease()

	secondRelease, err := PrepareWritebackTempDir()
	if err != nil {
		t.Fatal(err)
	}
	defer secondRelease()
	firstRelease()

	if thirdRelease, err := PrepareWritebackTempDir(); err == nil {
		thirdRelease()
		t.Fatal("repeated release unlocked a newer temp-root lock")
	}
}

func TestPrepareWritebackTempDirRejectsSymlinkRoot(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	root := filepath.Join(os.TempDir(), writebackTempDirName)
	target := t.TempDir()
	if err := os.Symlink(target, root); err != nil {
		t.Fatal(err)
	}
	if release, err := PrepareWritebackTempDir(); err == nil {
		release()
		t.Fatal("symlink temp root was accepted")
	}
	if info, err := os.Stat(target); err != nil || !info.IsDir() {
		t.Fatalf("symlink target was changed: info=%v err=%v", info, err)
	}
}
