//go:build windows

package indexer

import (
	"errors"
	"os"
	"time"

	"golang.org/x/sys/windows"
)

// replaceFile renames tmp over path. Windows refuses to replace a file that
// another process has open or is replacing at that moment (a thumbnail being
// served, two workers writing the same thumbnail), so those refusals are
// retried for up to about four seconds.
func replaceFile(tmp, path string) error {
	var err error
	for i := 1; i <= 40; i++ {
		if err = os.Rename(tmp, path); err == nil || !transientRename(err) {
			return err
		}
		time.Sleep(time.Duration(i) * 5 * time.Millisecond)
	}
	return err
}

func transientRename(err error) bool {
	return errors.Is(err, windows.ERROR_ACCESS_DENIED) || errors.Is(err, windows.ERROR_SHARING_VIOLATION)
}
