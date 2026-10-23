//go:build !unix && !windows

package indexer

// lockFile is a no-op where flock is unavailable.
func lockFile(path string, busy error) (unlock func(), err error) { return func() {}, nil }
