package cmd

import (
	"fmt"
	"os"
	"runtime"
	"runtime/debug"
	"strings"
	"time"
)

// Build-time variables — can be injected via ldflags, but also
// auto-detected from Go build info (VCS stamps, pseudo-versions).
var (
	Version    string // set from main.go
	CommitSHA  string
	CommitDate string
	CommitMsg  string
	// Dirty reports whether the binary was built from a working tree with
	// uncommitted changes (Go's vcs.modified stamp). Without it --version
	// shows only the *last commit's* SHA and date, so a binary built two
	// minutes ago from edited sources reads as days old and a successful
	// `go install` looks like it did nothing.
	Dirty bool
	// BuiltAt is the executable's mtime — when this binary was actually
	// produced, as opposed to when its last commit was made. Best-effort:
	// empty when the path can't be resolved or stat'ed.
	BuiltAt string
)

func ResolveVersion(injected string) string {
	buildInfoVersion := ""
	if bi, ok := debug.ReadBuildInfo(); ok {
		buildInfoVersion = bi.Main.Version
	}
	return resolvedVersion(injected, buildInfoVersion)
}

func resolvedVersion(injected, buildInfoVersion string) string {
	if v := normalizeVersion(injected); v != "" {
		return v
	}
	if v := normalizeVersion(buildInfoVersion); v != "" {
		return v
	}
	return "dev"
}

func normalizeVersion(version string) string {
	version = strings.TrimSpace(version)
	if version == "" || version == "(devel)" {
		return ""
	}
	return strings.TrimPrefix(version, "v")
}

func init() {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return
	}
	// VCS info from local git build
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			if CommitSHA == "" {
				CommitSHA = s.Value
			}
		case "vcs.time":
			if CommitDate == "" {
				if t, err := time.Parse(time.RFC3339, s.Value); err == nil {
					CommitDate = t.Format("2006-01-02 15:04")
				} else {
					CommitDate = s.Value
				}
			}
		case "vcs.modified":
			Dirty = s.Value == "true"
		}
	}
	BuiltAt = executableBuildTime()
	// Pseudo-version fallback (from go install)
	if CommitSHA == "" && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		parts := strings.Split(bi.Main.Version, "-")
		if len(parts) == 3 {
			CommitSHA = parts[2]
			if t, err := time.Parse("20060102150405", parts[1]); err == nil {
				CommitDate = t.Format("2006-01-02 15:04")
			}
		}
	}
}

// executableBuildTime returns the running binary's mtime, formatted like
// CommitDate. For `go build`/`go install` that is the moment the binary was
// produced, which is the question "is this current?" actually asks — the VCS
// stamps can only answer "what was committed last".
func executableBuildTime() string {
	path, err := os.Executable()
	if err != nil {
		return ""
	}
	info, err := os.Stat(path)
	if err != nil {
		return ""
	}
	return info.ModTime().Format("2006-01-02 15:04")
}

// PrintVersion prints version info to stdout.
func PrintVersion() {
	f := Fmt
	short := CommitSHA
	if len(short) > 7 {
		short = short[:7]
	}

	fmt.Printf("chb %s%s%s", f.Bold, Version, f.Reset)
	if short != "" {
		fmt.Printf(" %s(%s, %s)%s", f.Dim, short, CommitDate, f.Reset)
	}
	fmt.Println()
	fmt.Printf("  %sOS:%s     %s/%s\n", f.Cyan, f.Reset, runtime.GOOS, runtime.GOARCH)
	if BuiltAt != "" {
		fmt.Printf("  %sBuilt:%s  %s\n", f.Cyan, f.Reset, BuiltAt)
	}
	// Spell the +dirty suffix out. It is the difference between "this is
	// release 6128156" and "this is 6128156 plus whatever is in the working
	// tree", and it is far too easy to miss inside the version string.
	if Dirty {
		fmt.Printf("  %sSource:%s %suncommitted changes on top of %s%s\n",
			f.Cyan, f.Reset, f.Yellow, short, f.Reset)
	}
	if CommitMsg != "" {
		fmt.Printf("  %sCommit:%s %s\n", f.Cyan, f.Reset, firstLine(CommitMsg))
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
