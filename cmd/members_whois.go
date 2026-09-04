package cmd

import (
	"fmt"
	"strings"
)

// MembersWhois resolves an email address (or a membership id) to the member
// behind it, using the same salted hash the website computes from a signed-in
// user's session.
//
// It answers the two questions that come up in practice: "why can't this
// person see their membership" — usually because they subscribed under a
// different address than the one on their Discord account — and "what is this
// person's id", so it can be pasted into settings/funders.json.
//
// The salt itself is never printed. The id is: it is a one-way hash, it is
// already in the generated files, and it is the thing the operator needs.
func MembersWhois(args []string) error {
	if len(args) == 0 || HasFlag(args, "--help", "-h", "help") {
		printMembersWhoisHelp()
		return nil
	}

	var query string
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			query = strings.TrimSpace(a)
			break
		}
	}
	if query == "" {
		printMembersWhoisHelp()
		return nil
	}

	memberID, err := membershipIDForQuery(args, query)
	if err != nil {
		return err
	}

	fmt.Printf("\n%s%s%s\n", Fmt.Bold, query, Fmt.Reset)
	fmt.Printf("  %sMembership id:%s %s\n", Fmt.Dim, Fmt.Reset, memberID)

	path, ok := MemberHistoryPath(DataDir(), memberID)
	if !ok {
		return fmt.Errorf("not a usable membership id: %s", memberID)
	}

	history, found := loadMemberHistory(path)
	if !found {
		fmt.Printf("  %sNo membership history on file.%s\n", Fmt.Yellow, Fmt.Reset)
		fmt.Printf("  %sEither this address is not a member, or they subscribed under a\n", Fmt.Dim)
		fmt.Printf("  different one. History starts at %s; run `chb members sync` and\n", membershipHistoryStartMonth)
		fmt.Printf("  `chb generate` if it should be there.%s\n\n", Fmt.Reset)
		return nil
	}

	name := history.FirstName
	if name == "" {
		name = "(no name on file)"
	}
	fmt.Printf("  %sMember:%s %s\n", Fmt.Dim, Fmt.Reset, name)
	if history.Discord != "" {
		fmt.Printf("  %sDiscord:%s %s\n", Fmt.Dim, Fmt.Reset, history.Discord)
	}
	if history.CreatedAt != "" {
		fmt.Printf("  %sJoined:%s %s\n", Fmt.Dim, Fmt.Reset, history.CreatedAt)
	}
	fmt.Printf("  %s%s on file", Fmt.Dim, Pluralize(history.MonthsActive, "month", ""))
	if history.FirstMonth != "" {
		fmt.Printf(" (%s → %s)", history.FirstMonth, history.LastMonth)
	}
	fmt.Printf("%s\n\n", Fmt.Reset)

	for _, m := range history.Months {
		marker := ""
		if m.Derived {
			marker = " *"
		}
		fmt.Printf("    %s  %-10s %-8s %s%s\n",
			m.Month, m.Status, m.Source, fmtNumber(m.Amount.Value)+" "+m.Amount.Currency, marker)
	}
	if historyHasDerived(history) {
		fmt.Printf("\n    %s* reconstructed from a later snapshot, not recorded at the time%s\n",
			Fmt.Dim, Fmt.Reset)
	}
	fmt.Println()
	return nil
}

// membershipIDForQuery turns the argument into a membership id: a 64-hex value
// is taken as an id already, anything else is treated as an email address and
// hashed. Hashing needs the salt; looking up an id does not, which is why an
// id is accepted at all.
func membershipIDForQuery(args []string, query string) (string, error) {
	if emailHashPattern.MatchString(strings.ToLower(query)) {
		return strings.ToLower(query), nil
	}
	salt, err := resolveEmailHashSalt(args)
	if err != nil {
		return "", err
	}
	return hashEmail(query, salt), nil
}

func historyHasDerived(h *MemberHistoryFile) bool {
	for _, m := range h.Months {
		if m.Derived {
			return true
		}
	}
	return false
}

func printMembersWhoisHelp() {
	f := Fmt
	fmt.Printf(`
%schb members whois%s — Look up the member behind an email address

%sUSAGE%s
  %schb members whois%s <email>
  %schb members whois%s <membership-id>

%sWHAT IT DOES%s
  Hashes the address the same way the website hashes a signed-in user's, then
  reports the membership on file for it. Use it when someone says they cannot
  see their membership — the usual cause is that they subscribed under a
  different address than the one on their Discord account.

  The membership id it prints is what goes in %ssettings/funders.json%s for a
  funder paid outside Stripe and Odoo.

%sNOTES%s
  • Needs %sEMAIL_HASH_SALT%s to hash an address. A membership id can be looked
    up without it.
  • Reads only local files — run %schb members sync%s and %schb generate%s first
    for current data.
`,
		f.Bold, f.Reset,
		f.Bold, f.Reset,
		f.Cyan, f.Reset,
		f.Cyan, f.Reset,
		f.Bold, f.Reset,
		f.Cyan, f.Reset,
		f.Bold, f.Reset,
		f.Yellow, f.Reset,
		f.Cyan, f.Reset, f.Cyan, f.Reset,
	)
}
