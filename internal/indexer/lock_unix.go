//go:build unix

package indexer

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// ErrIndexRunning means another `archivis index` is using this catalogue.
var ErrIndexRunning = errors.New("another archivis index is already running on this catalogue")

// LockIndex takes the catalogue's index lock so two index runs (say, a
// nightly job and a manual first index) never write the same catalogue at
// once. The OS releases it if the process dies, so it can never go stale.
func LockIndex(dir string) (unlock func(), err error) {
	f, err := os.OpenFile(filepath.Join(dir, "index.lock"), os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("%w (%s)", ErrIndexRunning, dir)
		}
		return nil, err
	}
	f.Truncate(0)
	fmt.Fprintf(f, "%d\n", os.Getpid())
	return func() { syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, nil
}
