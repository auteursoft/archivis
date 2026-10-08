//go:build windows

package indexer

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

// ErrIndexRunning means another `archivis index` is using this catalogue.
var ErrIndexRunning = errors.New("another archivis index is already running on this catalogue")

// LockIndex takes the catalogue's index lock (see lock_unix.go) using a
// Windows byte-range lock, which the OS releases if the process dies.
func LockIndex(dir string) (unlock func(), err error) {
	f, err := os.OpenFile(filepath.Join(dir, "index.lock"), os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	h := windows.Handle(f.Fd())
	ol := new(windows.Overlapped)
	err = windows.LockFileEx(h, windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, ol)
	if err != nil {
		f.Close()
		if errors.Is(err, windows.ERROR_LOCK_VIOLATION) || errors.Is(err, windows.ERROR_IO_PENDING) {
			return nil, fmt.Errorf("%w (%s)", ErrIndexRunning, dir)
		}
		return nil, err
	}
	return func() { windows.UnlockFileEx(h, 0, 1, 0, ol); f.Close() }, nil
}
