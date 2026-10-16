// One swarm must never assign two queue owners to the same physical directory,
// including a new child beneath aliased existing parents. This is a read only
// identity check; the owning daemon creates its private directory later.
package main

import (
	"errors"
	"os"
	"path/filepath"
)

// A new reader refuses unsafe queue entries; an existing owner may replace
// one atomically without following it or importing its contents.
var errClaimQueueUnsafeFile = errors.New("claim queue entry is not a private owned regular file")

func canonicalClaimStateDirectory(path string) (string, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return "", errors.New("claim state directory is not canonical and absolute")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err == nil || !errors.Is(err, os.ErrNotExist) {
		return resolved, err
	}
	parent := filepath.Dir(path)
	if parent == path {
		return "", err
	}
	resolved, err = canonicalClaimStateDirectory(parent)
	if err != nil {
		return "", err
	}
	return filepath.Join(resolved, filepath.Base(path)), nil
}
