//go:build !unix && !windows

package indexer

import "errors"

// ErrIndexRunning means another `archivis index` is using this catalogue.
var ErrIndexRunning = errors.New("another archivis index is already running on this catalogue")

// LockIndex is a no-op where flock is unavailable.
func LockIndex(dir string) (unlock func(), err error) { return func() {}, nil }
