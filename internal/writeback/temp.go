package writeback

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"golang.org/x/sys/unix"
)

const (
	writebackTempDirName = "argocd-tag-updater"
	writebackClonePrefix = "clone-"
	writebackLockName    = ".writeback.lock"
)

// PrepareWritebackTempDir serializes startup cleanup and keeps the lock until
// the caller releases it after the manager has stopped.
func PrepareWritebackTempDir() (func(), error) {
	root, err := ensureWritebackTempDir()
	if err != nil {
		return nil, err
	}

	release, err := lockWritebackTempDir(root)
	if err != nil {
		return nil, err
	}
	if err := removeInterruptedClones(root); err != nil {
		release()
		return nil, fmt.Errorf("remove interrupted write-back clones: %w", err)
	}

	return release, nil
}

func newCloneDirectory() (string, error) {
	root, err := ensureWritebackTempDir()
	if err != nil {
		return "", err
	}
	cloneDirectoryPath, err := os.MkdirTemp(root, writebackClonePrefix)
	if err != nil {
		return "", fmt.Errorf("create write-back clone directory: %w", err)
	}
	return cloneDirectoryPath, nil
}

func ensureWritebackTempDir() (string, error) {
	root := filepath.Join(os.TempDir(), writebackTempDirName)
	if err := os.Mkdir(root, 0o700); err != nil && !os.IsExist(err) {
		return "", fmt.Errorf("create write-back temp root %s: %w", root, err)
	}
	var rootStatus unix.Stat_t
	if err := unix.Lstat(root, &rootStatus); err != nil {
		return "", fmt.Errorf("inspect write-back temp root %s: %w", root, err)
	}
	if rootStatus.Mode&unix.S_IFMT != unix.S_IFDIR {
		return "", fmt.Errorf("write-back temp root %s is not a real directory", root)
	}
	if rootStatus.Uid != uint32(os.Geteuid()) {
		return "", fmt.Errorf("write-back temp root %s is owned by uid %d, current uid is %d", root, rootStatus.Uid, os.Geteuid())
	}
	if rootStatus.Mode&0o7777 != 0o700 {
		return "", fmt.Errorf("write-back temp root %s must be owner-only mode 0700", root)
	}
	return root, nil
}

func lockWritebackTempDir(root string) (func(), error) {
	lockPath := filepath.Join(root, writebackLockName)
	lockDescriptor, err := unix.Open(lockPath, unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open write-back temp lock %s: %w", lockPath, err)
	}
	var lockStatus unix.Stat_t
	if err := unix.Fstat(lockDescriptor, &lockStatus); err != nil {
		_ = unix.Close(lockDescriptor)
		return nil, fmt.Errorf("inspect write-back temp lock %s: %w", lockPath, err)
	}
	if lockStatus.Mode&unix.S_IFMT != unix.S_IFREG || lockStatus.Uid != uint32(os.Geteuid()) || lockStatus.Mode&0o7777 != 0o600 {
		_ = unix.Close(lockDescriptor)
		return nil, fmt.Errorf("write-back temp lock %s must be a current-user regular file with owner-only permissions", lockPath)
	}
	if err := unix.Flock(lockDescriptor, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = unix.Close(lockDescriptor)
		return nil, fmt.Errorf("lock write-back temp root %s: %w", root, err)
	}
	lockFile := os.NewFile(uintptr(lockDescriptor), lockPath)
	var releaseOnce sync.Once
	return func() {
		releaseOnce.Do(func() {
			_ = unix.Flock(lockDescriptor, unix.LOCK_UN)
			_ = lockFile.Close()
		})
	}, nil
}

func removeInterruptedClones(root string) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		return fmt.Errorf("read write-back temp root %s: %w", root, err)
	}
	for _, entry := range entries {
		if len(entry.Name()) < len(writebackClonePrefix) || entry.Name()[:len(writebackClonePrefix)] != writebackClonePrefix {
			continue
		}
		candidatePath := filepath.Join(root, entry.Name())
		var candidateStatus unix.Stat_t
		if err := unix.Lstat(candidatePath, &candidateStatus); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return fmt.Errorf("inspect candidate %s: %w", candidatePath, err)
		}
		if candidateStatus.Mode&unix.S_IFMT != unix.S_IFDIR || candidateStatus.Uid != uint32(os.Geteuid()) {
			continue
		}
		if err := os.RemoveAll(candidatePath); err != nil {
			return fmt.Errorf("remove candidate %s: %w", candidatePath, err)
		}
	}
	return nil
}
