package cmd

import (
	"strings"
	"testing"
)

// The salt is the membership identity, so a missing one is a configuration
// error to fix once — never a value to invent per run.
func TestResolveEmailHashSaltRefusesToMintSilently(t *testing.T) {
	t.Setenv("APP_DATA_DIR", t.TempDir())
	t.Setenv("EMAIL_HASH_SALT", "")

	_, err := resolveEmailHashSalt(nil)
	if err == nil {
		t.Fatal("a missing salt must be an error, not a freshly minted identity")
	}
	for _, want := range []string{"EMAIL_HASH_SALT", "--init-salt"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q: %v", want, err)
		}
	}
}

func TestResolveEmailHashSaltUsesTheEnvironment(t *testing.T) {
	t.Setenv("EMAIL_HASH_SALT", "  prod-abc123  ")
	got, err := resolveEmailHashSalt(nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != "prod-abc123" {
		t.Fatalf("salt = %q, want it trimmed to prod-abc123", got)
	}
}

// --init-salt is the one path allowed to create one, for a first-ever setup.
func TestResolveEmailHashSaltInitSaltMints(t *testing.T) {
	t.Setenv("APP_DATA_DIR", t.TempDir())
	t.Setenv("EMAIL_HASH_SALT", "")

	got, err := resolveEmailHashSalt([]string{"--init-salt"})
	if err != nil {
		t.Fatal(err)
	}
	if got == "" {
		t.Fatal("--init-salt produced no salt")
	}
	// It must be persisted, or the next run mints a different one and every
	// membership id changes.
	if loadConfigEnv(configEnvPath())["EMAIL_HASH_SALT"] != got {
		t.Fatal("minted salt was not saved to config.env")
	}
}

// An existing salt wins over --init-salt: the flag must never overwrite a
// live identity.
func TestResolveEmailHashSaltInitSaltDoesNotOverwrite(t *testing.T) {
	t.Setenv("APP_DATA_DIR", t.TempDir())
	t.Setenv("EMAIL_HASH_SALT", "prod-existing")

	got, err := resolveEmailHashSalt([]string{"--init-salt"})
	if err != nil {
		t.Fatal(err)
	}
	if got != "prod-existing" {
		t.Fatalf("salt = %q, want the existing prod-existing", got)
	}
}
