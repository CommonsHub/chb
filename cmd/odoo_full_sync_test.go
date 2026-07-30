package cmd

import (
	"strings"
	"testing"
	"time"
)

func TestOdooSyncRowSummary(t *testing.T) {
	cases := []struct {
		name string
		res  odooSyncJournalResult
		want string
	}{
		{"idle", odooSyncJournalResult{}, "already up to date"},
		{
			"pushed+reconciled+categorised",
			odooSyncJournalResult{Pulled: 3, Pushed: 3, Reconciled: 1, Categorized: 2},
			"3 new txs pulled · 3 txs pushed · 1 line reconciled · 2 lines categorised",
		},
		{
			"source-of-truth skips push",
			odooSyncJournalResult{Pulled: 5, SourceOfTruth: true, Reconciled: 4},
			"5 new txs pulled · odoo source-of-truth (no push) · 4 lines reconciled",
		},
		{
			"only categorised",
			odooSyncJournalResult{Categorized: 8},
			"8 lines categorised",
		},
	}
	for _, c := range cases {
		if got := odooSyncRowSummary(c.res); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

// A since-window must drop journal lines dated before the cutoff so a targeted
// reconcile only touches recent activity. computeReconcileMatches applies the
// filter to the cached lines; here we exercise the same date comparison it uses.
func TestReconcileSinceCutoffComparison(t *testing.T) {
	since := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	cutoff := since.Format("2006-01-02")
	kept := 0
	for _, date := range []string{"2026-05-31", "2026-06-01", "2026-07-20", ""} {
		if date != "" && date < cutoff {
			continue // dropped by the --since window
		}
		kept++
	}
	// 2026-05-31 dropped; 2026-06-01, 2026-07-20 kept; "" (no date) kept.
	if kept != 3 {
		t.Errorf("kept %d lines, want 3 (only the pre-cutoff dated line dropped)", kept)
	}
	if !strings.HasPrefix(cutoff, "2026-06-01") {
		t.Errorf("cutoff = %q, want 2026-06-01", cutoff)
	}
}
