package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// membershipHistoryStartMonth is the first month the per-member history
// covers. Earlier months exist on disk but every one of them is an Odoo
// reconstruction (odooDerived) rebuilt from a later snapshot: a subscription
// cancelled before that snapshot was taken is missing entirely, so those
// months can undercount and cannot be told apart from a real gap in
// someone's membership. A history is a claim about a person, so it starts
// where the record is trustworthy.
const membershipHistoryStartMonth = "2026-01"

// memberHistoryDirName is the directory, under generated/restricted/, holding
// one file per member.
//
// "restricted", not "private". Nothing under private/ is ever served — it is
// operator-only material. These files exist precisely to be read by the member
// they describe, once that member has signed in, so they need a tree that says
// "served, but only to its owner". Both trees get 0700 directories and skip
// the PII scrubber; only restricted/ has a reader.
const memberHistoryDirName = "members"

// emailHashPattern matches the sha256 hex digest used as a membership id. The
// id becomes a filename, so it is validated rather than trusted.
var emailHashPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// MemberHistoryMonth is one month's standing for one member. Fields mirror the
// monthly members.json entry, minus the identity (which lives once, on the
// file) and minus subscriptionUrl (a staff dashboard link, not a member's).
type MemberHistoryMonth struct {
	Month              string         `json:"month"` // YYYY-MM
	Source             string         `json:"source,omitempty"`
	Status             string         `json:"status"`
	Plan               string         `json:"plan,omitempty"`
	Amount             MemberAmount   `json:"amount"`
	Interval           string         `json:"interval,omitempty"`
	CurrentPeriodStart string         `json:"currentPeriodStart,omitempty"`
	CurrentPeriodEnd   string         `json:"currentPeriodEnd,omitempty"`
	LatestPayment      *MemberPayment `json:"latestPayment,omitempty"`
	IsOrganization     bool           `json:"isOrganization,omitempty"`
	// Derived marks a month whose Odoo membership was reconstructed from a
	// later snapshot instead of captured at the time. Carried per month so a
	// reader can weigh each entry rather than the timeline as a whole.
	Derived     bool   `json:"derived,omitempty"`
	DerivedFrom string `json:"derivedFrom,omitempty"`
}

// MemberHistoryFile is one member's timeline: every month the person appears
// in, from membershipHistoryStartMonth onwards.
type MemberHistoryFile struct {
	SchemaVersion int    `json:"schemaVersion"`
	MemberID      string `json:"memberId"` // emailHash — the stable identity across months
	FirstName     string `json:"firstName,omitempty"`
	Discord       string `json:"discord,omitempty"`
	// CreatedAt is the earliest subscription start seen for this member, which
	// can predate FirstMonth — the history window starts at 2026-01, the
	// membership may not.
	CreatedAt string `json:"createdAt,omitempty"`
	// FirstMonth and LastMonth bound the months present in Months. Gaps
	// between them are real: a month the member does not appear in is a month
	// they were not a member.
	FirstMonth string `json:"firstMonth,omitempty"`
	LastMonth  string `json:"lastMonth,omitempty"`
	// Identifiers lists every handle this member is known by — the email
	// hashes they have paid under, plus any Discord or Nostr identity linked
	// to them. The member id is one of these, not a separate kind of thing.
	Identifiers  []string             `json:"identifiers,omitempty"`
	MonthsActive int                  `json:"monthsActive"`
	GeneratedAt  string               `json:"generatedAt"`
	Months       []MemberHistoryMonth `json:"months"`
}

// loadMemberHistory reads one member's history file.
func loadMemberHistory(path string) (*MemberHistoryFile, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	var h MemberHistoryFile
	if json.Unmarshal(data, &h) != nil {
		return nil, false
	}
	return &h, true
}

// memberHistoryDir returns the directory holding the per-member files.
func memberHistoryDir(dataDir string) string {
	return filepath.Join(dataDir, "latest", "generated", restrictedDirSegment, memberHistoryDirName)
}

// MemberHistoryPath returns the file for one membership id. Returns ok=false
// for an id that is not a well-formed hash, so a caller can never be talked
// into reading or writing outside the directory.
func MemberHistoryPath(dataDir, memberID string) (string, bool) {
	id := strings.ToLower(strings.TrimSpace(memberID))
	if !emailHashPattern.MatchString(id) {
		return "", false
	}
	return filepath.Join(memberHistoryDir(dataDir), id+".json"), true
}

// generateMemberHistories rebuilds the per-member history files from the
// monthly members.json snapshots. It returns a short status line for the
// generate step.
//
// The files are rewritten from scratch on every run: a member who dropped out
// of every month in the window must not keep a stale file, and the monthly
// snapshots are the only source of truth for who existed when.
func generateMemberHistories(dataDir string) string {
	months := membersMonthsInWindow(dataDir, membershipHistoryStartMonth)
	if len(months) == 0 {
		return ""
	}

	// Identifiers that belong to the same person resolve to one canonical id,
	// so a member who pays with one address and signs in with another gets a
	// single continuous history instead of two fragments.
	links, linkErr := loadMemberLinks()
	if linkErr != nil {
		Warnf("⚠ %v — continuing without member links", linkErr)
	}
	identities := buildMemberIdentityIndex(links)

	histories := map[string]*MemberHistoryFile{}
	monthIDs := map[string]map[string]bool{}
	for _, ym := range months {
		parts := strings.Split(ym, "-")
		if len(parts) != 2 {
			continue
		}
		mf, ok := loadMembersFile(dataDir, parts[0], parts[1])
		if !ok {
			continue
		}
		for _, m := range mf.Members {
			hash := strings.ToLower(strings.TrimSpace(m.Accounts.EmailHash))
			if !emailHashPattern.MatchString(hash) {
				// No stable identity — nothing to file this month under, and
				// guessing would attach one person's history to another.
				continue
			}
			// Unlinked members resolve to their own hash, so this is a no-op
			// for everyone who needs no link.
			id := identities.Resolve(EmailIdentifier(hash))
			if id == "" {
				continue
			}
			if monthIDs[ym] == nil {
				monthIDs[ym] = map[string]bool{}
			}
			monthIDs[ym][id] = true
			h := histories[id]
			if h == nil {
				h = &MemberHistoryFile{SchemaVersion: 1, MemberID: id}
				histories[id] = h
			}
			if seenAs := EmailIdentifier(hash); !containsString(h.Identifiers, seenAs) {
				h.Identifiers = append(h.Identifiers, seenAs)
			}
			// Later months win for the profile fields: a member's display name
			// or linked Discord can change, and the most recent reading is the
			// current one.
			if m.FirstName != "" {
				h.FirstName = m.FirstName
			}
			if m.Accounts.Discord != nil && *m.Accounts.Discord != "" {
				h.Discord = *m.Accounts.Discord
			}
			if m.CreatedAt != "" && (h.CreatedAt == "" || m.CreatedAt < h.CreatedAt) {
				h.CreatedAt = m.CreatedAt
			}
			h.Months = append(h.Months, MemberHistoryMonth{
				Month:              ym,
				Source:             m.Source,
				Status:             m.Status,
				Plan:               m.Plan,
				Amount:             m.Amount,
				Interval:           m.Interval,
				CurrentPeriodStart: m.CurrentPeriodStart,
				CurrentPeriodEnd:   m.CurrentPeriodEnd,
				LatestPayment:      m.LatestPayment,
				IsOrganization:     m.IsOrganization,
				Derived:            mf.OdooDerived && m.Source == "odoo",
				DerivedFrom:        derivedFromFor(mf, m),
			})
		}
	}

	_ = warnOnIdentityDiscontinuity(months, monthIDs)

	if err := resetMemberHistoryDir(dataDir); err != nil {
		Warnf("⚠ member history: %v", err)
		return ""
	}

	generatedAt := time.Now().UTC().Format(time.RFC3339)
	written := 0
	for id, h := range histories {
		// Merging two aliases can contribute the same month twice — someone
		// who paid under both addresses in the overlap. One entry per month.
		h.Months = dedupeHistoryMonths(h.Months)
		sort.Slice(h.Months, func(i, j int) bool { return h.Months[i].Month < h.Months[j].Month })
		for _, alias := range identities.Aliases(id) {
			if !containsString(h.Identifiers, alias) {
				h.Identifiers = append(h.Identifiers, alias)
			}
		}
		sort.Strings(h.Identifiers)
		h.MonthsActive = len(h.Months)
		if len(h.Months) > 0 {
			h.FirstMonth = h.Months[0].Month
			h.LastMonth = h.Months[len(h.Months)-1].Month
		}
		h.GeneratedAt = generatedAt

		path, ok := MemberHistoryPath(dataDir, id)
		if !ok {
			continue
		}
		data, err := json.MarshalIndent(h, "", "  ")
		if err != nil {
			continue
		}
		if err := writeDataFile(path, data); err != nil {
			Warnf("⚠ member history %s: %v", id[:8], err)
			continue
		}
		written++
	}
	writeMemberIdentityIndex(dataDir, histories)

	if written == 0 {
		return ""
	}
	return fmt.Sprintf("%s over %s",
		Pluralize(written, "member", ""), Pluralize(len(months), "month", ""))
}

// derivedFromFor reports which snapshot a reconstructed month was rebuilt
// from. Only Odoo membership is ever reconstructed; Stripe months are read
// back from Stripe's own history and are always as-observed.
func derivedFromFor(mf *MembersOutputFile, m Member) string {
	if mf.OdooDerived && m.Source == "odoo" {
		return mf.OdooDerivedFrom
	}
	return ""
}

// resetMemberHistoryDir empties the per-member directory so a member who no
// longer appears in any month in the window leaves no stale file behind.
// Only files this generator owns (<64-hex>.json) are removed.
func resetMemberHistoryDir(dataDir string) error {
	dir := memberHistoryDir(dataDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		if !emailHashPattern.MatchString(strings.TrimSuffix(name, ".json")) {
			continue
		}
		if err := os.Remove(filepath.Join(dir, name)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

// membersMonthsInWindow lists the YYYY-MM months at or after startMonth that
// have a generated members.json, oldest first.
func membersMonthsInWindow(dataDir, startMonth string) []string {
	var months []string
	years, _ := os.ReadDir(dataDir)
	for _, yd := range years {
		if !yd.IsDir() || !isYearSegment(yd.Name()) {
			continue
		}
		monthDirs, _ := os.ReadDir(filepath.Join(dataDir, yd.Name()))
		for _, md := range monthDirs {
			if !md.IsDir() || !isMonthSegment(md.Name()) {
				continue
			}
			ym := yd.Name() + "-" + md.Name()
			if ym < startMonth {
				continue
			}
			if !fileExists(filepath.Join(dataDir, yd.Name(), md.Name(), "generated", "members.json")) {
				continue
			}
			months = append(months, ym)
		}
	}
	sort.Strings(months)
	return months
}

// warnOnIdentityDiscontinuity reports months that share no membership id at all
// with the month before them. The id is a salted hash of the member's email, so
// two adjacent months of a stable membership overlap almost completely — unless
// EMAIL_HASH_SALT changed between the two syncs, in which case every member is
// re-hashed into a brand-new identity and their history silently splits in two.
//
// This is not hypothetical: 2026-04 was once written under a different salt and
// shared zero ids with either neighbour, turning 61 continuing members into 61
// one-month strangers. Re-running `chb members sync --month <ym> --force`
// followed by `chb generate <ym> --force` re-hashes the month with the current
// salt.
//
// Both months need enough members for the emptiness to mean anything; a month
// with one or two people can legitimately share nobody with the next.
func warnOnIdentityDiscontinuity(months []string, monthIDs map[string]map[string]bool) []string {
	const minMembersToJudge = 5
	var flagged []string
	for i := 1; i < len(months); i++ {
		prev, cur := monthIDs[months[i-1]], monthIDs[months[i]]
		if len(prev) < minMembersToJudge || len(cur) < minMembersToJudge {
			continue
		}
		shared := 0
		for id := range cur {
			if prev[id] {
				shared++
			}
		}
		if shared > 0 {
			continue
		}
		flagged = append(flagged, months[i])
		Warnf("⚠ member history: %s shares no membership id with %s (%d and %d members) — EMAIL_HASH_SALT likely differed when one was synced; re-run `chb members sync --month %s --force` then `chb generate %s --force`",
			months[i], months[i-1], len(prev), len(cur), months[i], months[i])
	}
	return flagged
}

// dedupeHistoryMonths keeps one entry per month, preferring an observed month
// over a reconstructed one when a member's aliases disagree.
func dedupeHistoryMonths(months []MemberHistoryMonth) []MemberHistoryMonth {
	best := map[string]MemberHistoryMonth{}
	order := []string{}
	for _, m := range months {
		existing, seen := best[m.Month]
		if !seen {
			best[m.Month] = m
			order = append(order, m.Month)
			continue
		}
		if existing.Derived && !m.Derived {
			best[m.Month] = m
		}
	}
	out := make([]MemberHistoryMonth, 0, len(order))
	for _, ym := range order {
		out = append(out, best[ym])
	}
	return out
}

// writeMemberIdentityIndex records which identifier belongs to which member,
// so a caller holding a Discord account id — or tomorrow a Nostr pubkey — can
// find the history without knowing the member's email.
//
// It lives beside the histories under restricted/, not in the public tree:
// that a given Discord account belongs to a member is exactly the fact we do
// not publish.
func writeMemberIdentityIndex(dataDir string, histories map[string]*MemberHistoryFile) {
	index := map[string]string{}
	for id, h := range histories {
		for _, identifier := range h.Identifiers {
			index[identifier] = id
		}
	}
	payload := struct {
		SchemaVersion int               `json:"schemaVersion"`
		GeneratedAt   string            `json:"generatedAt"`
		Identifiers   map[string]string `json:"identifiers"`
	}{
		SchemaVersion: 1,
		GeneratedAt:   time.Now().UTC().Format(time.RFC3339),
		Identifiers:   index,
	}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return
	}
	path := filepath.Join(dataDir, "latest", "generated", restrictedDirSegment, "members-index.json")
	if err := writeDataFile(path, data); err != nil {
		Warnf("⚠ member identity index: %v", err)
	}
}
