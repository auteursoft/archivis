//go:build unix

package indexer

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// lockFile takes an exclusive lock on the file, or fails at once with busy
// if another process holds it. The OS releases it if the process dies, so
// it can never go stale.
func lockFile(path string, busy error) (unlock func(), err error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("%w (%s)", busy, filepath.Dir(path))
		}
		return nil, err
	}
	f.Truncate(0)
	fmt.Fprintf(f, "%d\n", os.Getpid())
	return func() { syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, nil
}
