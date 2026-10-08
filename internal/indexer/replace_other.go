//go:build !windows

package indexer

import "os"

// replaceFile renames tmp over path (atomic on POSIX filesystems).
func replaceFile(tmp, path string) error { return os.Rename(tmp, path) }
