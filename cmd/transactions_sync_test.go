package cmd

import (
	"os"
	"path/filepath"
	"testing"

	etherscansource "github.com/CommonsHub/chb/providers/etherscan"
	stripesource "github.com/CommonsHub/chb/providers/stripe"
)

func TestCountNewTokenTransfersUsesExistingCacheKeys(t *testing.T) {
	existingTx := etherscansource.TokenTransfer{
		Hash: "0x1", From: "0xaaa", To: "0xbbb", Value: "100", TimeStamp: "1770000000", TokenDecimal: "18", TokenSymbol: "EURe",
	}
	newTx := etherscansource.TokenTransfer{
		Hash: "0x2", From: "0xaaa", To: "0xbbb", Value: "100", TimeStamp: "1770000010", TokenDecimal: "18", TokenSymbol: "EURe",
	}
	existing := map[string]bool{tokenTransferKey(existingTx): true}

	if got := countNewTokenTransfers(existing, []etherscansource.TokenTransfer{existingTx, newTx}); got != 1 {
		t.Fatalf("countNewTokenTransfers() = %d, want 1", got)
	}
}

func TestShouldRunBlockchainEnrichmentOnlyForChangedDefaultSync(t *testing.T) {
	changed := map[string]bool{"savings": true}

	if !shouldRunBlockchainEnrichment("savings", false, changed) {
		t.Fatal("changed account should run enrichment")
	}
	if shouldRunBlockchainEnrichment("checking", false, changed) {
		t.Fatal("unchanged default account should not run enrichment")
	}
	if !shouldRunBlockchainEnrichment("checking", true, changed) {
		t.Fatal("explicit refresh should run enrichment")
	}
}

// A month archived before charges.json/customers.json existed still holds its
// balance transactions. Counting that as cached is what let incomplete months
// survive every later --history run.
func TestStripeMonthProviderFilesCachedRequiresEveryFile(t *testing.T) {
	dir := t.TempDir()
	write := func(name string) {
		path := stripesource.Path(dir, "2026", "04", name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("{}"), 0600); err != nil {
			t.Fatal(err)
		}
	}

	if stripeMonthProviderFilesCached(dir, "2026", "04") {
		t.Fatal("empty month reported as cached")
	}
	write(stripesource.BalanceTransactionsFile)
	if stripeMonthProviderFilesCached(dir, "2026", "04") {
		t.Fatal("balance transactions alone should not count as a complete archive")
	}
	write(stripesource.ChargesFile)
	if stripeMonthProviderFilesCached(dir, "2026", "04") {
		t.Fatal("customers.json still missing")
	}
	write(stripesource.CustomersFile)
	if !stripeMonthProviderFilesCached(dir, "2026", "04") {
		t.Fatal("all three files present but month not reported as cached")
	}
}

func TestAllStripeMonthsCachedNeedsEveryMonth(t *testing.T) {
	dir := t.TempDir()
	writeMonth := func(year, month string) {
		for _, name := range []string{
			stripesource.BalanceTransactionsFile,
			stripesource.ChargesFile,
			stripesource.CustomersFile,
		} {
			path := stripesource.Path(dir, year, month, name)
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("{}"), 0600); err != nil {
				t.Fatal(err)
			}
		}
	}

	writeMonth("2026", "01")
	writeMonth("2026", "03")
	if allStripeMonthsCached(dir, "2026-01", "2026-03") {
		t.Fatal("range with a gap in February reported as fully cached")
	}
	writeMonth("2026", "02")
	if !allStripeMonthsCached(dir, "2026-01", "2026-03") {
		t.Fatal("complete range not reported as cached")
	}
	if allStripeMonthsCached(dir, "nonsense", "2026-03") {
		t.Fatal("unparseable range should not report as cached")
	}
}

// An incremental pull only fetches new balance transactions, so the customer
// PII archived by earlier runs must survive a month rewrite.
func TestMergeFetchedCustomersIntoMonthKeepsExistingPII(t *testing.T) {
	dir := t.TempDir()
	existing := &stripesource.CustomerData{
		FetchedAt: "2026-04-01T00:00:00Z",
		Customers: map[string]*stripesource.CustomerPII{
			"txn_old": {Name: "Ada", Email: "ada@example.org"},
		},
	}
	if err := stripesource.WriteJSON(dir, "2026", "04", existing, stripesource.CustomersFile); err != nil {
		t.Fatal(err)
	}

	merged, added := mergeFetchedCustomersIntoMonth(dir, "2026", "04", map[string]*stripesource.CustomerPII{
		"txn_new": {Name: "Grace", Email: "grace@example.org"},
		"txn_nil": nil,
	})
	if added != 1 {
		t.Fatalf("added = %d, want 1", added)
	}
	if len(merged) != 2 {
		t.Fatalf("merged has %d entries, want 2 (old + new)", len(merged))
	}
	if merged["txn_old"] == nil || merged["txn_old"].Name != "Ada" {
		t.Fatal("earlier run's customer PII was dropped")
	}
	if merged["txn_new"] == nil || merged["txn_new"].Name != "Grace" {
		t.Fatal("freshly fetched customer PII missing")
	}
	if _, ok := merged["txn_nil"]; ok {
		t.Fatal("nil PII should not be stored")
	}

	// Re-merging the same fetch adds nothing new.
	if _, added := mergeFetchedCustomersIntoMonth(dir, "2026", "04", nil); added != 0 {
		t.Fatalf("re-merge added = %d, want 0", added)
	}
}
