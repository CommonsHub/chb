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
