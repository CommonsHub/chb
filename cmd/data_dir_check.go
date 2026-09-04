package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// EnsureWritableDataDir verifies that DATA_DIR can be created (if needed) and written to.
// It returns the resolved directory path or an error with context about why the preflight failed.
func EnsureWritableDataDir() (string, error) {
	dataDir := filepath.Clean(resolveDataDir())

	// Before creating anything: an unmounted external drive must not be
	// mistaken for an empty data dir.
	if err := ensureRootMounted("DATA_DIR", dataDir); err != nil {
		return dataDir, err
	}

	if info, err := os.Stat(dataDir); err == nil && !info.IsDir() {
		return dataDir, fmt.Errorf("DATA_DIR %s is not a directory (%s)", dataDir, info.Mode().Type())
	}

	if err := os.MkdirAll(dataDir, dataRootDirMode); err != nil {
		return dataDir, fmt.Errorf("cannot create DATA_DIR %s: %w%s", dataDir, err, dataDirAccessContext(filepath.Dir(dataDir)))
	}

	testFile, err := os.CreateTemp(dataDir, ".chb-write-test-*")
	if err != nil {
		return dataDir, fmt.Errorf("cannot write to DATA_DIR %s: %w%s", dataDir, err, dataDirAccessContext(dataDir))
	}

	testPath := testFile.Name()
	if _, err := testFile.WriteString("ok\n"); err != nil {
		testFile.Close()
		_ = os.Remove(testPath)
		return dataDir, fmt.Errorf("cannot write to DATA_DIR %s: %w%s", dataDir, err, dataDirAccessContext(dataDir))
	}
	if err := testFile.Close(); err != nil {
		_ = os.Remove(testPath)
		return dataDir, fmt.Errorf("cannot finalize write test in DATA_DIR %s: %w%s", dataDir, err, dataDirAccessContext(dataDir))
	}
	if err := os.Remove(testPath); err != nil {
		return dataDir, fmt.Errorf("cannot clean up write test file in DATA_DIR %s: %w%s", dataDir, err, dataDirAccessContext(dataDir))
	}

	normalizeDataDir(dataDir)
	return dataDir, nil
}

func dataDirAccessContext(path string) string {
	info, err := os.Stat(path)
	if err != nil {
		return ""
	}
	return fmt.Sprintf(" (path %s exists with mode %o)", path, info.Mode().Perm())
}

// mountRoots are the directories under which desktops and distros mount
// external media. A data root below one of these is only meaningful while the
// drive is actually mounted — writing into a bare mount point silently creates
// a shadow tree on the internal disk that later runs then read as if it were
// the real mirror. Overridden in tests.
var mountRoots = []string{"/media", "/run/media", "/mnt", "/Volumes"}

const (
	// skipMountCheckEnv disables the guard for operators who really do keep
	// data on a plain directory below one of the mountRoots.
	skipMountCheckEnv = "CHB_SKIP_MOUNT_CHECK"
	// requireMountEnv names an explicit path that must be a mount point,
	// for data roots outside the well-known mountRoots (NFS shares, /srv/…).
	requireMountEnv = "CHB_REQUIRE_MOUNT"
)

// EnsureDataRootsMounted refuses to continue when APP_DATA_DIR or DATA_DIR
// lives on an external drive that isn't mounted. Both AppDataDir and
// EnsureWritableDataDir create their directory tree on demand, so without this
// check an unmounted drive is indistinguishable from an empty one: chb would
// happily mirror providers into the mount point on the internal disk.
//
// Call it before anything touches either root — including the diagnostics log,
// which lazily creates $DATA_DIR/logs on the first warning.
func EnsureDataRootsMounted() error {
	if err := ensureRootMounted("APP_DATA_DIR", appDataDirPath()); err != nil {
		return err
	}
	return ensureRootMounted("DATA_DIR", resolveDataDir())
}

func ensureRootMounted(label, dir string) error {
	if isTruthyEnv(os.Getenv(skipMountCheckEnv)) {
		return nil
	}

	candidates := mountCandidates(dir)
	if len(candidates) == 0 {
		return nil
	}

	// Deepest first: the drive may be mounted anywhere between the media
	// root and the data directory itself.
	for i := len(candidates) - 1; i >= 0; i-- {
		mounted, known := pathIsMountPoint(candidates[i])
		if !known {
			return nil
		}
		if mounted {
			return nil
		}
	}

	if required := strings.TrimSpace(os.Getenv(requireMountEnv)); required != "" {
		return fmt.Errorf("%s is not mounted, so %s %s is unsafe to write (%s). Mount it and re-run, "+
			"or set %s=1 to write to the internal disk anyway",
			filepath.Clean(required), label, dir, requireMountEnv, skipMountCheckEnv)
	}

	return fmt.Errorf("%s %s is on a drive that is not mounted — nothing is mounted at or above it "+
		"(expected mount point: %s). Mount the drive and re-run, or set %s=1 to write to the internal disk anyway",
		label, dir, likelyMountPoint(candidates), skipMountCheckEnv)
}

// mountCandidates returns the ancestors of dir (shallowest first, dir last)
// that could carry the mount, or nil when dir isn't on external media.
func mountCandidates(dir string) []string {
	dir = filepath.Clean(dir)

	if required := strings.TrimSpace(os.Getenv(requireMountEnv)); required != "" {
		return []string{filepath.Clean(required)}
	}

	for _, root := range mountRoots {
		root = filepath.Clean(root)
		if dir == root || !strings.HasPrefix(dir, root+string(filepath.Separator)) {
			continue
		}
		rel, err := filepath.Rel(root, dir)
		if err != nil {
			continue
		}
		var candidates []string
		current := root
		for _, part := range strings.Split(rel, string(filepath.Separator)) {
			current = filepath.Join(current, part)
			candidates = append(candidates, current)
		}
		return candidates
	}
	return nil
}

// likelyMountPoint picks the path to name in the error: the shallowest
// candidate that doesn't exist yet (that's where the drive should appear),
// falling back to the shallowest candidate.
func likelyMountPoint(candidates []string) string {
	for _, c := range candidates {
		if _, err := os.Stat(c); err != nil {
			return c
		}
	}
	return candidates[0]
}

func isTruthyEnv(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}
