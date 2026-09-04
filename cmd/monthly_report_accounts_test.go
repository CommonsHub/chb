package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestBuildMonthlyReportAccountsOnChain covers the two ways an on-chain
// account used to vanish from the 🏦 Accounts section:
//
//  1. rows were skipped when TransactionEntry.Account was empty — but that
//     field is internal-only and always empty after a round-trip through
//     generated/transactions.json, so every etherscan row was dropped;
//  2. MINT/BURN fell through to `default` in the direction switch, so even a
//     row that survived contributed nothing to in/out.
func TestBuildMonthlyReportAccountsOnChain(t *testing.T) {
	dataDir := t.TempDir()
	dir := filepath.Join(dataDir, "2026", "08", "generated")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}

	gnosis := "gnosis"
	file := TransactionsFile{
		Year:  "2026",
		Month: "08",
		Transactions: []TransactionEntry{
			{ID: "a", Provider: "etherscan", Chain: &gnosis, AccountSlug: "savings",
				AccountName: "Gnosis EURe", Currency: "EURe", Type: "MINT", GrossAmount: 363.00},
			{ID: "b", Provider: "etherscan", Chain: &gnosis, AccountSlug: "savings",
				AccountName: "Gnosis EURe", Currency: "EURe", Type: "MINT", GrossAmount: 181.50},
			{ID: "c", Provider: "etherscan", Chain: &gnosis, AccountSlug: "checking",
				AccountName: "Gnosis EURe", Currency: "EURe", Type: "BURN", GrossAmount: -302.50},
			{ID: "d", Provider: "stripe", AccountID: "acct_x", Currency: "EUR",
				Type: "CREDIT", GrossAmount: 10.00},
			// No account handle at all — not attributable, must be skipped.
			{ID: "e", Provider: "etherscan", Chain: &gnosis, Currency: "EURe",
				Type: "MINT", GrossAmount: 999.99},
		},
	}
	data, err := json.Marshal(file)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "transactions.json"), data, 0644); err != nil {
		t.Fatal(err)
	}

	accounts := buildMonthlyReportAccounts(dataDir, "2026", "08")

	byKey := map[string]MonthlyReportAccount{}
	for _, a := range accounts {
		key := a.AccountSlug
		if key == "" {
			key = a.Account
		}
		byKey[key] = a
	}
	if len(accounts) != 3 {
		t.Fatalf("got %d accounts, want 3: %+v", len(accounts), accounts)
	}

	savings, ok := byKey["savings"]
	if !ok {
		t.Fatalf("savings account missing; got %+v", byKey)
	}
	if savings.Amounts.In != 544.50 || savings.Amounts.Out != 0 {
		t.Errorf("savings amounts = in %.2f out %.2f, want in 544.50 out 0",
			savings.Amounts.In, savings.Amounts.Out)
	}
	if savings.Counts.Mints != 2 || savings.Counts.Credits != 2 {
		t.Errorf("savings counts = mints %d credits %d, want 2 and 2",
			savings.Counts.Mints, savings.Counts.Credits)
	}

	checking, ok := byKey["checking"]
	if !ok {
		t.Fatalf("checking account missing; got %+v", byKey)
	}
	if checking.Amounts.Out != 302.50 || checking.Counts.Burns != 1 {
		t.Errorf("checking = out %.2f burns %d, want 302.50 and 1",
			checking.Amounts.Out, checking.Counts.Burns)
	}

	if _, ok := byKey["acct_x"]; !ok {
		t.Errorf("stripe account missing; got %+v", byKey)
	}
}

// TestBuildMonthlyReportAccountsGroupsBySlug guards against splitting a
// community token into one row per holder. CHT transfers all carry
// accountSlug "cht" but a distinct accountId per member wallet, which produced
// 25 identical "cht" lines in the May 2026 report.
func TestBuildMonthlyReportAccountsGroupsBySlug(t *testing.T) {
	dataDir := t.TempDir()
	dir := filepath.Join(dataDir, "2026", "05", "generated")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	celo := "celo"
	file := TransactionsFile{Year: "2026", Month: "05", Transactions: []TransactionEntry{
		{ID: "a", Provider: "etherscan", Chain: &celo, AccountSlug: "cht", AccountID: "wallet-1",
			Currency: "CHT", Type: "CREDIT", GrossAmount: 3},
		{ID: "b", Provider: "etherscan", Chain: &celo, AccountSlug: "cht", AccountID: "wallet-2",
			Currency: "CHT", Type: "CREDIT", GrossAmount: 9},
		{ID: "c", Provider: "etherscan", Chain: &celo, AccountSlug: "cht", AccountID: "wallet-3",
			Currency: "CHT", Type: "DEBIT", GrossAmount: -4},
	}}
	data, err := json.Marshal(file)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "transactions.json"), data, 0644); err != nil {
		t.Fatal(err)
	}

	accounts := buildMonthlyReportAccounts(dataDir, "2026", "05")
	if len(accounts) != 1 {
		t.Fatalf("got %d rows, want 1 aggregated cht row: %+v", len(accounts), accounts)
	}
	if accounts[0].Amounts.In != 12 || accounts[0].Amounts.Out != 4 {
		t.Errorf("cht = in %.2f out %.2f, want in 12 out 4",
			accounts[0].Amounts.In, accounts[0].Amounts.Out)
	}
}

// TestBuildMonthlyReportCurrenciesOnChain guards the same MINT/BURN omission
// in the currency roll-up: EURe/EURb aggregate into the single EUR row, so
// dropping mints and burns silently under-reported every on-chain euro.
func TestBuildMonthlyReportCurrenciesOnChain(t *testing.T) {
	dataDir := t.TempDir()
	dir := filepath.Join(dataDir, "2026", "08", "generated")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}

	file := TransactionsFile{
		Year:  "2026",
		Month: "08",
		Transactions: []TransactionEntry{
			{ID: "a", Provider: "etherscan", AccountSlug: "savings", Currency: "EURe",
				Type: "MINT", GrossAmount: 363.00},
			{ID: "b", Provider: "etherscan", AccountSlug: "checking", Currency: "EURe",
				Type: "BURN", GrossAmount: -302.50},
			{ID: "c", Provider: "stripe", AccountSlug: "acct_x", Currency: "EUR",
				Type: "CREDIT", GrossAmount: 10.00},
		},
	}
	data, err := json.Marshal(file)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "transactions.json"), data, 0644); err != nil {
		t.Fatal(err)
	}

	byCurrency := map[string]MonthlyReportCurrency{}
	for _, c := range buildMonthlyReportCurrencies(dataDir, "2026", "08") {
		byCurrency[c.Currency] = c
	}

	eure, ok := byCurrency["EURE"]
	if !ok {
		t.Fatalf("EURE row missing; got %+v", byCurrency)
	}
	if eure.In != 363.00 || eure.Out != 302.50 {
		t.Errorf("EURE = in %.2f out %.2f, want in 363.00 out 302.50", eure.In, eure.Out)
	}
	if eur := byCurrency["EUR"]; eur.In != 10.00 {
		t.Errorf("EUR in = %.2f, want 10.00", eur.In)
	}
}

// TestBuildMonthlyReportTaggedFlowsSkipsInternal pins the rule that an
// account-to-account transfer is neither income nor expense. txDelta signs
// anything that isn't IsOutgoing() as an inflow, so an INTERNAL row used to
// land in `in` — and a Stripe payout, which appears once on each side of the
// move, was booked as revenue twice over.
func TestBuildMonthlyReportTaggedFlowsSkipsInternal(t *testing.T) {
	dataDir := t.TempDir()
	dir := filepath.Join(dataDir, "2026", "08", "generated")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}

	file := TransactionsFile{
		Year:  "2026",
		Month: "08",
		Transactions: []TransactionEntry{
			{ID: "a", Provider: "stripe", AccountSlug: "acct_x", Currency: "EUR",
				Type: "CREDIT", GrossAmount: 100.00, Collective: "commonshub"},
			// Both legs of one payout between our own accounts.
			{ID: "b", Provider: "stripe", AccountSlug: "acct_x", Currency: "EUR",
				Type: "INTERNAL", GrossAmount: 1401.60, Collective: "commonshub"},
			{ID: "c", Provider: "etherscan", AccountSlug: "savings", Currency: "EURe",
				Type: "INTERNAL", GrossAmount: 1401.60, Collective: "commonshub"},
		},
	}
	data, err := json.Marshal(file)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "transactions.json"), data, 0644); err != nil {
		t.Fatal(err)
	}

	collectives, _ := buildMonthlyReportTaggedFlows(dataDir, "2026", "08")

	var found bool
	for _, c := range collectives {
		if c.Slug != "commonshub" {
			continue
		}
		found = true
		if len(c.Currencies) != 1 {
			t.Fatalf("got %d currency rows, want 1: %+v", len(c.Currencies), c.Currencies)
		}
		if got := c.Currencies[0].In; got != 100.00 {
			t.Errorf("commonshub in = %.2f, want 100.00 (internal transfers excluded)", got)
		}
	}
	if !found {
		t.Fatalf("commonshub row missing; got %+v", collectives)
	}
}
