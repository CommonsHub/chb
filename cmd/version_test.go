package cmd

import (
	"testing"
	"time"
)

func TestResolvedVersionPrefersInjectedVersion(t *testing.T) {
	got := resolvedVersion("2.3.4", "v2.3.3")
	if got != "2.3.4" {
		t.Fatalf("expected injected version, got %q", got)
	}
}

func TestResolvedVersionFallsBackToBuildInfoVersion(t *testing.T) {
	got := resolvedVersion("", "v2.3.3")
	if got != "2.3.3" {
		t.Fatalf("expected build info version, got %q", got)
	}
}

func TestResolvedVersionFallsBackToDev(t *testing.T) {
	got := resolvedVersion("", "(devel)")
	if got != "dev" {
		t.Fatalf("expected dev fallback, got %q", got)
	}
}

func TestNormalizeVersionStripsPrefix(t *testing.T) {
	got := normalizeVersion("v2.3.3")
	if got != "2.3.3" {
		t.Fatalf("expected stripped version, got %q", got)
	}
}

// TestResolvedVersionKeepsDirtySuffix pins that the "+dirty" marker survives
// normalization. It is the only thing in the version string distinguishing a
// binary built from edited sources from the plain commit it sits on.
func TestResolvedVersionKeepsDirtySuffix(t *testing.T) {
	got := resolvedVersion("", "v0.0.0-20260825083546-612815663041+dirty")
	want := "0.0.0-20260825083546-612815663041+dirty"
	if got != want {
		t.Errorf("resolvedVersion() = %q, want %q", got, want)
	}
}

// TestExecutableBuildTimeIsPopulated guards the fallback contract: the helper
// either returns a parseable timestamp or an empty string, never a partial
// value that would render as a broken line in --version.
func TestExecutableBuildTimeIsPopulated(t *testing.T) {
	got := executableBuildTime()
	if got == "" {
		t.Skip("executable path not resolvable in this environment")
	}
	if _, err := time.Parse("2006-01-02 15:04", got); err != nil {
		t.Errorf("executableBuildTime() = %q, not a valid timestamp: %v", got, err)
	}
}
