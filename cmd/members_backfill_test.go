package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	odoosource "github.com/CommonsHub/chb/providers/odoo"
)

func TestSubscriptionCoversMonth(t *testing.T) {
	sub := func(start, end string) providerSubscription {
		return providerSubscription{CurrentPeriodStart: start, CurrentPeriodEnd: end}
	}
	for _, tc := range []struct {
		name  string
		sub   providerSubscription
		year  string
		month string
		want  bool
	}{
		{"spans the month", sub("2025-09-01", "2027-09-01"), "2026", "08", true},
		{"starts mid-month", sub("2026-08-20", "2027-08-20"), "2026", "08", true},
		{"ends mid-month", sub("2025-08-14", "2026-08-14"), "2026", "08", true},
		{"ends on the first", sub("2025-08-01", "2026-08-01"), "2026", "08", true},
		{"ended before", sub("2025-01-01", "2026-07-31"), "2026", "08", false},
		{"starts after", sub("2026-09-01", "2027-09-01"), "2026", "08", false},
		{"open ended", sub("2025-01-01", ""), "2026", "08", true},
		{"open ended but later start", sub("2026-09-05", ""), "2026", "08", false},
		{"rfc3339 timestamps", sub("2025-09-01T00:00:00Z", "2027-09-01T00:00:00Z"), "2026", "08", true},
		{"no dates at all", sub("", ""), "2026", "08", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := subscriptionCoversMonth(tc.sub, tc.year, tc.month); got != tc.want {
				t.Errorf("subscriptionCoversMonth(%+v, %s-%s) = %v, want %v",
					tc.sub, tc.year, tc.month, got, tc.want)
			}
		})
	}
}

// TestSubscriptionCoversMonthFallsBackToCreatedAt covers snapshots where the
// period start is absent — CreatedAt is the only anchor for when the member
// joined.
func TestSubscriptionCoversMonthFallsBackToCreatedAt(t *testing.T) {
	s := providerSubscription{CreatedAt: "2025-01-01", CurrentPeriodEnd: "2027-01-01"}
	if !subscriptionCoversMonth(s, "2026", "08") {
		t.Errorf("want the createdAt fallback to anchor the span")
	}
}

func TestDeriveOdooSnapshotForMonth(t *testing.T) {
	dataDir := t.TempDir()
	writeOdooSubs(t, dataDir, "2026", "09", providerSnapshot{
		Provider:  odoosource.Source,
		FetchedAt: "2026-09-01T10:00:00Z",
		Subscriptions: []providerSubscription{
			{ID: "odoo-25", CurrentPeriodStart: "2025-09-01", CurrentPeriodEnd: "2027-09-01"},
			{ID: "odoo-40", CurrentPeriodStart: "2025-09-02", CurrentPeriodEnd: "2026-09-02"},
			// Joined after August — must not appear in an August reconstruction.
			{ID: "odoo-new", CurrentPeriodStart: "2026-09-10", CurrentPeriodEnd: "2027-09-10"},
		},
	})

	got, ok := deriveOdooSnapshotForMonth(dataDir, "2026", "08")
	if !ok {
		t.Fatal("deriveOdooSnapshotForMonth() = false, want a derived snapshot")
	}
	if !got.Derived || got.DerivedFrom != "2026-09" {
		t.Errorf("derived=%v from=%q, want true and 2026-09", got.Derived, got.DerivedFrom)
	}
	if len(got.Subscriptions) != 2 {
		t.Fatalf("got %d subscriptions, want 2: %+v", len(got.Subscriptions), got.Subscriptions)
	}
	for _, s := range got.Subscriptions {
		if s.ID == "odoo-new" {
			t.Errorf("a subscription starting after the target month leaked in")
		}
	}

	t.Run("never derives forward", func(t *testing.T) {
		if _, ok := deriveOdooSnapshotForMonth(dataDir, "2026", "10"); ok {
			t.Errorf("derived a month later than the source snapshot")
		}
		if _, ok := deriveOdooSnapshotForMonth(dataDir, "2026", "09"); ok {
			t.Errorf("derived the source month itself")
		}
	})

	t.Run("empty result is not a snapshot", func(t *testing.T) {
		// Long before anyone subscribed.
		if _, ok := deriveOdooSnapshotForMonth(dataDir, "2024", "01"); ok {
			t.Errorf("returned a snapshot with no covering subscriptions")
		}
	})
}

// TestDeriveOdooSnapshotIgnoresDerivedSources stops an approximation from
// seeding another approximation, which would let one month's undercount
// silently propagate backwards.
func TestDeriveOdooSnapshotIgnoresDerivedSources(t *testing.T) {
	dataDir := t.TempDir()
	writeOdooSubs(t, dataDir, "2026", "09", providerSnapshot{
		Provider: odoosource.Source,
		Derived:  true,
		Subscriptions: []providerSubscription{
			{ID: "odoo-1", CurrentPeriodStart: "2025-01-01", CurrentPeriodEnd: "2027-01-01"},
		},
	})
	if _, ok := deriveOdooSnapshotForMonth(dataDir, "2026", "08"); ok {
		t.Errorf("used a derived snapshot as the source for another derivation")
	}
}

func writeOdooSubs(t *testing.T, dataDir, year, month string, snap providerSnapshot) {
	t.Helper()
	path := odoosource.Path(dataDir, year, month, odoosource.SubscriptionsFile)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}
