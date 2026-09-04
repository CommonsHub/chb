package cmd

import (
	"path/filepath"
	"strings"
	"testing"
)

// withFakeMountRoot points the guard at a temp dir standing in for /media.
func withFakeMountRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	prev := mountRoots
	mountRoots = []string{root}
	t.Cleanup(func() { mountRoots = prev })
	return root
}

func TestEnsureRootMountedRejectsUnmountedDrive(t *testing.T) {
	root := withFakeMountRoot(t)
	dir := filepath.Join(root, "chb", ".chb", "data")

	err := ensureRootMounted("DATA_DIR", dir)
	if err == nil {
		t.Fatalf("expected an error for an unmounted drive at %s", dir)
	}
	if want := filepath.Join(root, "chb"); !strings.Contains(err.Error(), want) {
		t.Errorf("error should name the expected mount point %s, got: %v", want, err)
	}
}

func TestEnsureRootMountedIgnoresPathsOutsideMediaRoots(t *testing.T) {
	withFakeMountRoot(t)
	if err := ensureRootMounted("APP_DATA_DIR", filepath.Join(t.TempDir(), ".chb")); err != nil {
		t.Fatalf("paths outside the media roots must not be guarded: %v", err)
	}
}

func TestEnsureRootMountedSkipEnv(t *testing.T) {
	root := withFakeMountRoot(t)
	t.Setenv(skipMountCheckEnv, "1")
	if err := ensureRootMounted("DATA_DIR", filepath.Join(root, "chb", ".chb", "data")); err != nil {
		t.Fatalf("%s=1 must disable the guard: %v", skipMountCheckEnv, err)
	}
}

func TestEnsureRootMountedRequireMountEnv(t *testing.T) {
	withFakeMountRoot(t)
	missing := filepath.Join(t.TempDir(), "share")
	t.Setenv(requireMountEnv, missing)

	// The data dir itself is a plain local path, but the operator declared
	// an explicit mount point that isn't mounted.
	err := ensureRootMounted("DATA_DIR", filepath.Join(t.TempDir(), ".chb", "data"))
	if err == nil || !strings.Contains(err.Error(), missing) {
		t.Fatalf("expected %s to be enforced, got: %v", requireMountEnv, err)
	}
}

func TestEnsureRootMountedAcceptsMountedAncestor(t *testing.T) {
	root := withFakeMountRoot(t)
	// The temp dir is its own filesystem in most CI sandboxes but not on a
	// plain disk, so assert via the candidate walk instead: "/" always
	// counts as mounted, which is what makes a real mounted drive pass.
	candidates := mountCandidates(filepath.Join(root, "chb", ".chb", "data"))
	if len(candidates) < 3 {
		t.Fatalf("expected the full ancestor chain, got %v", candidates)
	}
	if candidates[0] != filepath.Join(root, "chb") {
		t.Errorf("first candidate should be the drive dir, got %s", candidates[0])
	}
	if mounted, known := pathIsMountPoint("/"); known && !mounted {
		t.Error("/ must count as a mount point")
	}
}
