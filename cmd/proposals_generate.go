package cmd

// Generate half of the proposals provider: read the raw forum archive written
// by proposals_sync.go and produce the proposals index. Every bit of parsing,
// timezone conversion and enrichment lives here — the sync file only downloads.
//
// Output (cross-month, like the member histories — a proposal is a thread that
// outlives the month it was opened in):
//
//	latest/generated/restricted/proposals.json
//	latest/generated/restricted/proposals.md
//
// restricted/, not the public tree: a proposal carries the names of the members
// who wrote it, argued about it and agreed to it. Nothing here is meant for the
// public website.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	discordsource "github.com/CommonsHub/chb/providers/discord"
)

// proposalReviewAfterMonths is how long after a decision a proposal is due for
// a look-back ("we agreed this in spring — did it work?").
const proposalReviewAfterMonths = 3

// Proposal is one forum thread, parsed.
type Proposal struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	URL      string `json:"url"`
	Author   string `json:"author,omitempty"`
	AuthorID string `json:"authorId,omitempty"`

	CreatedAt      string `json:"createdAt"`      // RFC3339, Brussels offset
	LastActivityAt string `json:"lastActivityAt"` // RFC3339, Brussels offset

	Status   string `json:"status"`             // open | agreed | rejected | withdrawn
	Archived bool   `json:"archived,omitempty"` // thread closed on Discord
	Locked   bool   `json:"locked,omitempty"`

	// AgreedAt is when the decision landed: the message that carries the
	// decision, or the thread's archive time when only a tag says so.
	AgreedAt string `json:"agreedAt,omitempty"`
	// ReviewDue is AgreedAt + proposalReviewAfterMonths. Set for agreed
	// proposals only — it is what `chb proposals --review` sorts on.
	ReviewDue string `json:"reviewDue,omitempty"`

	// Deadline is the date the proposal itself asks to be decided by, when the
	// opening post states one (a Discord <t:…> timestamp, or a written date).
	Deadline string `json:"deadline,omitempty"`
	// DeadlineNote is the line the deadline was read from, kept so a human can
	// check the parse without opening Discord.
	DeadlineNote string `json:"deadlineNote,omitempty"`

	Tags         []string           `json:"tags,omitempty"`
	Messages     int                `json:"messages"`
	Participants []string           `json:"participants,omitempty"`
	Reactions    []ProposalReaction `json:"reactions,omitempty"`

	Summary string `json:"summary,omitempty"` // first lines of the opening post
	Body    string `json:"body,omitempty"`    // the opening post, in full

	// MetadataOnly marks a proposal the bot could list but not read: title,
	// dates and message count are real, everything else is simply unknown —
	// an empty Deadline here means "not visible", not "none stated".
	MetadataOnly bool `json:"metadataOnly,omitempty"`
}

// ProposalReaction is one emoji tally on the opening post. In a consent-based
// process these are the votes.
type ProposalReaction struct {
	Emoji string `json:"emoji"`
	Count int    `json:"count"`
}

// ProposalsFile is generated/restricted/proposals.json.
type ProposalsFile struct {
	GeneratedAt string     `json:"generatedAt"`
	ChannelID   string     `json:"channelId"`
	Channel     string     `json:"channel,omitempty"`
	Proposals   []Proposal `json:"proposals"`
}

const (
	ProposalStatusOpen      = "open"
	ProposalStatusAgreed    = "agreed"
	ProposalStatusRejected  = "rejected"
	ProposalStatusWithdrawn = "withdrawn"
)

// GenerateProposals rebuilds the proposals index from the forum archive.
// Local-only: it never touches the network.
func GenerateProposals(args []string) error {
	if HasFlag(args, "--help", "-h", "help") {
		printProposalsGenerateHelp()
		return nil
	}

	settings, err := LoadSettings()
	if err != nil {
		return fmt.Errorf("failed to load settings: %w", err)
	}
	forumID := ProposalsForumChannelID(settings)
	if override := GetOption(args, "--channel"); override != "" {
		forumID = override
	}
	dataDir := DataDir()

	threads := readArchivedProposalThreads(dataDir, forumID)
	if len(threads) == 0 {
		// Not a warning: a hub that never mirrored the forum (or has no
		// access to it yet) should not get a ⚠ on every `chb generate`.
		fmt.Printf("  %sNo proposals archived yet — run `chb proposals sync` first.%s\n", Fmt.Dim, Fmt.Reset)
		return nil
	}

	tagNames := readForumTagNames(dataDir, forumID)
	displayNames := readDiscordDisplayNames(dataDir)
	guildID := settings.Discord.GuildID

	proposals := make([]Proposal, 0, len(threads))
	for _, t := range threads {
		proposals = append(proposals, buildProposal(t, tagNames, displayNames, guildID))
	}
	// Newest activity first: the list command's default order.
	sort.Slice(proposals, func(i, j int) bool {
		if proposals[i].LastActivityAt != proposals[j].LastActivityAt {
			return proposals[i].LastActivityAt > proposals[j].LastActivityAt
		}
		return proposals[i].ID > proposals[j].ID
	})

	out := ProposalsFile{
		GeneratedAt: time.Now().In(BrusselsTZ()).Format(time.RFC3339),
		ChannelID:   forumID,
		Channel:     proposalsForumName(dataDir, forumID),
		Proposals:   proposals,
	}
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	if err := writeDataFile(proposalsJSONPath(dataDir), data); err != nil {
		return err
	}
	if err := writeDataFile(proposalsMarkdownPath(dataDir), []byte(renderProposalsMarkdown(out))); err != nil {
		return err
	}

	open, agreed, metaOnly := 0, 0, 0
	for _, p := range proposals {
		switch p.Status {
		case ProposalStatusOpen:
			open++
		case ProposalStatusAgreed:
			agreed++
		}
		if p.MetadataOnly {
			metaOnly++
		}
	}
	fmt.Printf("%s✓ Proposals:%s %s (%d open, %d agreed) → %s\n",
		Fmt.Green, Fmt.Reset, Pluralize(len(proposals), "proposal", ""), open, agreed,
		filepath.ToSlash(filepath.Join("latest", "generated", restrictedDirSegment, "proposals.json")))
	if metaOnly > 0 {
		fmt.Printf("  %s%s metadata-only (no read access to the forum)%s\n", Fmt.Dim, Pluralize(metaOnly, "proposal", ""), Fmt.Reset)
	}
	return nil
}

func proposalsJSONPath(dataDir string) string {
	return filepath.Join(dataDir, "latest", "generated", restrictedDirSegment, "proposals.json")
}

func proposalsMarkdownPath(dataDir string) string {
	return filepath.Join(dataDir, "latest", "generated", restrictedDirSegment, "proposals.md")
}

// readArchivedProposalThreads collects every archived thread file for the
// forum across all months. The monthly archive is canonical; latest/ is only
// its mirror, so the walk goes over YYYY/MM.
func readArchivedProposalThreads(dataDir, forumID string) []discordsource.ThreadCacheFile {
	var out []discordsource.ThreadCacheFile
	seen := map[string]bool{}
	for _, year := range getAvailableYears(dataDir) {
		for _, month := range getAvailableMonths(dataDir, year) {
			dir := discordsource.ThreadsDirPath(dataDir, year, month, forumID)
			entries, err := os.ReadDir(dir)
			if err != nil {
				continue
			}
			for _, e := range entries {
				if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
					continue
				}
				data, err := os.ReadFile(filepath.Join(dir, e.Name()))
				if err != nil {
					continue
				}
				var cache discordsource.ThreadCacheFile
				if json.Unmarshal(data, &cache) != nil || cache.ThreadID == "" {
					continue
				}
				if seen[cache.ThreadID] {
					continue
				}
				seen[cache.ThreadID] = true
				out = append(out, cache)
			}
		}
	}
	return out
}

// readDiscordDisplayNames maps Discord user ids to display names, using the
// channel mirrors `chb messages sync` already wrote. It is the only way to put
// a name on a metadata-only proposal: the thread object carries owner_id and
// nothing else, and looking the user up over the network is not this command's
// job (generate never fetches).
func readDiscordDisplayNames(dataDir string) map[string]string {
	names := map[string]string{}
	pattern := filepath.Join(dataDir, "latest", "providers", "discord", "*", discordsource.MessagesFile)
	files, err := filepath.Glob(pattern)
	if err != nil {
		return names
	}
	for _, path := range files {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var cache discordsource.CacheFile
		if json.Unmarshal(data, &cache) != nil {
			continue
		}
		for _, m := range cache.Messages {
			if m.Author.ID == "" {
				continue
			}
			if name := strings.TrimSpace(discordDisplayName(m.Author)); name != "" {
				names[m.Author.ID] = name
			}
		}
	}
	return names
}

// readForumTagNames maps forum tag ids to their names, so applied_tags stops
// being a list of snowflakes. Empty when the forum snapshot is missing.
func readForumTagNames(dataDir, forumID string) map[string]string {
	names := map[string]string{}
	path := filepath.Join(dataDir, "latest", discordsource.ForumRelPath(forumID))
	data, err := os.ReadFile(path)
	if err != nil {
		return names
	}
	var snapshot discordsource.ForumCacheFile
	if json.Unmarshal(data, &snapshot) != nil {
		return names
	}
	for _, tag := range snapshot.Channel.AvailableTags {
		names[tag.ID] = tag.Name
	}
	return names
}

func buildProposal(cache discordsource.ThreadCacheFile, tagNames, displayNames map[string]string, guildID string) Proposal {
	tz := BrusselsTZ()
	thread := cache.Thread

	// The archive keeps Discord's own order (newest first); the proposal is
	// the OLDEST message, so work on an oldest-first copy.
	msgs := append([]DiscordMessage(nil), cache.Messages...)
	sort.Slice(msgs, func(i, j int) bool { return snowflakeLess(msgs[i].ID, msgs[j].ID) })

	p := Proposal{
		ID:           thread.ID,
		Title:        strings.TrimSpace(thread.Name),
		URL:          proposalURL(guildID, thread.ID),
		Status:       ProposalStatusOpen,
		Archived:     thread.ThreadMetadata.Archived,
		Locked:       thread.ThreadMetadata.Locked,
		Messages:     len(msgs),
		MetadataOnly: cache.MetadataOnly && len(msgs) == 0,
	}
	if p.MetadataOnly {
		// Discord's own count, since we cannot count the messages ourselves.
		p.Messages = thread.MessageCount
		if p.Messages == 0 {
			p.Messages = thread.TotalMessageSent
		}
		p.AuthorID = thread.OwnerID
		p.Author = displayNames[thread.OwnerID]
	}
	if created, ok := thread.CreatedAt(); ok {
		p.CreatedAt = created.In(tz).Format(time.RFC3339)
	}
	for _, id := range thread.AppliedTags {
		if name := tagNames[id]; name != "" {
			p.Tags = append(p.Tags, name)
		}
	}

	if len(msgs) > 0 {
		opening := msgs[0]
		p.Author = strings.TrimSpace(discordDisplayName(opening.Author))
		p.AuthorID = opening.Author.ID
		p.Body = strings.TrimSpace(opening.Content)
		p.Summary = summarizeProposalBody(p.Body)
		for _, r := range opening.Reactions {
			p.Reactions = append(p.Reactions, ProposalReaction{Emoji: r.Emoji.Name, Count: r.Count})
		}
		if p.CreatedAt == "" {
			if t, ok := parseDiscordTime(opening.Timestamp); ok {
				p.CreatedAt = t.In(tz).Format(time.RFC3339)
			}
		}
		if deadline, note, ok := parseProposalDeadline(opening.Content); ok {
			p.Deadline = deadline
			p.DeadlineNote = note
		}
	}
	// The thread owner is authoritative when the opening post was deleted.
	if p.AuthorID == "" {
		p.AuthorID = thread.OwnerID
	}

	// Participants in the order they first spoke — the opening author first.
	seen := map[string]bool{}
	for _, m := range msgs {
		name := strings.TrimSpace(discordDisplayName(m.Author))
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		p.Participants = append(p.Participants, name)
	}

	if len(msgs) > 0 {
		if t, ok := parseDiscordTime(msgs[len(msgs)-1].Timestamp); ok {
			p.LastActivityAt = t.In(tz).Format(time.RFC3339)
		}
	} else if t, ok := thread.LastActivityAt(); ok {
		// Metadata-only: the last message's snowflake still dates the thread's
		// most recent activity, which is what the list command sorts on.
		p.LastActivityAt = t.In(tz).Format(time.RFC3339)
	}
	if p.LastActivityAt == "" {
		p.LastActivityAt = p.CreatedAt
	}

	applyProposalDecision(&p, thread, msgs, tz)
	return p
}

// applyProposalDecision resolves the proposal's status. A forum tag is the
// deliberate signal and always wins; a decision sentence in the discussion is
// the fallback for threads nobody tagged.
func applyProposalDecision(p *Proposal, thread discordsource.Thread, msgs []DiscordMessage, tz *time.Location) {
	if status, ok := proposalStatusFromTags(p.Tags); ok {
		p.Status = status
		if status == ProposalStatusAgreed {
			p.AgreedAt = proposalDecisionTime(msgs, thread, tz)
		}
	} else if status, at, ok := proposalStatusFromMessages(msgs, tz); ok {
		p.Status = status
		if status == ProposalStatusAgreed {
			p.AgreedAt = at
		}
	}

	if p.Status == ProposalStatusAgreed && p.AgreedAt != "" {
		if t, err := time.Parse(time.RFC3339, p.AgreedAt); err == nil {
			p.ReviewDue = t.AddDate(0, proposalReviewAfterMonths, 0).Format(time.RFC3339)
		}
	}
}

// proposalStatusFromTags reads the forum's own tag vocabulary. Matching is on
// substrings so "✅ agreed", "Agreed", "accepted" and "Decision taken" all land
// on the same status without hardcoding one server's exact tag names.
func proposalStatusFromTags(tags []string) (string, bool) {
	for _, tag := range tags {
		t := strings.ToLower(tag)
		switch {
		case strings.Contains(t, "agree"), strings.Contains(t, "accept"),
			strings.Contains(t, "approv"), strings.Contains(t, "adopt"),
			strings.Contains(t, "consent"), strings.Contains(t, "decided"),
			strings.Contains(t, "implement"), strings.Contains(t, "done"):
			return ProposalStatusAgreed, true
		case strings.Contains(t, "reject"), strings.Contains(t, "declin"),
			strings.Contains(t, "object"), strings.Contains(t, "not approved"):
			return ProposalStatusRejected, true
		case strings.Contains(t, "withdraw"), strings.Contains(t, "cancel"),
			strings.Contains(t, "abandon"):
			return ProposalStatusWithdrawn, true
		}
	}
	return "", false
}

var (
	// A decision sentence, not a mention of the word: "we agreed to …",
	// "this is now approved", "consent reached". Kept deliberately narrow —
	// "I agree" from one member is an opinion, not a decision, so the phrases
	// all read as a statement about the proposal.
	proposalAgreedRe    = regexp.MustCompile(`(?i)\b(we (have )?(agreed|decided)|agreement reached|consent (reached|given)|no objections?( (were )?raised)?|proposal (is )?(accepted|approved|adopted)|approved by consent|decision:? (agreed|accepted|approved))\b`)
	proposalRejectedRe  = regexp.MustCompile(`(?i)\b(proposal (is )?(rejected|declined|not approved)|we (have )?(rejected|declined)|decision:? (rejected|declined))\b`)
	proposalWithdrawnRe = regexp.MustCompile(`(?i)\b(withdraw(n|ing)? (this|the) proposal|proposal (is )?withdrawn|i withdraw this)\b`)
)

// proposalStatusFromMessages scans the discussion for an explicit decision.
// The LAST matching message wins — a proposal reopened after a "no objections"
// note should read as whatever the thread said most recently.
func proposalStatusFromMessages(msgs []DiscordMessage, tz *time.Location) (status, at string, ok bool) {
	for _, m := range msgs {
		content := m.Content
		switch {
		case proposalWithdrawnRe.MatchString(content):
			status, ok = ProposalStatusWithdrawn, true
		case proposalRejectedRe.MatchString(content):
			status, ok = ProposalStatusRejected, true
		case proposalAgreedRe.MatchString(content):
			status, ok = ProposalStatusAgreed, true
		default:
			continue
		}
		if t, parsed := parseDiscordTime(m.Timestamp); parsed {
			at = t.In(tz).Format(time.RFC3339)
		}
	}
	return status, at, ok
}

// proposalDecisionTime dates a tag-driven decision. A tag carries no timestamp
// of its own, so: the message that states the decision, else the moment the
// thread was archived, else its last activity.
func proposalDecisionTime(msgs []DiscordMessage, thread discordsource.Thread, tz *time.Location) string {
	if _, at, ok := proposalStatusFromMessages(msgs, tz); ok && at != "" {
		return at
	}
	if ts := thread.ThreadMetadata.ArchiveTimestamp; ts != "" {
		if t, err := time.Parse(time.RFC3339Nano, ts); err == nil {
			return t.In(tz).Format(time.RFC3339)
		}
	}
	if len(msgs) > 0 {
		if t, ok := parseDiscordTime(msgs[len(msgs)-1].Timestamp); ok {
			return t.In(tz).Format(time.RFC3339)
		}
	}
	return ""
}

var (
	// Discord's own timestamp markup: <t:1757000000:F>. Unambiguous, so it is
	// the preferred deadline source.
	discordTimestampRe = regexp.MustCompile(`<t:(\d+)(?::[tTdDfFR])?>`)
	// A line that announces a deadline in words.
	deadlineLineRe = regexp.MustCompile(`(?i)(deadline|decide by|decision by|closes? on|closing|respond by|reply by|objections? by|until)\b`)
	// Written dates, in the forms members actually type.
	isoDateRe   = regexp.MustCompile(`\b(\d{4})-(\d{2})-(\d{2})\b`)
	dmyDateRe   = regexp.MustCompile(`\b(\d{1,2})[./](\d{1,2})[./](\d{4})\b`)
	dayMonthRe  = regexp.MustCompile(`(?i)\b(\d{1,2})(?:st|nd|rd|th)?\s+(january|february|march|april|may|june|july|august|september|october|november|december)\b(?:\s+(\d{4}))?`)
	monthDayRe  = regexp.MustCompile(`(?i)\b(january|february|march|april|may|june|july|august|september|october|november|december)\s+(\d{1,2})(?:st|nd|rd|th)?\b(?:,?\s+(\d{4}))?`)
	monthNumber = map[string]time.Month{
		"january": time.January, "february": time.February, "march": time.March,
		"april": time.April, "may": time.May, "june": time.June,
		"july": time.July, "august": time.August, "september": time.September,
		"october": time.October, "november": time.November, "december": time.December,
	}
)

// parseProposalDeadline finds the date the opening post asks to be decided by.
// Returns the date (YYYY-MM-DD, Brussels) and the line it was read from, so a
// reader can sanity-check the parse.
//
// Only lines that ANNOUNCE a deadline are considered — a proposal that happens
// to mention "the potluck on 12 September" must not turn that into a deadline.
func parseProposalDeadline(body string) (deadline, note string, ok bool) {
	tz := BrusselsTZ()
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || !deadlineLineRe.MatchString(trimmed) {
			continue
		}
		if m := discordTimestampRe.FindStringSubmatch(trimmed); m != nil {
			if secs, err := strconv.ParseInt(m[1], 10, 64); err == nil {
				return time.Unix(secs, 0).In(tz).Format("2006-01-02"), trimmed, true
			}
		}
		if d, found := parseWrittenDate(trimmed, tz); found {
			return d, trimmed, true
		}
	}
	return "", "", false
}

// parseWrittenDate pulls the first date out of a line. A day+month without a
// year is read as the next such date at or after the line's own context is
// unknown — so it defaults to the year of the date's own month relative to
// now, which keeps "objections by 12 September" meaningful in either half of
// the year.
func parseWrittenDate(line string, tz *time.Location) (string, bool) {
	if m := isoDateRe.FindStringSubmatch(line); m != nil {
		if t, err := time.ParseInLocation("2006-01-02", m[0], tz); err == nil {
			return t.Format("2006-01-02"), true
		}
	}
	if m := dmyDateRe.FindStringSubmatch(line); m != nil {
		day, _ := strconv.Atoi(m[1])
		month, _ := strconv.Atoi(m[2])
		year, _ := strconv.Atoi(m[3])
		if month >= 1 && month <= 12 && day >= 1 && day <= 31 {
			return time.Date(year, time.Month(month), day, 0, 0, 0, 0, tz).Format("2006-01-02"), true
		}
	}
	if m := dayMonthRe.FindStringSubmatch(line); m != nil {
		return assembleDate(m[1], m[2], m[3], tz)
	}
	if m := monthDayRe.FindStringSubmatch(line); m != nil {
		return assembleDate(m[2], m[1], m[3], tz)
	}
	return "", false
}

func assembleDate(dayStr, monthName, yearStr string, tz *time.Location) (string, bool) {
	day, err := strconv.Atoi(dayStr)
	if err != nil || day < 1 || day > 31 {
		return "", false
	}
	month, ok := monthNumber[strings.ToLower(monthName)]
	if !ok {
		return "", false
	}
	year := time.Now().In(tz).Year()
	if yearStr != "" {
		if y, err := strconv.Atoi(yearStr); err == nil {
			year = y
		}
	}
	return time.Date(year, month, day, 0, 0, 0, 0, tz).Format("2006-01-02"), true
}

// parseDiscordTime accepts both timestamp shapes Discord has emitted.
func parseDiscordTime(ts string) (time.Time, bool) {
	if ts == "" {
		return time.Time{}, false
	}
	if t, err := time.Parse(time.RFC3339Nano, ts); err == nil {
		return t, true
	}
	if t, err := time.Parse("2006-01-02T15:04:05+00:00", ts); err == nil {
		return t, true
	}
	return time.Time{}, false
}

func proposalURL(guildID, threadID string) string {
	if guildID == "" {
		return ""
	}
	return fmt.Sprintf("https://discord.com/channels/%s/%s", guildID, threadID)
}

// summarizeProposalBody keeps the opening lines of a post, with Discord's
// markup left intact (it is what the author wrote) but mentions and custom
// emoji reduced to something readable in a terminal.
func summarizeProposalBody(body string) string {
	cleaned := cleanDiscordMarkup(body)
	fields := strings.Fields(cleaned)
	const maxWords = 45
	if len(fields) > maxWords {
		return strings.Join(fields[:maxWords], " ") + "…"
	}
	return strings.Join(fields, " ")
}

var (
	customEmojiRe = regexp.MustCompile(`<a?:(\w+):\d+>`)
	mentionRe     = regexp.MustCompile(`<@[!&]?\d+>`)
	channelRefRe  = regexp.MustCompile(`<#\d+>`)
)

func cleanDiscordMarkup(s string) string {
	s = customEmojiRe.ReplaceAllString(s, ":$1:")
	s = mentionRe.ReplaceAllString(s, "@someone")
	s = channelRefRe.ReplaceAllString(s, "#channel")
	s = discordTimestampRe.ReplaceAllStringFunc(s, func(m string) string {
		sub := discordTimestampRe.FindStringSubmatch(m)
		secs, err := strconv.ParseInt(sub[1], 10, 64)
		if err != nil {
			return m
		}
		return time.Unix(secs, 0).In(BrusselsTZ()).Format("2 Jan 2006 15:04")
	})
	return s
}

func renderProposalsMarkdown(f ProposalsFile) string {
	var b strings.Builder
	title := f.Channel
	if title == "" {
		title = "proposals"
	}
	fmt.Fprintf(&b, "# Proposals — #%s\n\n", title)
	fmt.Fprintf(&b, "_Generated %s. %s._\n\n",
		f.GeneratedAt, Pluralize(len(f.Proposals), "proposal", ""))

	sections := []struct {
		heading string
		status  string
	}{
		{"Open", ProposalStatusOpen},
		{"Agreed", ProposalStatusAgreed},
		{"Rejected", ProposalStatusRejected},
		{"Withdrawn", ProposalStatusWithdrawn},
	}
	for _, section := range sections {
		var rows []Proposal
		for _, p := range f.Proposals {
			if p.Status == section.status {
				rows = append(rows, p)
			}
		}
		if len(rows) == 0 {
			continue
		}
		fmt.Fprintf(&b, "## %s (%d)\n\n", section.heading, len(rows))
		for _, p := range rows {
			fmt.Fprintf(&b, "### %s\n\n", p.Title)
			fmt.Fprintf(&b, "- Opened %s by %s\n", markdownDate(p.CreatedAt), fallbackText(p.Author, "unknown"))
			fmt.Fprintf(&b, "- Last activity %s · %s\n", markdownDate(p.LastActivityAt), Pluralize(p.Messages, "message", ""))
			if p.Deadline != "" {
				fmt.Fprintf(&b, "- Deadline **%s** (%s)\n", p.Deadline, p.DeadlineNote)
			}
			if p.AgreedAt != "" {
				fmt.Fprintf(&b, "- Agreed %s", markdownDate(p.AgreedAt))
				if p.ReviewDue != "" {
					fmt.Fprintf(&b, " · review due %s", markdownDate(p.ReviewDue))
				}
				b.WriteString("\n")
			}
			if len(p.Tags) > 0 {
				fmt.Fprintf(&b, "- Tags: %s\n", strings.Join(p.Tags, ", "))
			}
			if len(p.Reactions) > 0 {
				var parts []string
				for _, r := range p.Reactions {
					parts = append(parts, fmt.Sprintf("%s %d", r.Emoji, r.Count))
				}
				fmt.Fprintf(&b, "- Reactions: %s\n", strings.Join(parts, "  "))
			}
			if p.URL != "" {
				fmt.Fprintf(&b, "- [Open in Discord](%s)\n", p.URL)
			}
			if p.MetadataOnly {
				fmt.Fprintf(&b, "- _Metadata only: the bot cannot read this forum, so there is no post body, deadline or decision here._\n")
			}
			if p.Summary != "" {
				fmt.Fprintf(&b, "\n%s\n", p.Summary)
			}
			b.WriteString("\n")
		}
	}
	return b.String()
}

func markdownDate(rfc3339 string) string {
	t, err := time.Parse(time.RFC3339, rfc3339)
	if err != nil {
		return rfc3339
	}
	return t.In(BrusselsTZ()).Format("2 Jan 2006")
}

func fallbackText(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}

func printProposalsGenerateHelp() {
	f := Fmt
	fmt.Printf(`
%schb proposals generate%s — Rebuild the proposals index from the local archive

%sUSAGE%s
  %schb proposals generate%s [options]

%sOPTIONS%s
  %s--channel%s <id>     Generate from a different mirrored forum
  %s--help, -h%s         Show this help

%sOUTPUT%s
  DATA_DIR/latest/generated/restricted/proposals.json
  DATA_DIR/latest/generated/restricted/proposals.md

  restricted/, because a proposal names the members who wrote, discussed and
  agreed it. Local-only: this command never calls Discord.

%sEXAMPLES%s
  %schb proposals generate%s            Rebuild after a sync
  %schb proposals --review%s            Proposals agreed ~%d months ago
`,
		f.Bold, f.Reset,
		f.Bold, f.Reset,
		f.Cyan, f.Reset,
		f.Bold, f.Reset,
		f.Yellow, f.Reset,
		f.Yellow, f.Reset,
		f.Bold, f.Reset,
		f.Bold, f.Reset,
		f.Cyan, f.Reset,
		f.Cyan, f.Reset,
		proposalReviewAfterMonths,
	)
}
