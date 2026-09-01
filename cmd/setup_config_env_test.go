package cmd

import (
	"os"
	"path/filepath"
	"testing"
)

// TestSaveConfigEnvPreservesUnknownKeys covers the bug that broke month-over-
// month member identity: saveConfigEnv only wrote keys listed in envKeys, so
// the EMAIL_HASH_SALT that `chb members sync` generates was silently dropped.
// Every sync then minted a fresh salt, every emailHash changed, and no member
// could be matched across months.
func TestSaveConfigEnvPreservesUnknownKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.env")

	env := map[string]string{
		"ODOO_URL":        "https://example.odoo.com",
		"EMAIL_HASH_SALT": "prod-deadbeef",
		"SOME_FUTURE_KEY": "keep-me",
		"EMPTY_KEY":       "",
	}
	if err := saveConfigEnv(path, env); err != nil {
		t.Fatalf("saveConfigEnv: %v", err)
	}

	got := loadConfigEnv(path)
	for key, want := range map[string]string{
		"ODOO_URL":        "https://example.odoo.com",
		"EMAIL_HASH_SALT": "prod-deadbeef",
		"SOME_FUTURE_KEY": "keep-me",
	} {
		if got[key] != want {
			t.Errorf("loadConfigEnv()[%q] = %q, want %q", key, got[key], want)
		}
	}
	if _, ok := got["EMPTY_KEY"]; ok {
		t.Errorf("empty value should not be persisted, got %+v", got)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("config.env mode = %o, want 600 (it holds secrets)", mode)
	}
}

// TestSaveConfigEnvRoundTripsSalt is the concrete regression: generate a salt,
// save, reload — the salt must survive, or hashes are unstable across runs.
func TestSaveConfigEnvRoundTripsSalt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.env")

	first := map[string]string{"EMAIL_HASH_SALT": "prod-1111"}
	if err := saveConfigEnv(path, first); err != nil {
		t.Fatal(err)
	}
	// A later `chb setup` run rewrites the file after loading it; the salt
	// must come along rather than be regenerated.
	reloaded := loadConfigEnv(path)
	reloaded["STRIPE_SECRET_KEY"] = "sk_test_x"
	if err := saveConfigEnv(path, reloaded); err != nil {
		t.Fatal(err)
	}
	if got := loadConfigEnv(path)["EMAIL_HASH_SALT"]; got != "prod-1111" {
		t.Errorf("EMAIL_HASH_SALT = %q after a setup rewrite, want it preserved", got)
	}
}
