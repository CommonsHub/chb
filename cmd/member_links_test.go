package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func linkHash(seed string) string {
	return strings.Repeat(seed, 64/len(seed))[:64]
}

// No identifier kind is privileged. Discord is what most people sign in with
// today; that must be a fact about the deployment, not about the code.
func TestNormalizeIdentifierAcceptsEveryKind(t *testing.T) {
	cases := map[string]bool{
		"email:sha256:" + linkHash("a"):                  true,
		"EMAIL:SHA256:" + strings.ToUpper(linkHash("a")): true, // case-normalised
		"  discord:user:123456789012345678  ":            true, // trimmed
		"discord:user:1":                                 true,
		"nostr:pubkey:" + linkHash("b"):                  true,
		"email:sha256:short":                             false,
		"discord:user:":                                  false,
		"discord:user:not-numeric":                       false,
		"nostr:pubkey:zz":                                false,
		"nostr:pubkey:" + strings.Repeat("g", 64):        false, // not hex
		"twitter:user:123":                               false, // unknown kind
		"":                                               false,
		linkHash("a"):                                    false, // bare hash, untyped
	}
	for raw, want := range cases {
		if _, ok := normalizeIdentifier(raw); ok != want {
			t.Errorf("normalizeIdentifier(%q) ok = %v, want %v", raw, ok, want)
		}
	}
}

// The whole point: a member who pays under one address and signs in with a
// Discord account resolves to one person from either handle.
func TestIdentityIndexResolvesAcrossKinds(t *testing.T) {
	stripeHash := linkHash("a")
	idx := buildMemberIdentityIndex([]MemberLink{{
		Identifiers: []string{
			"discord:user:123456789012345678",
			EmailIdentifier(stripeHash),
			"nostr:pubkey:" + linkHash("b"),
		},
		Note: "pays with a personal address",
	}})

	for _, identifier := range []string{
		"discord:user:123456789012345678",
		EmailIdentifier(stripeHash),
		"nostr:pubkey:" + linkHash("b"),
	} {
		if got := idx.Resolve(identifier); got != stripeHash {
			t.Errorf("Resolve(%q) = %q, want %q", identifier, got, stripeHash)
		}
	}
	if got := len(idx.Aliases(stripeHash)); got != 3 {
		t.Fatalf("aliases = %d, want 3", got)
	}
}

// Existing members keep their filenames: an unlinked email hash resolves to
// itself, so introducing the links file migrates nothing.
func TestIdentityIndexLeavesUnlinkedMembersAlone(t *testing.T) {
	idx := buildMemberIdentityIndex(nil)
	hash := linkHash("a")
	if got := idx.Resolve(EmailIdentifier(hash)); got != hash {
		t.Fatalf("Resolve = %q, want the hash itself", got)
	}
	// A Discord id nobody has linked belongs to no member — not to itself.
	if got := idx.Resolve("discord:user:999"); got != "" {
		t.Fatalf("Resolve(unlinked discord) = %q, want empty", got)
	}
	// A nil index still resolves plain email hashes.
	var nilIdx *MemberIdentityIndex
	if got := nilIdx.Resolve(EmailIdentifier(hash)); got != hash {
		t.Fatalf("nil index Resolve = %q, want the hash", got)
	}
}

// A member with no email at all — the Nostr-only case — still gets a stable
// id in the same shape, without anyone assigning one by hand.
func TestCanonicalIDForAMemberWithoutEmail(t *testing.T) {
	idx := buildMemberIdentityIndex([]MemberLink{{
		Identifiers: []string{"nostr:pubkey:" + linkHash("c"), "discord:user:42"},
	}})
	got := idx.Resolve("discord:user:42")
	if !emailHashPattern.MatchString(got) {
		t.Fatalf("canonical id = %q, want a 64-hex id", got)
	}
	// Stable across rebuilds.
	again := buildMemberIdentityIndex([]MemberLink{{
		Identifiers: []string{"nostr:pubkey:" + linkHash("c"), "discord:user:42"},
	}})
	if again.Resolve("discord:user:42") != got {
		t.Fatal("canonical id is not stable across rebuilds")
	}
}

// An explicit id wins, for a member whose canonical handle should not be
// derived from any provider.
func TestExplicitCanonicalIDWins(t *testing.T) {
	pinned := linkHash("d")
	idx := buildMemberIdentityIndex([]MemberLink{{
		ID:          pinned,
		Identifiers: []string{EmailIdentifier(linkHash("a")), "discord:user:7"},
	}})
	if got := idx.Resolve("discord:user:7"); got != pinned {
		t.Fatalf("Resolve = %q, want the pinned id %q", got, pinned)
	}
}

// Two entries sharing an identifier describe the same person — the shared
// identifier is the proof — so they must not end up as two members.
func TestLinksSharingAnIdentifierAreMerged(t *testing.T) {
	shared := "discord:user:555"
	first, second := linkHash("a"), linkHash("b")
	idx := buildMemberIdentityIndex([]MemberLink{
		{Identifiers: []string{shared, EmailIdentifier(first)}},
		{Identifiers: []string{shared, EmailIdentifier(second)}},
	})
	if idx.Resolve(EmailIdentifier(second)) != idx.Resolve(EmailIdentifier(first)) {
		t.Fatal("links sharing an identifier resolved to different members")
	}
}

// One bad identifier must not cost every other member their link.
func TestBadIdentifiersAreSkippedNotFatal(t *testing.T) {
	good := linkHash("a")
	idx := buildMemberIdentityIndex([]MemberLink{
		{Identifiers: []string{"twitter:user:1", "nonsense"}},
		{Identifiers: []string{"discord:user:8", EmailIdentifier(good), "also-nonsense"}},
	})
	if got := idx.Resolve("discord:user:8"); got != good {
		t.Fatalf("Resolve = %q, want %q — a valid link was lost to an invalid one", got, good)
	}
}

func TestLoadMemberLinksMissingFileIsNotAnError(t *testing.T) {
	t.Setenv("APP_DATA_DIR", t.TempDir())
	links, err := loadMemberLinks()
	if err != nil {
		t.Fatalf("a missing links file must not be an error: %v", err)
	}
	if len(links) != 0 {
		t.Fatalf("got %d links from nothing", len(links))
	}
}

func TestLoadMemberLinksReadsTheFile(t *testing.T) {
	appDir := t.TempDir()
	t.Setenv("APP_DATA_DIR", appDir)
	dir := filepath.Join(appDir, "settings")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(memberLinksFile{Links: []MemberLink{
		{Identifiers: []string{"discord:user:1", EmailIdentifier(linkHash("a"))}, Note: "n"},
	}})
	if err := os.WriteFile(filepath.Join(dir, memberLinksFileName), body, 0644); err != nil {
		t.Fatal(err)
	}

	links, err := loadMemberLinks()
	if err != nil {
		t.Fatal(err)
	}
	if len(links) != 1 || len(links[0].Identifiers) != 2 {
		t.Fatalf("links = %+v", links)
	}
}

// A member seen under two addresses gets one continuous history, and the
// identity index can find them by either.
func TestMemberHistoryUnionsLinkedAliases(t *testing.T) {
	appDir := t.TempDir()
	t.Setenv("APP_DATA_DIR", appDir)
	dataDir := t.TempDir()
	t.Setenv("DATA_DIR", dataDir)

	oldHash, newHash := linkHash("a"), linkHash("b")
	settingsDir := filepath.Join(appDir, "settings")
	if err := os.MkdirAll(settingsDir, 0755); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(memberLinksFile{Links: []MemberLink{{
		Identifiers: []string{
			EmailIdentifier(oldHash),
			EmailIdentifier(newHash),
			"discord:user:123456789012345678",
		},
	}}})
	if err := os.WriteFile(filepath.Join(settingsDir, memberLinksFileName), body, 0644); err != nil {
		t.Fatal(err)
	}

	// Paid under the old address in January, the new one in February.
	writeMembersFixture(t, dataDir, "2026", "01", MembersOutputFile{
		Members: []Member{memberFixture(oldHash, "Ada", "stripe", "active")},
	})
	writeMembersFixture(t, dataDir, "2026", "02", MembersOutputFile{
		Members: []Member{memberFixture(newHash, "Ada", "stripe", "active")},
	})

	generateMemberHistories(dataDir)

	// One history, both months, filed under the first email hash.
	h := readHistory(t, dataDir, oldHash)
	if h.MonthsActive != 2 {
		t.Fatalf("monthsActive = %d, want 2 — the aliases were not merged", h.MonthsActive)
	}
	if h.FirstMonth != "2026-01" || h.LastMonth != "2026-02" {
		t.Fatalf("span = %s..%s", h.FirstMonth, h.LastMonth)
	}
	// The second address must not have a history of its own.
	if p, ok := MemberHistoryPath(dataDir, newHash); ok {
		if _, err := os.Stat(p); err == nil {
			t.Fatal("the linked alias kept a separate history file")
		}
	}

	// And the index resolves every identifier to that member, including the
	// Discord account the person signs in with.
	data, err := os.ReadFile(filepath.Join(dataDir, "latest", "generated", "restricted", "members-index.json"))
	if err != nil {
		t.Fatalf("no identity index written: %v", err)
	}
	var index struct {
		Identifiers map[string]string `json:"identifiers"`
	}
	if err := json.Unmarshal(data, &index); err != nil {
		t.Fatal(err)
	}
	for _, identifier := range []string{
		"discord:user:123456789012345678",
		EmailIdentifier(oldHash),
		EmailIdentifier(newHash),
	} {
		if index.Identifiers[identifier] != oldHash {
			t.Errorf("index[%q] = %q, want %q", identifier, index.Identifiers[identifier], oldHash)
		}
	}
}
