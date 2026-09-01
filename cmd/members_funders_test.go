package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func funderHash(seed string) string {
	return strings.Repeat(seed, 64/len(seed))[:64]
}

// A funder's membership is paid *until* the expiry date, so the month
// containing that date is covered in full — someone paid to the 15th is a
// member for that month, not two thirds of one.
func TestFunderCoversMonthThroughExpiry(t *testing.T) {
	f := Funder{ExpiresAt: "2026-06-15"}

	cases := []struct {
		year  int
		month time.Month
		want  bool
	}{
		{2026, time.May, true},   // before expiry
		{2026, time.June, true},  // the expiry month itself
		{2026, time.July, false}, // after
	}
	for _, c := range cases {
		got, err := funderCoversMonth(f, c.year, c.month)
		if err != nil {
			t.Fatalf("%s %d: %v", c.month, c.year, err)
		}
		if got != c.want {
			t.Errorf("%s %d covered = %v, want %v", c.month, c.year, got, c.want)
		}
	}
}

// The last day of the term is inclusive, right down to the boundary.
func TestFunderCoversMonthAtTheExactBoundary(t *testing.T) {
	f := Funder{ExpiresAt: "2026-06-30"}
	if covered, _ := funderCoversMonth(f, 2026, time.June); !covered {
		t.Error("expiry on the last day of the month should cover that month")
	}
	if covered, _ := funderCoversMonth(f, 2026, time.July); covered {
		t.Error("July must not be covered by a term ending 2026-06-30")
	}

	f = Funder{ExpiresAt: "2026-07-01"}
	if covered, _ := funderCoversMonth(f, 2026, time.July); !covered {
		t.Error("a term ending on the first of the month covers that month")
	}
}

func TestFunderStartDateExcludesEarlierMonths(t *testing.T) {
	f := Funder{StartsAt: "2026-03-10", ExpiresAt: "2026-12-31"}
	if covered, _ := funderCoversMonth(f, 2026, time.February); covered {
		t.Error("February precedes the term and must not be covered")
	}
	// The term starts mid-March, so March is covered: the month has not ended.
	if covered, _ := funderCoversMonth(f, 2026, time.March); !covered {
		t.Error("the starting month should be covered")
	}
}

func TestFunderCoversMonthRejectsBadDates(t *testing.T) {
	if _, err := funderCoversMonth(Funder{ExpiresAt: ""}, 2026, time.June); err == nil {
		t.Error("a funder with no expiry date must be an error, not an open-ended membership")
	}
	if _, err := funderCoversMonth(Funder{ExpiresAt: "31/12/2026"}, 2026, time.June); err == nil {
		t.Error("a non-ISO date must be rejected")
	}
	if _, err := funderCoversMonth(Funder{ExpiresAt: "2026-12-31", StartsAt: "nope"}, 2026, time.June); err == nil {
		t.Error("a malformed start date must be rejected")
	}
}

func TestFunderMemberIDPrefersTheHash(t *testing.T) {
	hash := funderHash("a")
	// Both present: the hash wins, and no hashing of the address happens.
	got := funderMemberID(Funder{EmailHash: hash, Email: "someone@example.org"}, "salt")
	if got != hash {
		t.Fatalf("memberID = %q, want the configured hash", got)
	}
	// An address alone is hashed with the salt.
	if got := funderMemberID(Funder{Email: "ada@example.org"}, "salt"); got != hashEmail("ada@example.org", "salt") {
		t.Fatalf("memberID = %q, want the salted hash of the address", got)
	}
	// An address with no salt identifies nobody rather than guessing.
	if got := funderMemberID(Funder{Email: "ada@example.org"}, ""); got != "" {
		t.Fatalf("memberID = %q, want empty without a salt", got)
	}
	// A malformed hash is refused rather than used as a filename.
	for _, bad := range []string{"not-a-hash", strings.Repeat("z", 64), "../../etc/passwd"} {
		if got := funderMemberID(Funder{EmailHash: bad}, "salt"); got != "" {
			t.Errorf("memberID(%q) = %q, want empty", bad, got)
		}
	}
}

func TestBuildFundersSnapshotOnlyIncludesCoveredMonths(t *testing.T) {
	ada, grace := funderHash("a"), funderHash("b")
	funders := []Funder{
		{EmailHash: ada, FirstName: "Ada", ExpiresAt: "2026-06-30",
			Amount: MemberAmount{Value: 120, Decimals: 2, Currency: "EUR"}, Interval: "year"},
		{EmailHash: grace, FirstName: "Grace", StartsAt: "2026-07-01", ExpiresAt: "2026-12-31"},
	}

	june := buildFundersSnapshot(funders, 2026, time.June, "salt")
	if len(june.Subscriptions) != 1 || june.Subscriptions[0].EmailHash != ada {
		t.Fatalf("June = %+v, want Ada alone", june.Subscriptions)
	}
	if june.Provider != "funders" {
		t.Fatalf("provider = %q, want funders", june.Provider)
	}
	sub := june.Subscriptions[0]
	if sub.Source != "funders" || sub.Status != "active" || sub.Plan != "yearly" {
		t.Fatalf("subscription = %+v", sub)
	}
	if sub.CurrentPeriodEnd != "2026-06-30" {
		t.Fatalf("period end = %q, want the expiry date", sub.CurrentPeriodEnd)
	}

	july := buildFundersSnapshot(funders, 2026, time.July, "salt")
	if len(july.Subscriptions) != 1 || july.Subscriptions[0].EmailHash != grace {
		t.Fatalf("July = %+v, want Grace alone", july.Subscriptions)
	}
}

// One malformed entry must not cost the operator the rest of the month's
// membership.
func TestBuildFundersSnapshotSkipsBadEntriesAndKeepsGoing(t *testing.T) {
	good := funderHash("a")
	funders := []Funder{
		{EmailHash: "not-a-hash", ExpiresAt: "2026-12-31"},
		{EmailHash: funderHash("c"), ExpiresAt: "nonsense"},
		{EmailHash: good, FirstName: "Ada", ExpiresAt: "2026-12-31"},
	}

	snap := buildFundersSnapshot(funders, 2026, time.June, "salt")
	if len(snap.Subscriptions) != 1 || snap.Subscriptions[0].EmailHash != good {
		t.Fatalf("snapshot = %+v, want only the valid entry", snap.Subscriptions)
	}
}

func TestLoadFundersMissingFileIsNotAnError(t *testing.T) {
	t.Setenv("APP_DATA_DIR", t.TempDir())
	funders, err := loadFunders()
	if err != nil {
		t.Fatalf("a missing funders.json must not be an error: %v", err)
	}
	if len(funders) != 0 {
		t.Fatalf("got %d funders from nothing", len(funders))
	}
}

func TestLoadFundersReadsTheFile(t *testing.T) {
	appDir := t.TempDir()
	t.Setenv("APP_DATA_DIR", appDir)
	dir := filepath.Join(appDir, "settings")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(fundersFile{Funders: []Funder{
		{EmailHash: funderHash("a"), FirstName: "Ada", ExpiresAt: "2027-01-31", Note: "bank transfer"},
	}})
	if err := os.WriteFile(filepath.Join(dir, fundersFileName), body, 0644); err != nil {
		t.Fatal(err)
	}

	funders, err := loadFunders()
	if err != nil {
		t.Fatal(err)
	}
	if len(funders) != 1 || funders[0].FirstName != "Ada" || funders[0].ExpiresAt != "2027-01-31" {
		t.Fatalf("funders = %+v", funders)
	}
}

// Stripe and Odoo are the systems of record: someone who appears in either
// keeps that entry rather than being relabelled a funder.
func TestFundersDoNotOverrideStripeOrOdoo(t *testing.T) {
	id := funderHash("a")
	members := mergeProviderSnapshots([]providerSnapshot{
		{Provider: "stripe", Subscriptions: []providerSubscription{
			{ID: "sub_1", Source: "stripe", EmailHash: id, Status: "active"},
		}},
		{Provider: "funders", Subscriptions: []providerSubscription{
			{ID: "funder:x", Source: "funders", EmailHash: id, Status: "active"},
		}},
	})
	if len(members) != 1 {
		t.Fatalf("got %d members, want 1", len(members))
	}
	if members[0].Source != "stripe" {
		t.Fatalf("source = %q, want stripe to win", members[0].Source)
	}
}
