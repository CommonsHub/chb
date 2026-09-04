package cmd

// Read side of the proposals provider: list what is in the generated index.
// Offline-first — this reads latest/generated/restricted/proposals.json and
// never calls Discord. When the index is missing it says which command to run.

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// What "this week" means for `--week`: activity in the last 7 days, or a
// deadline inside the next 14 (a deadline that lands early next week is
// already this week's business).
const (
	proposalsWeekActivityDays = 7
	proposalsWeekDeadlineDays = 14
)

// ProposalsList prints the proposals index.
func ProposalsList(args []string) error {
	if HasFlag(args, "--help", "-h", "help") {
		printProposalsHelp()
		return nil
	}

	dataDir := DataDir()
	file, err := loadProposalsFile(dataDir)
	if err != nil {
		return err
	}

	now := time.Now().In(BrusselsTZ())
	proposals := filterProposals(file.Proposals, args, now)

	if HasFlag(args, "--json") || GetOption(args, "--format") == "json" {
		out := ProposalsFile{
			GeneratedAt: file.GeneratedAt,
			ChannelID:   file.ChannelID,
			Channel:     file.Channel,
			Proposals:   proposals,
		}
		data, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(data))
		return nil
	}

	// A single proposal (by id, or by a search term matching exactly one)
	// prints in full rather than as a table row.
	if len(proposals) == 1 && proposalsQuery(args) != "" {
		printProposalDetail(proposals[0], now)
		return nil
	}

	printProposalsTable(file, proposals, args, now)
	return nil
}

func loadProposalsFile(dataDir string) (ProposalsFile, error) {
	var file ProposalsFile
	path := proposalsJSONPath(dataDir)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return file, fmt.Errorf("no proposals index found at %s.\n  ↪ Run `chb proposals sync` then `chb proposals generate`", path)
		}
		return file, err
	}
	if err := json.Unmarshal(data, &file); err != nil {
		return file, fmt.Errorf("proposals index at %s is not readable: %w", path, err)
	}
	return file, nil
}

// proposalsQuery returns the free-text argument (a thread id, or words from a
// title), "" when the caller only passed flags.
func proposalsQuery(args []string) string {
	var words []string
	skipNext := false
	for _, a := range args {
		if skipNext {
			skipNext = false
			continue
		}
		if strings.HasPrefix(a, "--") {
			// Flags that take a value consume the next argument.
			switch a {
			case "--format", "--channel", "--review":
				skipNext = true
			}
			continue
		}
		words = append(words, a)
	}
	return strings.TrimSpace(strings.Join(words, " "))
}

func filterProposals(all []Proposal, args []string, now time.Time) []Proposal {
	query := strings.ToLower(proposalsQuery(args))
	showAll := HasFlag(args, "--all")
	onlyAgreed := HasFlag(args, "--agreed")
	onlyOpen := HasFlag(args, "--open")
	week := HasFlag(args, "--week")
	deadlines := HasFlag(args, "--deadline", "--deadlines")
	review := HasFlag(args, "--review")

	reviewMonths := proposalReviewAfterMonths
	if v := GetOption(args, "--review"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			reviewMonths = n
		}
	}

	var out []Proposal
	for _, p := range all {
		if query != "" && !proposalMatchesQuery(p, query) {
			continue
		}
		switch {
		case review:
			// Agreed long enough ago to be worth a look back, and not so long
			// ago that it is ancient history: [months, months+2).
			if p.Status != ProposalStatusAgreed || p.AgreedAt == "" {
				continue
			}
			agreed, ok := parseRFC3339(p.AgreedAt)
			if !ok {
				continue
			}
			from := now.AddDate(0, -reviewMonths-2, 0)
			until := now.AddDate(0, -reviewMonths, 0)
			if agreed.Before(from) || agreed.After(until) {
				continue
			}
		case week:
			if p.Status != ProposalStatusOpen {
				continue
			}
			// A proposal whose deadline has passed is not this week's
			// business, however recently someone posted in it.
			if d, ok := proposalDeadlineDate(p); ok && d.Before(startOfDay(now)) {
				continue
			}
			recent := false
			if last, ok := parseRFC3339(p.LastActivityAt); ok &&
				now.Sub(last) <= time.Duration(proposalsWeekActivityDays)*24*time.Hour {
				recent = true
			}
			if d, ok := proposalDeadlineDate(p); ok &&
				d.Sub(startOfDay(now)) <= time.Duration(proposalsWeekDeadlineDays)*24*time.Hour {
				recent = true
			}
			if !recent {
				continue
			}
		case deadlines:
			d, ok := proposalDeadlineDate(p)
			if !ok || d.Before(startOfDay(now)) {
				continue
			}
		case onlyAgreed:
			if p.Status != ProposalStatusAgreed {
				continue
			}
		case onlyOpen:
			if p.Status != ProposalStatusOpen {
				continue
			}
		case showAll || query != "":
			// no status filter
		default:
			// Default view: what still needs the community's attention.
			if p.Status != ProposalStatusOpen {
				continue
			}
		}
		out = append(out, p)
	}

	switch {
	case review:
		// Oldest decision first — the one most overdue for a review.
		sort.Slice(out, func(i, j int) bool { return out[i].AgreedAt < out[j].AgreedAt })
	case deadlines:
		sort.Slice(out, func(i, j int) bool { return out[i].Deadline < out[j].Deadline })
	default:
		sort.Slice(out, func(i, j int) bool { return out[i].LastActivityAt > out[j].LastActivityAt })
	}
	return out
}

func proposalMatchesQuery(p Proposal, query string) bool {
	if p.ID == query {
		return true
	}
	return strings.Contains(strings.ToLower(p.Title), query) ||
		strings.Contains(strings.ToLower(p.Summary), query)
}

func proposalDeadlineDate(p Proposal) (time.Time, bool) {
	if p.Deadline == "" {
		return time.Time{}, false
	}
	t, err := time.ParseInLocation("2006-01-02", p.Deadline, BrusselsTZ())
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

func parseRFC3339(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, false
	}
	return t.In(BrusselsTZ()), true
}

func startOfDay(t time.Time) time.Time {
	tz := BrusselsTZ()
	t = t.In(tz)
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, tz)
}

func printProposalsTable(file ProposalsFile, proposals []Proposal, args []string, now time.Time) {
	channel := file.Channel
	if channel == "" {
		channel = "proposals"
	}
	heading := "Open proposals"
	switch {
	case HasFlag(args, "--review"):
		heading = fmt.Sprintf("Proposals to review (agreed ~%s ago)", reviewLabel(args))
	case HasFlag(args, "--week"):
		heading = "Proposals needing attention this week"
	case HasFlag(args, "--deadline", "--deadlines"):
		heading = "Upcoming proposal deadlines"
	case HasFlag(args, "--agreed"):
		heading = "Agreed proposals"
	case HasFlag(args, "--all"):
		heading = "All proposals"
	}

	fmt.Printf("\n%s🗳  %s%s %s(#%s)%s\n", Fmt.Bold, heading, Fmt.Reset, Fmt.Dim, channel, Fmt.Reset)
	if len(proposals) == 0 {
		fmt.Printf("\n  %sNothing to show.%s\n\n", Fmt.Dim, Fmt.Reset)
		return
	}
	fmt.Println()

	type row struct{ status, color, when, title, who, msgs string }
	rows := make([]row, 0, len(proposals))
	metaOnly := 0
	for _, p := range proposals {
		status, color := proposalStatusLabel(p, now)
		title := Truncate(p.Title, 52)
		if p.MetadataOnly {
			metaOnly++
			// The row is real, its blanks are not: mark it so nobody reads a
			// missing deadline as "this proposal has none".
			title = Truncate(p.Title, 50) + " *"
		}
		rows = append(rows, row{
			status: status,
			color:  color,
			when:   proposalWhenLabel(p, args, now),
			title:  title,
			who:    Truncate(fallbackText(p.Author, "—"), 16),
			msgs:   strconv.Itoa(p.Messages),
		})
	}
	statusW, whenW, titleW, whoW := 0, 0, 0, 0
	for _, r := range rows {
		statusW = Max(statusW, displayWidth(r.status))
		whenW = Max(whenW, displayWidth(r.when))
		titleW = Max(titleW, displayWidth(r.title))
		whoW = Max(whoW, displayWidth(r.who))
	}
	for _, r := range rows {
		fmt.Printf("  %s%s%s  %s  %s  %s%s  %s msg%s\n",
			r.color, padDisplay(r.status, statusW), Fmt.Reset,
			padDisplay(r.when, whenW),
			padDisplay(r.title, titleW),
			Fmt.Dim, padDisplay(r.who, whoW),
			r.msgs, Fmt.Reset)
	}
	fmt.Printf("\n  %s%s%s\n", Fmt.Dim, Pluralize(len(proposals), "proposal", ""), Fmt.Reset)
	if metaOnly > 0 {
		fmt.Printf("  %s* %s metadata-only: the bot cannot read this forum, so titles and dates\n"+
			"    are real but bodies, deadlines and decisions are unknown.%s\n",
			Fmt.Yellow, Pluralize(metaOnly, "proposal", ""), Fmt.Reset)
	}
	fmt.Printf("\n  %s↪ Details: chb proposals <id|words from the title>%s\n\n", Fmt.Dim, Fmt.Reset)
}

func reviewLabel(args []string) string {
	months := proposalReviewAfterMonths
	if v := GetOption(args, "--review"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			months = n
		}
	}
	return Pluralize(months, "month", "")
}

// proposalStatusLabel returns the status word and the colour to print it in,
// kept apart so column widths are measured on the text, not on ANSI escapes.
func proposalStatusLabel(p Proposal, now time.Time) (label, color string) {
	switch p.Status {
	case ProposalStatusAgreed:
		return "agreed", Fmt.Green
	case ProposalStatusRejected:
		return "rejected", Fmt.Red
	case ProposalStatusWithdrawn:
		return "withdrawn", Fmt.Dim
	}
	if d, ok := proposalDeadlineDate(p); ok && d.Before(startOfDay(now)) {
		return "overdue", Fmt.Yellow
	}
	return "open", Fmt.Cyan
}

// proposalWhenLabel picks the date that matters for the current view: the
// deadline when there is one to meet, the review date in review mode, the last
// activity otherwise.
func proposalWhenLabel(p Proposal, args []string, now time.Time) string {
	if HasFlag(args, "--review") {
		if t, ok := parseRFC3339(p.AgreedAt); ok {
			return "agreed " + t.Format("2 Jan")
		}
	}
	if d, ok := proposalDeadlineDate(p); ok {
		days := int(d.Sub(startOfDay(now)).Hours() / 24)
		switch {
		case days < 0:
			return fmt.Sprintf("due %s (%dd ago)", d.Format("2 Jan"), -days)
		case days == 0:
			return fmt.Sprintf("due %s (today)", d.Format("2 Jan"))
		default:
			return fmt.Sprintf("due %s (%dd)", d.Format("2 Jan"), days)
		}
	}
	if t, ok := parseRFC3339(p.LastActivityAt); ok {
		return "active " + t.Format("2 Jan")
	}
	return ""
}

func printProposalDetail(p Proposal, now time.Time) {
	fmt.Printf("\n%s%s%s\n", Fmt.Bold, p.Title, Fmt.Reset)
	fmt.Printf("%s%s%s\n\n", Fmt.Dim, p.URL, Fmt.Reset)

	status, statusColor := proposalStatusLabel(p, now)
	fmt.Printf("  Status        %s%s%s\n", statusColor, status, Fmt.Reset)
	if t, ok := parseRFC3339(p.CreatedAt); ok {
		fmt.Printf("  Opened        %s by %s\n", FormatDateLong(t), fallbackText(p.Author, "unknown"))
	}
	if t, ok := parseRFC3339(p.LastActivityAt); ok {
		fmt.Printf("  Last activity %s (%s)\n", FormatDateLong(t), Pluralize(p.Messages, "message", ""))
	}
	if p.Deadline != "" {
		fmt.Printf("  Deadline      %s%s%s  %s(%s)%s\n", Fmt.Yellow, p.Deadline, Fmt.Reset, Fmt.Dim, p.DeadlineNote, Fmt.Reset)
	}
	if t, ok := parseRFC3339(p.AgreedAt); ok {
		fmt.Printf("  Agreed        %s\n", FormatDateLong(t))
	}
	if t, ok := parseRFC3339(p.ReviewDue); ok {
		fmt.Printf("  Review due    %s\n", FormatDateLong(t))
	}
	if len(p.Tags) > 0 {
		fmt.Printf("  Tags          %s\n", strings.Join(p.Tags, ", "))
	}
	if len(p.Reactions) > 0 {
		var parts []string
		for _, r := range p.Reactions {
			parts = append(parts, fmt.Sprintf("%s %d", r.Emoji, r.Count))
		}
		fmt.Printf("  Reactions     %s\n", strings.Join(parts, "  "))
	}
	if len(p.Participants) > 0 {
		fmt.Printf("  Participants  %s\n", strings.Join(p.Participants, ", "))
	}
	if p.Body != "" {
		fmt.Printf("\n%s\n", cleanDiscordMarkup(p.Body))
	}
	if p.MetadataOnly {
		fmt.Printf("\n  %sMetadata only — the bot has no read access to this forum, so the post\n"+
			"  body, its deadline and any decision are not mirrored. Grant it View Channel\n"+
			"  + Read Message History and re-run `chb proposals sync`.%s\n", Fmt.Yellow, Fmt.Reset)
	}
	fmt.Println()
}

// padDisplay pads to a printable width, ignoring ANSI colour codes.
func padDisplay(s string, width int) string {
	pad := width - displayWidth(s)
	if pad <= 0 {
		return s
	}
	return s + strings.Repeat(" ", pad)
}

func printProposalsHelp() {
	f := Fmt
	fmt.Printf(`
%schb proposals%s — Proposals from the Discord forum

%sUSAGE%s
  %schb proposals%s [query] [options]
  %schb proposals sync%s [options]
  %schb proposals generate%s [options]

%sVIEWS%s
  %s(no args)%s          Open proposals, most recent activity first
  %s<query>%s            One proposal in full (thread id, or words from the title)
  %s--week%s             Needs attention this week: recent activity or a
                     deadline within %d days (past deadlines are dropped)
  %s--deadline%s         Upcoming deadlines only, soonest first
  %s--review%s [months]  Agreed ~%d months ago and due a look back
  %s--agreed%s           Agreed proposals
  %s--all%s              Every proposal, whatever its status

%sOPTIONS%s
  %s--json%s             Machine-readable output
  %s--help, -h%s         Show this help

%sBEHAVIOR%s
  Reads DATA_DIR/latest/generated/restricted/proposals.json — offline, like
  every other read command. If it is missing, sync then generate first.

  A title marked %s*%s is metadata-only: the bot could list the thread but not
  read it, so its title and dates are real while its body, deadline and
  decision are simply unknown.

  A proposal is one thread in the Discord forum channel. Its status comes from
  the forum's own tags when the thread has them, and from an explicit decision
  sentence in the discussion when it does not.

%sEXAMPLES%s
  %schb proposals%s                      What is open right now
  %schb proposals --week%s               This week's business
  %schb proposals --review%s             Agreed ~%d months ago — time to review
  %schb proposals --review 6%s           Agreed ~6 months ago
  %schb proposals furniture%s            One proposal in full
`,
		f.Bold, f.Reset,
		f.Bold, f.Reset,
		f.Cyan, f.Reset,
		f.Cyan, f.Reset,
		f.Cyan, f.Reset,
		f.Bold, f.Reset,
		f.Dim, f.Reset,
		f.Yellow, f.Reset,
		f.Yellow, f.Reset,
		proposalsWeekDeadlineDays,
		f.Yellow, f.Reset,
		f.Yellow, f.Reset,
		proposalReviewAfterMonths,
		f.Yellow, f.Reset,
		f.Yellow, f.Reset,
		f.Bold, f.Reset,
		f.Yellow, f.Reset,
		f.Yellow, f.Reset,
		f.Bold, f.Reset,
		f.Yellow, f.Reset,
		f.Bold, f.Reset,
		f.Cyan, f.Reset,
		f.Cyan, f.Reset,
		f.Cyan, f.Reset,
		proposalReviewAfterMonths,
		f.Cyan, f.Reset,
		f.Cyan, f.Reset,
	)
}
