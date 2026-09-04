//go:build unix

package cmd

import (
	"os"
	"path/filepath"
	"syscall"
)

// pathIsMountPoint reports whether path is the root of a mounted filesystem.
// A directory is a mount point when its device differs from its parent's.
// The second return value is false when the question can't be answered on
// this platform, in which case callers must skip the check rather than guess.
func pathIsMountPoint(path string) (mounted bool, known bool) {
	path = filepath.Clean(path)
	parent := filepath.Dir(path)
	if parent == path {
		// The filesystem root is always mounted.
		return true, true
	}

	info, err := os.Stat(path)
	if err != nil {
		// Missing (or unreadable) means the drive isn't mounted here.
		return false, true
	}
	parentInfo, err := os.Stat(parent)
	if err != nil {
		return false, true
	}

	stat, ok := info.Sys().(*syscall.Stat_t)
	parentStat, parentOK := parentInfo.Sys().(*syscall.Stat_t)
	if !ok || !parentOK {
		return false, false
	}
	return stat.Dev != parentStat.Dev, true
}
