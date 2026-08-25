package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func rTx(desc, category string) Rule {
	return Rule{Match: RuleMatch{Description: desc}, Assign: RuleAssign{Category: category}}
}

func rIBAN(iban, category string) Rule {
	return Rule{Match: RuleMatch{IBAN: iban}, Assign: RuleAssign{Category: category}}
}

func marker() Rule { return Rule{Include: localRulesInclude} }

// useTestIBANAllowlist pins the shared-IBAN allowlist for the test so results
// don't depend on the embedded default rules.json.
func useTestIBANAllowlist(t *testing.T, ibans ...string) {
	t.Helper()
	set := map[string]bool{}
	for _, i := range ibans {
		set[normalizeIBAN(i)] = true
	}
	sharedRuleIBANsForTest = set
	t.Cleanup(func() { sharedRuleIBANsForTest = nil })
}

func TestRuleIsPrivate(t *testing.T) {
	useTestIBANAllowlist(t, "NL41CITI2032304805")

	cases := []struct {
		name string
		rule Rule
		want bool
	}{
		{"concrete third-party IBAN", rIBAN("BE31410000071155", "insurance"), true},
		{"spaces and case are normalised", rIBAN("be31 4100 0007 1155", "insurance"), true},
		{"glob pattern names no account", rIBAN("DE*", "consulting"), false},
		{"allowlisted corporate IBAN stays shared", rIBAN("NL41CITI2032304805", "internal_transfer"), false},
		{"no IBAN at all", rTx("*rent *", "rent"), false},
		{"splice marker", marker(), false},
	}
	for _, c := range cases {
		if got := ruleIsPrivate(c.rule); got != c.want {
			t.Errorf("%s: ruleIsPrivate = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestSpliceLocalRules(t *testing.T) {
	private := []Rule{rIBAN("BE1", "a"), rIBAN("BE2", "b")}

	t.Run("at marker", func(t *testing.T) {
		shared := []Rule{rTx("x", "1"), marker(), rTx("y", "2")}
		out := spliceLocalRules(shared, private)
		want := []string{"1", "a", "b", "2"}
		if len(out) != 4 {
			t.Fatalf("len = %d, want 4 (marker itself must not survive)", len(out))
		}
		for i, w := range want {
			if out[i].Assign.Category != w {
				t.Errorf("out[%d] = %q, want %q", i, out[i].Assign.Category, w)
			}
		}
	})

	t.Run("no marker means private first", func(t *testing.T) {
		shared := []Rule{rTx("x", "1")}
		out := spliceLocalRules(shared, private)
		if len(out) != 3 || out[0].Assign.Category != "a" || out[2].Assign.Category != "1" {
			t.Errorf("out = %+v", out)
		}
	})

	t.Run("extra markers are dropped", func(t *testing.T) {
		shared := []Rule{marker(), rTx("x", "1"), marker()}
		out := spliceLocalRules(shared, private)
		if len(out) != 3 {
			t.Fatalf("len = %d, want 3", len(out))
		}
		for _, r := range out {
			if r.Include != "" {
				t.Error("a marker leaked into the merged list")
			}
		}
	})

	t.Run("no private rules", func(t *testing.T) {
		shared := []Rule{rTx("x", "1"), marker(), rTx("y", "2")}
		out := spliceLocalRules(shared, nil)
		if len(out) != 2 {
			t.Fatalf("len = %d, want 2", len(out))
		}
	})
}

func TestMarkerRuleNeverMatches(t *testing.T) {
	m := marker()
	if m.MatchesTransaction(TransactionEntry{Amount: 10}) {
		t.Error("a splice marker must not match transactions (old-binary back-compat relies on empty assign, new binaries must skip it outright)")
	}
	if m.MatchesMove(OdooOutgoingInvoicePublic{Title: "INV/1"}, "Partner", moveKind{}) {
		t.Error("a splice marker must not match invoices")
	}
}

func TestSaveLoadRulesRoundTrip(t *testing.T) {
	t.Setenv("APP_DATA_DIR", t.TempDir())
	useTestIBANAllowlist(t)

	merged := []Rule{
		rTx("*idg*", "ticket"),
		rTx("*rent *", "rent"),
		rIBAN("BE31410000071155", "insurance"),
		rIBAN("BE47732024272380", "accounting"),
		rTx("*", "uncategorized-sweep"),
	}
	if err := SaveRules(merged); err != nil {
		t.Fatal(err)
	}

	// The shared file must hold no third-party IBANs and carry the marker at
	// the private block's position.
	sharedData, err := os.ReadFile(filepath.Join(AppSettingsDir(), "rules.json"))
	if err != nil {
		t.Fatal(err)
	}
	var shared []Rule
	if err := json.Unmarshal(sharedData, &shared); err != nil {
		t.Fatal(err)
	}
	if len(shared) != 4 {
		t.Fatalf("shared = %d entries, want 4 (3 rules + marker)", len(shared))
	}
	if shared[2].Include != localRulesInclude {
		t.Errorf("marker not at index 2: %+v", shared[2])
	}
	for _, r := range shared {
		if ruleIsPrivate(r) {
			t.Errorf("private rule leaked into rules.json: %+v", r)
		}
	}

	localData, err := os.ReadFile(filepath.Join(AppSettingsDir(), "rules.local.json"))
	if err != nil {
		t.Fatal(err)
	}
	var private []Rule
	if err := json.Unmarshal(localData, &private); err != nil {
		t.Fatal(err)
	}
	if len(private) != 2 || private[0].Match.IBAN != "BE31410000071155" {
		t.Fatalf("private = %+v", private)
	}
	if info, err := os.Stat(filepath.Join(AppSettingsDir(), "rules.local.json")); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("rules.local.json mode = %v, want 0600", info.Mode().Perm())
	}

	// Loading back must reproduce the merged order exactly.
	back, err := LoadRules()
	if err != nil {
		t.Fatal(err)
	}
	if len(back) != len(merged) {
		t.Fatalf("round-trip len = %d, want %d", len(back), len(merged))
	}
	for i := range merged {
		if back[i].Assign.Category != merged[i].Assign.Category {
			t.Errorf("round-trip[%d] = %q, want %q", i, back[i].Assign.Category, merged[i].Assign.Category)
		}
	}

	// A second save/load cycle must be stable.
	if err := SaveRules(back); err != nil {
		t.Fatal(err)
	}
	again, err := LoadRules()
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != len(merged) || again[2].Match.IBAN != "BE31410000071155" {
		t.Errorf("second round-trip drifted: %+v", again)
	}
}

func TestSaveRulesWithoutPrivateRulesKeepsMarker(t *testing.T) {
	t.Setenv("APP_DATA_DIR", t.TempDir())
	useTestIBANAllowlist(t)

	// Existing shared file with a marker at index 1 and an existing (empty)
	// local file.
	if err := writeRulesJSON(rulesPath(), []Rule{rTx("a", "1"), marker(), rTx("b", "2")}, 0644); err != nil {
		t.Fatal(err)
	}
	if err := writeRulesJSON(rulesLocalPath(), nil, 0600); err != nil {
		t.Fatal(err)
	}

	if err := SaveRules([]Rule{rTx("a", "1"), rTx("b", "2"), rTx("c", "3")}); err != nil {
		t.Fatal(err)
	}
	shared, err := readRulesFile(rulesPath())
	if err != nil {
		t.Fatal(err)
	}
	if len(shared) != 4 || shared[1].Include != localRulesInclude {
		t.Errorf("marker position lost when no private rule anchors it: %+v", shared)
	}
}

func TestMigrateIBANRulesToLocal(t *testing.T) {
	useTestIBANAllowlist(t, "NL41CITI2032304805")
	dir := t.TempDir()

	monolithic := []Rule{
		rTx("*idg*", "ticket"),           // 0: specific desc rule
		rIBAN("BE01", "donation"),        // 1: private outlier
		rTx("*rent *", "rent"),           // 2
		rIBAN("NL41CITI2032304805", "x"), // 3: allowlisted, stays shared
		rIBAN("BE02", "rental"),          // 4: private
		rIBAN("BE03", "rental"),          // 5: private
		rIBAN("DE*", "consulting"),       // 6: glob, stays shared
		rTx("*", "sweep"),                // 7: catch-all
	}
	if err := writeRulesJSON(filepath.Join(dir, "rules.json"), monolithic, 0644); err != nil {
		t.Fatal(err)
	}

	migrateIBANRulesToLocal(dir)

	shared, err := readRulesFile(filepath.Join(dir, "rules.json"))
	if err != nil {
		t.Fatal(err)
	}
	local, err := readRulesFile(filepath.Join(dir, "rules.local.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(local) != 3 {
		t.Fatalf("local = %d rules, want 3 (BE01, BE02, BE03)", len(local))
	}
	// Shared keeps 5 rules + 1 marker; the allowlisted and glob IBANs stay.
	if len(shared) != 6 {
		t.Fatalf("shared = %d entries, want 6", len(shared))
	}
	var markerIdx = -1
	for i, r := range shared {
		if r.Include != "" {
			markerIdx = i
		}
	}
	// Private rules sat before shared positions [1, 3, 3]; the median is 3:
	// after "*rent *" and the allowlisted NL41 rule, before the glob and the
	// catch-all — where the bulk of the IBAN block lived.
	if markerIdx != 3 {
		t.Errorf("marker at %d, want 3 (median of the extracted block)", markerIdx)
	}

	// Idempotence: a second run must change nothing.
	before, _ := os.ReadFile(filepath.Join(dir, "rules.json"))
	migrateIBANRulesToLocal(dir)
	after, _ := os.ReadFile(filepath.Join(dir, "rules.json"))
	if string(before) != string(after) {
		t.Error("second migration run modified rules.json")
	}
	local2, _ := readRulesFile(filepath.Join(dir, "rules.local.json"))
	if len(local2) != 3 {
		t.Errorf("second migration run duplicated local rules: %d", len(local2))
	}
}
