package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestBusinessLinesDefaults pins the split the member report is built on: the
// space and its standing costs on one side, hiring rooms out and catering them
// on the other.
func TestBusinessLinesDefaults(t *testing.T) {
	b := LoadBusinessLines()
	for slug, want := range map[string]string{
		"membership": LineHub,
		"coworking":  LineHub,
		"rent":       LineHub,
		"utilities":  LineHub,
		"fridge":     LineHub,
		"rental":     LineEvents,
		"catering":   LineEvents,
		"ticket":     LineEvents,
		"accounting": LineShared,
		"stripe_fee": LineShared,
	} {
		if got := b.LineFor(slug); got != want {
			t.Errorf("LineFor(%q) = %q, want %q", slug, got, want)
		}
	}
}

// TestBusinessLinesUnknownIsShared is the safety property: an unmapped category
// must surface as unassigned rather than silently inflate one line's result.
func TestBusinessLinesUnknownIsShared(t *testing.T) {
	b := LoadBusinessLines()
	for _, slug := range []string{"", "(untagged)", "something-new-in-2027"} {
		if got := b.LineFor(slug); got != LineShared {
			t.Errorf("LineFor(%q) = %q, want %q", slug, got, LineShared)
		}
	}
}

func TestBusinessLinesCaseInsensitive(t *testing.T) {
	b := LoadBusinessLines()
	if b.LineFor("  Coworking  ") != LineHub {
		t.Error("category lookup should trim and ignore case")
	}
}

func TestBusinessLinesAccounts(t *testing.T) {
	b := LoadBusinessLines()
	if b.AccountLineFor("fridge") != LineHub {
		t.Error("the fridge wallet is dedicated to the hub")
	}
	// The general bank and Stripe accounts serve both lines.
	if b.AccountLineFor("kbc") != LineShared {
		t.Error("a shared rail must not be attributed to one line")
	}
}

// TestLoadBusinessLinesMergesLocalFile covers the override: a local file only
// has to name what it changes, and everything else keeps the default.
func TestLoadBusinessLinesMergesLocalFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("APP_DATA_DIR", dir)
	if err := os.MkdirAll(filepath.Join(dir, "settings"), 0o755); err != nil {
		t.Fatal(err)
	}
	local := BusinessLines{
		Categories: map[string]string{
			"catering": LineHub,    // reassigned
			"grant":    "NONSENSE", // unrecognised value falls back to shared
		},
	}
	data, _ := json.Marshal(local)
	if err := os.WriteFile(filepath.Join(dir, "settings", "business-lines.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	b := LoadBusinessLines()
	if got := b.LineFor("catering"); got != LineHub {
		t.Errorf("local override ignored: catering = %q", got)
	}
	if got := b.LineFor("grant"); got != LineShared {
		t.Errorf("an unrecognised line value should fall back to shared, got %q", got)
	}
	if got := b.LineFor("rental"); got != LineEvents {
		t.Errorf("untouched defaults should survive the merge: rental = %q", got)
	}
}
