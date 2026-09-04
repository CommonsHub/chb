package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	etherscansource "github.com/CommonsHub/chb/providers/etherscan"
)

func TestPlanEtherscanRenames(t *testing.T) {
	// Isolate settings so etherscanAddressOwners() reads our accounts.json,
	// not the developer's real config.
	appDir := filepath.Join(t.TempDir(), "app")
	t.Setenv("APP_DATA_DIR", appDir)
	if err := os.MkdirAll(filepath.Join(appDir, "settings"), 0755); err != nil {
		t.Fatal(err)
	}
	accounts := []map[string]any{
		{"slug": "savings", "provider": "etherscan", "chain": "gnosis", "address": "0xb01cCCE2d75d517EE520De31eAE6aFCa735aadc1"},
		{"slug": "savings-old", "provider": "etherscan", "chain": "gnosis", "address": "0x6fDF0AaE33E313d9C98D2Aa19Bcd8EF777912CBf"},
	}
	accJSON, _ := json.Marshal(accounts)
	if err := os.WriteFile(filepath.Join(appDir, "settings", "accounts.json"), accJSON, 0644); err != nil {
		t.Fatal(err)
	}

	dataDir := t.TempDir()
	gnosis := etherscansource.Path(dataDir, "2024", "01", "gnosis")
	celo := etherscansource.Path(dataDir, "2024", "01", "celo")
	for _, d := range []string{gnosis, celo} {
		if err := os.MkdirAll(d, 0755); err != nil {
			t.Fatal(err)
		}
	}

	// Legacy file whose wallet is owned by a *different* account → should be
	// re-filed under the owning slug (savings-old) and address-qualified.
	refile := filepath.Join(gnosis, "savings.EURe.json")
	writeJSONFileForTest(t, refile, etherscansource.CacheFile{
		Transactions: []etherscansource.TokenTransfer{{Hash: "0x1"}},
		Account:      "0x6fDF0AaE33E313d9C98D2Aa19Bcd8EF777912CBf",
		Chain:        "gnosis",
		Token:        "EURe",
	})
	// Legacy file without an account address → should be skipped.
	noAddr := filepath.Join(celo, "cht.CHT.json")
	writeJSONFileForTest(t, noAddr, etherscansource.CacheFile{
		Transactions: []etherscansource.TokenTransfer{{Hash: "0x2"}},
		Account:      "",
		Chain:        "celo",
		Token:        "CHT",
	})
	// Already-correctly-named file (no configured owner) → left alone.
	qualified := filepath.Join(gnosis, "coffee.0xab33-7800.EURb.json")
	writeJSONFileForTest(t, qualified, etherscansource.CacheFile{
		Transactions: []etherscansource.TokenTransfer{{Hash: "0x3"}},
		Account:      "0xab33000000000000000000000000000000bc7800",
		Chain:        "gnosis",
		Token:        "EURb",
	})

	renames, skipped := planEtherscanRenames(dataDir)

	if len(renames) != 1 {
		t.Fatalf("renames = %d, want 1: %+v", len(renames), renames)
	}
	wantTo := filepath.Join(gnosis, "savings-old.0x6fdf-2cbf.EURe.json")
	if renames[0].to != wantTo {
		t.Errorf("rename target = %q, want %q", renames[0].to, wantTo)
	}
	if len(skipped) != 1 {
		t.Fatalf("skipped = %d, want 1: %v", len(skipped), skipped)
	}
}

func TestPlanStaleSourceDirs(t *testing.T) {
	dataDir := t.TempDir()
	// A month with both sources/ and providers/ → sources/ is stale.
	withProviders := filepath.Join(dataDir, "2024", "01")
	if err := os.MkdirAll(filepath.Join(withProviders, "sources", "odoo"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(withProviders, "providers", "odoo"), 0755); err != nil {
		t.Fatal(err)
	}
	// A month with only sources/ (no providers/ sibling) → left alone.
	soloSources := filepath.Join(dataDir, "2024", "02", "sources")
	if err := os.MkdirAll(soloSources, 0755); err != nil {
		t.Fatal(err)
	}

	dirs := planStaleSourceDirs(dataDir)
	if len(dirs) != 1 || dirs[0] != filepath.Join(withProviders, "sources") {
		t.Fatalf("staleDirs = %v, want [%s]", dirs, filepath.Join(withProviders, "sources"))
	}
}

// The same wallet address is routinely configured on two chains (a Gnosis
// account and its Polygon counterpart). Ownership must be resolved per chain:
// an address-only map keeps whichever account comes last in accounts.json and
// renames the Gnosis archives to the Polygon slug, after which generate books
// Gnosis transfers against the Polygon account.
func TestPlanEtherscanRenamesDoesNotCrossChains(t *testing.T) {
	appDir := filepath.Join(t.TempDir(), "app")
	t.Setenv("APP_DATA_DIR", appDir)
	if err := os.MkdirAll(filepath.Join(appDir, "settings"), 0755); err != nil {
		t.Fatal(err)
	}
	const shared = "0x6fDF0AaE33E313d9C98D2Aa19Bcd8EF777912CBf"
	// Polygon deliberately listed last — it is the entry an address-only map
	// would keep for both chains.
	accounts := []map[string]any{
		{"slug": "savings-hacked", "provider": "etherscan", "chain": "gnosis", "address": shared},
		{"slug": "savings-polygon", "provider": "etherscan", "chain": "polygon", "address": shared},
	}
	accJSON, _ := json.Marshal(accounts)
	if err := os.WriteFile(filepath.Join(appDir, "settings", "accounts.json"), accJSON, 0644); err != nil {
		t.Fatal(err)
	}

	dataDir := t.TempDir()
	gnosis := etherscansource.Path(dataDir, "2024", "01", "gnosis")
	polygon := etherscansource.Path(dataDir, "2024", "01", "polygon")
	for _, d := range []string{gnosis, polygon} {
		if err := os.MkdirAll(d, 0755); err != nil {
			t.Fatal(err)
		}
	}

	// Both archives are already named for the account that owns the wallet on
	// their own chain. Neither may be renamed.
	gnosisFile := filepath.Join(gnosis, "savings-hacked.0x6fdf-2cbf.EURe.json")
	writeJSONFileForTest(t, gnosisFile, etherscansource.CacheFile{
		Transactions: []etherscansource.TokenTransfer{{Hash: "0x1"}},
		Account:      shared, Chain: "gnosis", Token: "EURe",
	})
	polygonFile := filepath.Join(polygon, "savings-polygon.0x6fdf-2cbf.EURe.json")
	writeJSONFileForTest(t, polygonFile, etherscansource.CacheFile{
		Transactions: []etherscansource.TokenTransfer{{Hash: "0x2"}},
		Account:      shared, Chain: "polygon", Token: "EURe",
	})

	if renames, _ := planEtherscanRenames(dataDir); len(renames) != 0 {
		for _, r := range renames {
			t.Errorf("unexpected rename %s → %s", r.relFrom, filepath.Base(r.to))
		}
		t.Fatalf("renames = %d, want 0", len(renames))
	}

	// And a Gnosis archive misfiled under the Polygon slug (what the
	// address-only map produced) is re-filed back to its Gnosis owner.
	misfiled := filepath.Join(gnosis, "savings-polygon.0x6fdf-2cbf.EURb.json")
	writeJSONFileForTest(t, misfiled, etherscansource.CacheFile{
		Transactions: []etherscansource.TokenTransfer{{Hash: "0x3"}},
		Account:      shared, Chain: "gnosis", Token: "EURb",
	})
	renames, _ := planEtherscanRenames(dataDir)
	if len(renames) != 1 {
		t.Fatalf("renames = %d, want 1: %+v", len(renames), renames)
	}
	want := filepath.Join(gnosis, "savings-hacked.0x6fdf-2cbf.EURb.json")
	if renames[0].to != want {
		t.Errorf("rename target = %q, want %q", renames[0].to, want)
	}
}
