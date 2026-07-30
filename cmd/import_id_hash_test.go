package cmd

import "testing"

// A blockchain tx reloaded from transactions.json usually has an empty TxHash
// (it's internal + omitempty) but keeps ProviderID (the raw 0x hash) and ID
// (the NIP-73 URI "ethereum:100:tx:0x…"). The unique_import_id must be built
// from the clean hash so it matches what was pushed to Odoo — falling back to
// the prefixed ID would corrupt it and silently break metadata/reconcile
// matching (this was the "burn EURe" line that never got its Monerium memo).
func TestBuildUniqueImportIDPrefersProviderIDOverPrefixedID(t *testing.T) {
	acc := &AccountConfig{
		Provider: "etherscan",
		Chain:    "gnosis",
		Address:  "0xD578e7cd845e1ecD979b04784e77068D5eBd8716",
	}
	hash := "0x3a0a61f59b0228bb8352e30b8fadc329d5db8685d3241f2dbfa44b4955eb752e"
	tx := TransactionEntry{
		ID:         "ethereum:100:tx:" + hash, // NIP-73 URI, must NOT leak into the id
		ProviderID: hash,                      // clean hash — the one Odoo stored
		// TxHash intentionally empty (as after reload from public JSON)
	}
	want := "gnosis:0xd578e7cd845e1ecd979b04784e77068d5ebd8716:" + hash + ":0"
	if got := buildUniqueImportID(acc, tx); got != want {
		t.Errorf("import id = %q, want %q", got, want)
	}

	// When TxHash is present it still wins (unchanged behaviour).
	tx.TxHash = hash
	if got := buildUniqueImportID(acc, tx); got != want {
		t.Errorf("with TxHash set: import id = %q, want %q", got, want)
	}
}
