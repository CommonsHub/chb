package cmd

import "testing"

func mustBalance(t *testing.T, b MonthlyReportBalance) (opening, ending float64) {
	t.Helper()
	if b.Opening == nil || b.Ending == nil {
		t.Fatalf("balance not filled: %+v", b)
	}
	return *b.Opening, *b.Ending
}

// TestReconcileAccountBalancesRollsBackFromLiveAnchor is the core contract: a
// transaction history yields deltas, and only a live balance can turn those
// deltas into balances. Rolling the anchor backwards must land each month
// boundary exactly.
func TestReconcileAccountBalancesRollsBackFromLiveAnchor(t *testing.T) {
	key := reconciledAccountKey{source: "etherscan", chain: "gnosis", identity: "savings", currency: "EURE"}
	deltas := map[reconciledAccountKey][]monthlyAccountDelta{
		key: {
			{ym: "2026-06", net: 100},
			{ym: "2026-07", net: -30},
			{ym: "2026-08", net: 250},
		},
	}
	// Live balance today is 1000: June opened at 680.
	balances, recon := reconcileAccountBalances(deltas, map[reconciledAccountKey]float64{key: 1000})

	months := balances[key]
	for _, tc := range []struct {
		ym              string
		opening, ending float64
	}{
		{"2026-06", 680, 780},
		{"2026-07", 780, 750},
		{"2026-08", 750, 1000},
	} {
		o, e := mustBalance(t, months[tc.ym])
		if o != tc.opening || e != tc.ending {
			t.Errorf("%s = opening %.2f ending %.2f, want %.2f / %.2f", tc.ym, o, e, tc.opening, tc.ending)
		}
		if !months[tc.ym].Computed {
			t.Errorf("%s should be marked computed", tc.ym)
		}
	}

	// Only the newest month can claim verification — the live balance vouches
	// for today, every earlier boundary is inferred from it.
	if !months["2026-08"].Verified {
		t.Error("newest month should be verified against the live anchor")
	}
	if months["2026-07"].Verified {
		t.Error("an inferred earlier boundary must not claim verification")
	}

	r := recon[key]
	if !r.HasAnchor || r.BookedTotal != 320 {
		t.Errorf("recon = %+v, want anchored with booked 320", r)
	}
	// 1000 live against 320 booked: 680 of the balance predates our history.
	// That is an opening balance, not an error.
	if r.PreHistory != 680 {
		t.Errorf("PreHistory = %.2f, want 680", r.PreHistory)
	}
}

// TestReconcileAccountBalancesWithoutAnchor covers the honest fallback: the
// shape of the balance is right, its level is not, and it is never "verified".
func TestReconcileAccountBalancesWithoutAnchor(t *testing.T) {
	key := reconciledAccountKey{source: "stripe", identity: "acct_x", currency: "EUR"}
	deltas := map[reconciledAccountKey][]monthlyAccountDelta{
		key: {{ym: "2026-07", net: 100}, {ym: "2026-08", net: 50}},
	}
	balances, recon := reconcileAccountBalances(deltas, nil)

	o, e := mustBalance(t, balances[key]["2026-07"])
	if o != 0 || e != 100 {
		t.Errorf("first month = %.2f / %.2f, want 0 / 100", o, e)
	}
	if _, e := mustBalance(t, balances[key]["2026-08"]); e != 150 {
		t.Errorf("second month ending = %.2f, want 150", e)
	}
	for _, ym := range []string{"2026-07", "2026-08"} {
		if balances[key][ym].Verified {
			t.Errorf("%s claimed verification with no anchor", ym)
		}
		if !balances[key][ym].Computed {
			t.Errorf("%s should still be marked computed", ym)
		}
	}
	if recon[key].HasAnchor {
		t.Error("HasAnchor should be false when nothing anchored the series")
	}
}

// TestReconcileOutOfOrderMonths guards the sort: months arrive from a directory
// walk, and rolling backwards through an unsorted series would scramble every
// boundary.
func TestReconcileOutOfOrderMonths(t *testing.T) {
	key := reconciledAccountKey{identity: "kbc", currency: "EUR"}
	deltas := map[reconciledAccountKey][]monthlyAccountDelta{
		key: {{ym: "2026-08", net: 50}, {ym: "2026-06", net: 100}, {ym: "2026-07", net: -30}},
	}
	balances, _ := reconcileAccountBalances(deltas, map[reconciledAccountKey]float64{key: 1000})
	// Aug ends at the anchor 1000; Jul ends at 1000-50=950; Jun at 950-(-30)=980.
	if _, e := mustBalance(t, balances[key]["2026-06"]); e != 980 {
		t.Errorf("June ending = %.2f, want 980", e)
	}
}

// TestReconciliationNote pins the three things the note has to be able to say.
func TestReconciliationNote(t *testing.T) {
	acct := MonthlyReportAccount{Source: "etherscan", Chain: "gnosis", AccountSlug: "savings", Currency: "EURE"}
	key := accountRowKey(acct)

	t.Run("nothing anchored", func(t *testing.T) {
		note := reconciliationNote([]MonthlyReportAccount{acct}, map[reconciledAccountKey]accountReconciliation{})
		if !contains(note, "no live balance") {
			t.Errorf("note = %q", note)
		}
	})
	t.Run("all clean", func(t *testing.T) {
		note := reconciliationNote([]MonthlyReportAccount{acct}, map[reconciledAccountKey]accountReconciliation{
			key: {HasAnchor: true, Anchor: 100, BookedTotal: 100},
		})
		if !contains(note, "fully account") {
			t.Errorf("note = %q", note)
		}
	})
	// The residual is stated without claiming which of the two causes it is:
	// nothing in the data distinguishes an opening balance from a gap.
	t.Run("a residual is stated without diagnosing it", func(t *testing.T) {
		note := reconciliationNote([]MonthlyReportAccount{acct}, map[reconciledAccountKey]accountReconciliation{
			key: {HasAnchor: true, Anchor: 1000, BookedTotal: 320, PreHistory: 680},
		})
		if !contains(note, "680") {
			t.Errorf("note should state the residual, got %q", note)
		}
		if !contains(note, "opening balance") || !contains(note, "missing") {
			t.Errorf("note should offer both readings, got %q", note)
		}
	})
}
