package cmd

import (
	"encoding/json"
	"testing"
	"time"
)

// loadDefaultAccountConfigsForTest reads the accounts shipped in the binary,
// independent of whatever the developer has in their own settings dir.
func loadDefaultAccountConfigsForTest(t *testing.T) []AccountConfig {
	t.Helper()
	data, err := readDefaultSettingsFile("accounts.json")
	if err != nil {
		t.Fatalf("read shipped accounts.json: %v", err)
	}
	var accounts []AccountConfig
	if err := json.Unmarshal(data, &accounts); err != nil {
		t.Fatalf("parse shipped accounts.json: %v", err)
	}
	return accounts
}

func floatOf(v float64) *float64 { return &v }

func TestOpeningBalanceAtAppliesOnlyFromItsDate(t *testing.T) {
	acc := &AccountConfig{
		Slug:               "stripe",
		OpeningBalance:     floatOf(1476.92),
		OpeningBalanceAsOf: "2026-07-01",
	}

	// Before the opening date chb has no basis to claim a balance, so the
	// figure must not be applied.
	if _, ok := acc.OpeningBalanceAt(time.Date(2026, 6, 30, 23, 59, 59, 0, BrusselsTZ())); ok {
		t.Fatal("opening balance applied to a cutoff before its date")
	}
	// On the date itself, and after, it seeds the running total.
	for _, cutoff := range []time.Time{
		time.Date(2026, 7, 1, 0, 0, 0, 0, BrusselsTZ()),
		time.Date(2026, 9, 1, 23, 59, 59, 0, BrusselsTZ()),
	} {
		v, ok := acc.OpeningBalanceAt(cutoff)
		if !ok {
			t.Fatalf("opening balance not applied at %s", cutoff.Format("2006-01-02"))
		}
		if v != 1476.92 {
			t.Fatalf("opening balance = %v, want 1476.92", v)
		}
	}
}

// An undated opening balance applies to every cutoff — the account is simply
// known to have started at that figure.
func TestOpeningBalanceWithoutDateAlwaysApplies(t *testing.T) {
	acc := &AccountConfig{Slug: "x", OpeningBalance: floatOf(500)}
	v, ok := acc.OpeningBalanceAt(time.Date(2000, 1, 1, 0, 0, 0, 0, BrusselsTZ()))
	if !ok || v != 500 {
		t.Fatalf("OpeningBalanceAt = (%v, %v), want (500, true)", v, ok)
	}
}

// An explicit zero means "verified empty", which is a different claim from an
// absent field — the pointer keeps them apart.
func TestOpeningBalanceZeroIsDistinctFromUnset(t *testing.T) {
	zero := &AccountConfig{Slug: "z", OpeningBalance: floatOf(0)}
	if _, ok := zero.OpeningBalanceAt(time.Now()); !ok {
		t.Fatal("an explicit zero opening balance should apply")
	}
	unset := &AccountConfig{Slug: "u"}
	if _, ok := unset.OpeningBalanceAt(time.Now()); ok {
		t.Fatal("an unset opening balance must not apply")
	}
	if _, ok := (*AccountConfig)(nil).OpeningBalanceAt(time.Now()); ok {
		t.Fatal("a nil account must not report an opening balance")
	}
}

// A malformed date is ignored rather than silently suppressing the balance.
func TestOpeningBalanceMalformedDateStillApplies(t *testing.T) {
	acc := &AccountConfig{Slug: "m", OpeningBalance: floatOf(10), OpeningBalanceAsOf: "not-a-date"}
	if _, ok := acc.OpeningBalanceAsOfTime(); ok {
		t.Fatal("malformed date should not parse")
	}
	if _, ok := acc.OpeningBalanceAt(time.Now()); !ok {
		t.Fatal("opening balance should still apply when the date is unusable")
	}
}

// The shipped Stripe account carries the opening balance derived from its live
// balance; without it the account reads as deeply negative.
func TestStripeDefaultCarriesOpeningBalance(t *testing.T) {
	var stripe *AccountConfig
	for _, a := range loadDefaultAccountConfigsForTest(t) {
		if a.Slug == "stripe" {
			acc := a
			stripe = &acc
			break
		}
	}
	if stripe == nil {
		t.Skip("no stripe account in the shipped defaults")
	}
	if stripe.OpeningBalance == nil {
		t.Fatal("stripe has no openingBalance in the shipped defaults")
	}
	if stripe.OpeningBalanceAsOf == "" {
		t.Fatal("stripe openingBalance carries no date")
	}
	if _, ok := stripe.OpeningBalanceAsOfTime(); !ok {
		t.Fatalf("stripe openingBalanceAsOf %q does not parse", stripe.OpeningBalanceAsOf)
	}
}

// The Odoo starting-balance plan compares the journal's manual opening entry
// against what chb computes for the period before the cutoff. When the local
// archive does not reach back that far — chb's Stripe history starts in 2025,
// the account is older — that computation is only correct if it counts the
// configured opening balance. Without it the plan reads 0.00, decides the
// journal's correct opening entry is wrong, and plans to rewrite it to zero.
func TestStartingBalancePlanKeepsOpeningEntryWhenArchiveStartsAtCutoff(t *testing.T) {
	cutoff := time.Date(2025, 1, 1, 0, 0, 0, 0, BrusselsTZ())
	acc := &AccountConfig{
		Slug:               "stripe",
		Provider:           "stripe",
		Currency:           "EUR",
		OdooJournalID:      48,
		OdooSyncSince:      "2025-01-01",
		OpeningBalance:     floatOf(9326.90),
		OpeningBalanceAsOf: "2025-01-01",
	}
	// The journal's manual opening entry: no uniqueImportId, so chb does not
	// own it. Nothing else is dated before the cutoff.
	lines := []OdooCacheLine{
		{ID: 31159, Date: "2025-01-01", PaymentRef: "Solde de départ 2025-01-01", Amount: 9326.90},
		{ID: 31160, Date: "2025-01-02", UniqueImportID: "stripe-txn_1", Amount: 12.34},
	}

	plan := planStartingBalanceConvergence(acc, cutoff, lines)

	if plan.ExpectedOpening != 9326.90 {
		t.Fatalf("ExpectedOpening = %.2f, want 9326.90 — the configured opening balance was not counted", plan.ExpectedOpening)
	}
	if plan.OpeningAction != "ok" {
		t.Fatalf("OpeningAction = %q, want \"ok\" — the plan would rewrite a correct opening entry", plan.OpeningAction)
	}
	if plan.hasChanges() {
		t.Fatalf("plan reports changes against an already-correct journal: %+v", plan.DeleteLines)
	}
}

// An account with no configured opening balance keeps the old behaviour: the
// balance before the cutoff is whatever the archive holds.
func TestStartingBalancePlanUnaffectedWithoutOpeningBalance(t *testing.T) {
	cutoff := time.Date(2025, 1, 1, 0, 0, 0, 0, BrusselsTZ())
	acc := &AccountConfig{Slug: "x", Provider: "stripe", OdooSyncSince: "2025-01-01"}
	if got := accountLocalBalanceBefore(acc, cutoff); got != 0 {
		t.Fatalf("accountLocalBalanceBefore = %.2f, want 0 for an account with no opening balance", got)
	}
}

// The local snapshot is compared against a journal balance that includes the
// journal's own opening entry, so it has to start from the same position.
func TestLocalOdooSnapshotIncludesOpeningBalance(t *testing.T) {
	acc := &AccountConfig{
		Slug:               "stripe",
		Currency:           "EUR",
		OpeningBalance:     floatOf(9326.90),
		OpeningBalanceAsOf: "2025-01-01",
	}
	snap := accountLocalOdooSnapshot(acc, nil)
	if snap.Balance != 9326.90 {
		t.Fatalf("snapshot balance = %.2f, want 9326.90 with no transactions", snap.Balance)
	}
	if snap.TxCount != 0 {
		t.Fatalf("TxCount = %d, want 0 — the opening balance is not a transaction", snap.TxCount)
	}
}
