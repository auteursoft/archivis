package indexer

import (
	"errors"
	"path/filepath"
)

// ErrIndexRunning means another `archivis index` is using this catalogue.
var ErrIndexRunning = errors.New("another archivis index is already running on this catalogue")

// ErrGroupsRunning means another process is regrouping this catalogue.
var ErrGroupsRunning = errors.New("another archivis process is already regrouping this catalogue")

// LockIndex takes the catalogue's index lock so two index runs (say, a
// nightly job and a manual first index) never write the same catalogue at
// once.
func LockIndex(dir string) (unlock func(), err error) {
	return lockFile(filepath.Join(dir, "index.lock"), ErrIndexRunning)
}

// LockGroups takes the catalogue's regrouping lock, so an index run and the
// web interface never rebuild the groups at once. It is separate from the
// index lock: regrouping never stops an index run from starting.
func LockGroups(dir string) (unlock func(), err error) {
	return lockFile(filepath.Join(dir, "groups.lock"), ErrGroupsRunning)
}
