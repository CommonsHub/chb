package cmd

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// memberLinksFileName is the operator-maintained list of identifiers that
// belong to the same person. Members routinely pay with one address and sign
// in with another — a work email on Stripe, a personal one on Discord — and
// without a link the two look like two different people, or like nobody.
const memberLinksFileName = "member-links.json"

// Member identifiers follow the NIP-73 URI convention the rest of the dataset
// already uses (see docs/data-model.md): a typed, self-describing handle.
//
//	discord:user:123456789012345678
//	nostr:pubkey:<32-byte hex>
//	email:sha256:<the salted emailHash>
//
// No identifier kind is privileged. Nothing below knows that Discord is what
// most people sign in with today, which is the point: adding Nostr auth means
// adding entries of another kind, not another special case.
const (
	identifierKindEmail   = "email:sha256:"
	identifierKindDiscord = "discord:user:"
	identifierKindNostr   = "nostr:pubkey:"
)

// MemberLink is one person's set of identifiers.
type MemberLink struct {
	// ID pins the canonical member id. Leave it empty and the first
	// email:sha256: identifier is used, which keeps every existing member's
	// history filename exactly as it was. Set it for someone who has no email
	// in the system at all — a member who only ever authenticates over Nostr.
	ID          string   `json:"id,omitempty"`
	Identifiers []string `json:"identifiers"`
	// Note says why this link exists, for whoever reads the file next.
	Note string `json:"note,omitempty"`
}

type memberLinksFile struct {
	Description string       `json:"description,omitempty"`
	Links       []MemberLink `json:"links"`
}

// MemberIdentityIndex resolves any identifier to the member it belongs to.
type MemberIdentityIndex struct {
	// byIdentifier maps a normalised identifier to a canonical member id.
	byIdentifier map[string]string
	// aliases lists every identifier of a member, keyed by canonical id.
	aliases map[string][]string
}

// normalizeIdentifier lower-cases and trims an identifier, and returns ok=false
// for anything that is not one of the known kinds. An unknown kind is refused
// rather than stored: identifiers become lookup keys, and a typo that silently
// becomes its own "member" is worse than an error.
func normalizeIdentifier(raw string) (string, bool) {
	id := strings.ToLower(strings.TrimSpace(raw))
	switch {
	case strings.HasPrefix(id, identifierKindEmail):
		return id, emailHashPattern.MatchString(strings.TrimPrefix(id, identifierKindEmail))
	case strings.HasPrefix(id, identifierKindDiscord):
		v := strings.TrimPrefix(id, identifierKindDiscord)
		return id, v != "" && strings.IndexFunc(v, func(r rune) bool { return r < '0' || r > '9' }) < 0
	case strings.HasPrefix(id, identifierKindNostr):
		v := strings.TrimPrefix(id, identifierKindNostr)
		if len(v) != 64 {
			return id, false
		}
		_, err := hex.DecodeString(v)
		return id, err == nil
	default:
		return id, false
	}
}

// EmailIdentifier builds the identifier for a membership id (an emailHash).
func EmailIdentifier(emailHash string) string {
	return identifierKindEmail + strings.ToLower(strings.TrimSpace(emailHash))
}

// DiscordIdentifier builds the identifier for a Discord account id.
func DiscordIdentifier(discordID string) string {
	return identifierKindDiscord + strings.TrimSpace(discordID)
}

// loadMemberLinks reads settings/member-links.json. A missing file is not an
// error: most members need no link at all.
func loadMemberLinks() ([]MemberLink, error) {
	data, err := os.ReadFile(settingsFilePath(memberLinksFileName))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var f memberLinksFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("%s: %w", memberLinksFileName, err)
	}
	return f.Links, nil
}

// buildMemberIdentityIndex turns the links into a resolver.
//
// A malformed entry is reported and skipped: one bad identifier must not cost
// every other member their link. Two links that share an identifier are merged
// onto the first one's canonical id, because they describe the same person by
// definition — the shared identifier is the proof.
func buildMemberIdentityIndex(links []MemberLink) *MemberIdentityIndex {
	idx := &MemberIdentityIndex{
		byIdentifier: map[string]string{},
		aliases:      map[string][]string{},
	}

	for i, link := range links {
		var identifiers []string
		for _, raw := range link.Identifiers {
			id, ok := normalizeIdentifier(raw)
			if !ok {
				Warnf("⚠ %s link %d: %q is not a recognised identifier (expected %suser:…, %s…, or %s…) — skipped",
					memberLinksFileName, i+1, raw, identifierKindDiscord[:len("discord:")], identifierKindNostr, identifierKindEmail)
				continue
			}
			identifiers = append(identifiers, id)
		}
		if len(identifiers) == 0 {
			Warnf("⚠ %s link %d has no usable identifier — skipped", memberLinksFileName, i+1)
			continue
		}

		canonical := canonicalMemberID(link, identifiers)
		// An identifier already claimed by an earlier link means both describe
		// the same person; fold this one onto the established id.
		for _, id := range identifiers {
			if existing, claimed := idx.byIdentifier[id]; claimed && existing != canonical {
				canonical = existing
				break
			}
		}
		for _, id := range identifiers {
			idx.byIdentifier[id] = canonical
			if !containsString(idx.aliases[canonical], id) {
				idx.aliases[canonical] = append(idx.aliases[canonical], id)
			}
		}
	}
	return idx
}

// canonicalMemberID picks a member's stable id: the explicit one when given,
// otherwise the first email hash — which leaves every existing history file
// named exactly as it was — and failing both, a digest of the first
// identifier, so a member with no email still gets a stable 64-hex id in the
// same shape as everyone else's.
func canonicalMemberID(link MemberLink, identifiers []string) string {
	if id := strings.ToLower(strings.TrimSpace(link.ID)); emailHashPattern.MatchString(id) {
		return id
	}
	for _, id := range identifiers {
		if strings.HasPrefix(id, identifierKindEmail) {
			return strings.TrimPrefix(id, identifierKindEmail)
		}
	}
	sum := sha256.Sum256([]byte(identifiers[0]))
	return hex.EncodeToString(sum[:])
}

// Resolve returns the canonical member id for any identifier. Unlinked
// identifiers resolve to themselves when they are an email hash — the case
// for every member who needs no link — and to "" otherwise.
func (idx *MemberIdentityIndex) Resolve(identifier string) string {
	id, ok := normalizeIdentifier(identifier)
	if !ok {
		return ""
	}
	if idx != nil {
		if canonical, found := idx.byIdentifier[id]; found {
			return canonical
		}
	}
	if strings.HasPrefix(id, identifierKindEmail) {
		return strings.TrimPrefix(id, identifierKindEmail)
	}
	return ""
}

// Aliases lists every identifier belonging to a canonical member id.
func (idx *MemberIdentityIndex) Aliases(memberID string) []string {
	if idx == nil {
		return nil
	}
	return idx.aliases[strings.ToLower(memberID)]
}
