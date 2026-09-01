package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func hashID(seed string) string {
	return strings.Repeat(seed, 64/len(seed))[:64]
}

func writeMembersFixture(t *testing.T, dataDir, year, month string, f MembersOutputFile) {
	t.Helper()
	f.Year, f.Month = year, month
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dataDir, year, month, "generated", "members.json")
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}
}

func memberFixture(id, name, source, status string) Member {
	return Member{
		ID:        "sub_" + id[:6],
		Source:    source,
		Accounts:  MemberAccounts{EmailHash: id},
		FirstName: name,
		Plan:      "monthly",
		Amount:    MemberAmount{Value: 10, Decimals: 2, Currency: "EUR"},
		Interval:  "month",
		Status:    status,
		CreatedAt: "2025-11-02",
	}
}

func readHistory(t *testing.T, dataDir, id string) MemberHistoryFile {
	t.Helper()
	path, ok := MemberHistoryPath(dataDir, id)
	if !ok {
		t.Fatalf("MemberHistoryPath rejected %q", id)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read history for %s: %v", id[:8], err)
	}
	var h MemberHistoryFile
	if err := json.Unmarshal(data, &h); err != nil {
		t.Fatal(err)
	}
	return h
}

func TestGenerateMemberHistoriesBuildsOneFilePerMember(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("DATA_DIR", dataDir)
	ada, grace := hashID("a"), hashID("b")

	writeMembersFixture(t, dataDir, "2026", "01", MembersOutputFile{
		Members: []Member{memberFixture(ada, "Ada", "stripe", "active")},
	})
	writeMembersFixture(t, dataDir, "2026", "02", MembersOutputFile{
		Members: []Member{
			memberFixture(ada, "Ada", "stripe", "active"),
			memberFixture(grace, "Grace", "odoo", "active"),
		},
	})

	if got := generateMemberHistories(dataDir); got == "" {
		t.Fatal("generateMemberHistories reported nothing written")
	}

	adaHistory := readHistory(t, dataDir, ada)
	if adaHistory.MonthsActive != 2 {
		t.Fatalf("Ada monthsActive = %d, want 2", adaHistory.MonthsActive)
	}
	if adaHistory.FirstMonth != "2026-01" || adaHistory.LastMonth != "2026-02" {
		t.Fatalf("Ada span = %s..%s, want 2026-01..2026-02", adaHistory.FirstMonth, adaHistory.LastMonth)
	}
	if adaHistory.MemberID != ada {
		t.Fatalf("memberId = %q, want the emailHash", adaHistory.MemberID)
	}
	// Months must be chronological regardless of directory walk order.
	if adaHistory.Months[0].Month != "2026-01" || adaHistory.Months[1].Month != "2026-02" {
		t.Fatalf("months out of order: %+v", adaHistory.Months)
	}

	graceHistory := readHistory(t, dataDir, grace)
	if graceHistory.MonthsActive != 1 || graceHistory.FirstMonth != "2026-02" {
		t.Fatalf("Grace history = %+v, want a single 2026-02 month", graceHistory)
	}
}

// Months before the window are excluded — they are Odoo reconstructions that
// can undercount, and a missing month in a history reads as "not a member".
func TestGenerateMemberHistoriesSkipsMonthsBeforeTheWindow(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("DATA_DIR", dataDir)
	ada := hashID("a")

	writeMembersFixture(t, dataDir, "2025", "11", MembersOutputFile{
		Members: []Member{memberFixture(ada, "Ada", "odoo", "active")},
	})
	writeMembersFixture(t, dataDir, "2026", "01", MembersOutputFile{
		Members: []Member{memberFixture(ada, "Ada", "stripe", "active")},
	})

	generateMemberHistories(dataDir)

	h := readHistory(t, dataDir, ada)
	if h.MonthsActive != 1 || h.Months[0].Month != "2026-01" {
		t.Fatalf("history = %+v, want only 2026-01", h.Months)
	}
}

// A reconstructed Odoo month is marked per entry so a reader can weigh it.
// Stripe entries in the same file are as-observed and must not be marked.
func TestGenerateMemberHistoriesMarksDerivedOdooMonths(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("DATA_DIR", dataDir)
	ada, grace := hashID("a"), hashID("b")

	writeMembersFixture(t, dataDir, "2026", "01", MembersOutputFile{
		OdooDerived:     true,
		OdooDerivedFrom: "2026-09",
		Members: []Member{
			memberFixture(ada, "Ada", "odoo", "active"),
			memberFixture(grace, "Grace", "stripe", "active"),
		},
	})

	generateMemberHistories(dataDir)

	odooMonth := readHistory(t, dataDir, ada).Months[0]
	if !odooMonth.Derived || odooMonth.DerivedFrom != "2026-09" {
		t.Fatalf("odoo month not marked derived: %+v", odooMonth)
	}
	stripeMonth := readHistory(t, dataDir, grace).Months[0]
	if stripeMonth.Derived {
		t.Fatal("a stripe month in a derived file must not be marked derived")
	}
}

// A member who leaves the window entirely must not keep a stale file: the
// snapshots are the only record of who existed when.
func TestGenerateMemberHistoriesRemovesStaleFiles(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("DATA_DIR", dataDir)
	ada, gone := hashID("a"), hashID("c")

	writeMembersFixture(t, dataDir, "2026", "01", MembersOutputFile{
		Members: []Member{memberFixture(ada, "Ada", "stripe", "active"), memberFixture(gone, "Gone", "stripe", "canceled")},
	})
	generateMemberHistories(dataDir)
	if _, err := os.Stat(mustPath(t, dataDir, gone)); err != nil {
		t.Fatalf("expected a file for the departing member first: %v", err)
	}

	// Rewrite the month without them, then regenerate.
	writeMembersFixture(t, dataDir, "2026", "01", MembersOutputFile{
		Members: []Member{memberFixture(ada, "Ada", "stripe", "active")},
	})
	generateMemberHistories(dataDir)

	if _, err := os.Stat(mustPath(t, dataDir, gone)); !os.IsNotExist(err) {
		t.Fatal("stale member file survived a regenerate")
	}
	if _, err := os.Stat(mustPath(t, dataDir, ada)); err != nil {
		t.Fatalf("remaining member lost their file: %v", err)
	}
}

func mustPath(t *testing.T, dataDir, id string) string {
	t.Helper()
	p, ok := MemberHistoryPath(dataDir, id)
	if !ok {
		t.Fatalf("MemberHistoryPath rejected %q", id)
	}
	return p
}

// The membership id becomes a filename, so anything that is not a plain hash
// is refused rather than resolved.
func TestMemberHistoryPathRejectsNonHashIDs(t *testing.T) {
	dataDir := t.TempDir()
	for _, bad := range []string{
		"", "not-a-hash",
		"../../../etc/passwd",
		strings.Repeat("a", 63),
		strings.Repeat("a", 65),
		strings.Repeat("A", 64), // uppercase is normalised, but a mixed-case non-hex is not
		strings.Repeat("z", 64),
		hashID("a") + "/../x",
	} {
		if _, ok := MemberHistoryPath(dataDir, bad); ok && bad != strings.Repeat("A", 64) {
			t.Errorf("MemberHistoryPath accepted %q", bad)
		}
	}
	// A well-formed hash resolves inside the members directory.
	p, ok := MemberHistoryPath(dataDir, hashID("a"))
	if !ok {
		t.Fatal("a valid hash was rejected")
	}
	if filepath.Dir(p) != memberHistoryDir(dataDir) {
		t.Fatalf("path %q escaped the members directory", p)
	}
}

// A member with no usable id cannot be filed anywhere, and must not be
// attached to somebody else's history.
func TestGenerateMemberHistoriesSkipsMembersWithoutAnID(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("DATA_DIR", dataDir)
	writeMembersFixture(t, dataDir, "2026", "01", MembersOutputFile{
		Members: []Member{memberFixture(hashID("a"), "Ada", "stripe", "active"), {FirstName: "Nameless"}},
	})

	generateMemberHistories(dataDir)

	entries, err := os.ReadDir(memberHistoryDir(dataDir))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("wrote %d files, want 1 — the member without an id should be skipped", len(entries))
	}
}

// A salt change re-hashes every member into a new identity, silently splitting
// each person's history in two. Adjacent months of a stable membership always
// overlap, so no overlap at all is the signal.
func TestWarnOnIdentityDiscontinuity(t *testing.T) {
	ids := func(prefix string, n int) map[string]bool {
		out := map[string]bool{}
		for i := 0; i < n; i++ {
			out[prefix+string(rune('a'+i))] = true
		}
		return out
	}
	months := []string{"2026-01", "2026-02", "2026-03"}

	flagged := warnOnIdentityDiscontinuity(months, map[string]map[string]bool{
		"2026-01": ids("x", 8),
		"2026-02": ids("y", 8), // fully disjoint → the salt changed
		"2026-03": ids("y", 8),
	})
	if len(flagged) != 1 || flagged[0] != "2026-02" {
		t.Fatalf("flagged = %v, want [2026-02]", flagged)
	}

	// Overlapping months are silent, and a month too small to judge is not
	// reported on: a handful of members can legitimately all churn at once.
	flagged = warnOnIdentityDiscontinuity(months, map[string]map[string]bool{
		"2026-01": ids("x", 8),
		"2026-02": ids("x", 8),
		"2026-03": ids("q", 2),
	})
	if len(flagged) != 0 {
		t.Fatalf("flagged = %v, want none", flagged)
	}
}
