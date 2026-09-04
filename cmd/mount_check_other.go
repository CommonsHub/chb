//go:build !unix

package cmd

// pathIsMountPoint can't be answered without unix stat semantics, so the
// mount guard is a no-op on these platforms.
func pathIsMountPoint(path string) (mounted bool, known bool) {
	return false, false
}
