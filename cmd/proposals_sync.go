package cmd

// Fetch half of the proposals provider: mirror a Discord FORUM channel into
// providers/discord/<forumID>/. Nothing here transforms — parsing, deadline
// extraction and every generated/ write live in proposals_generate.go.
//
// Why this is not `chb messages sync`: a forum channel (type 15) has no
// messages of its own. Each post is a thread, so mirroring one means walking
// three endpoints instead of one:
//
//	GET /channels/<forum>                        → channel object + tag vocabulary
//	GET /guilds/<guild>/threads/active           → threads still open
//	GET /channels/<forum>/threads/archived/public → threads already archived (paginated)
//	GET /channels/<thread>/messages              → the post body + its discussion
//
// The message endpoint is shared with messages_sync.go (fetchMessagePage), and
// so is the rate-limit pacer.
//
// Degraded (metadata-only) mode: the guild-wide active-threads endpoint answers
// even for a forum the bot may not read, so when the forum itself is forbidden
// the sync still archives what it can see — titles, authors' ids, creation and
// last-activity times — and marks those files MetadataOnly. That is enough for
// `chb proposals` to list what is open, and nothing more: no post bodies, no
// deadlines, no decisions, and no archived (closed) threads. Granting the bot
// read access and re-running replaces those files with the full mirror.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	discordsource "github.com/CommonsHub/chb/providers/discord"
)

const (
	// proposalsForumSettingsKey names the forum in settings.discord.forums.
	proposalsForumSettingsKey = "proposals"

	// defaultProposalsForumChannelID is the Commons Hub #proposals forum.
	// Settings win: put {"forums": {"proposals": "<id>"}} under "discord" in
	// settings.json to point the command somewhere else.
	defaultProposalsForumChannelID = "1280931158254682134"

	// proposalsDefaultWindowMonths is how far back a plain
	// `chb proposals sync` walks the ARCHIVED thread list. Proposals stay
	// interesting long after the discussion goes quiet (a decision taken in
	// spring is reviewed in autumn), so the window is much wider than the
	// two-month one used for chat messages. Active threads are always
	// listed in full regardless of age. Use --history for everything.
	proposalsDefaultWindowMonths = 12
)

// errProposalsForumAccess marks the one failure an operator cannot fix from
// here: the bot has no permission on the forum AND cannot even list its threads,
// so not even metadata-only mode has anything to write. `chb proposals sync`
// reports it as the error it is; the all-provider pull downgrades it to a
// skipped row, because one missing Discord permission should not abort the
// nightly sync of every other provider.
var errProposalsForumAccess = errors.New("no access to the proposals forum")

// warnProposalsMetadataOnly explains, once a 403 has forced the degraded mode,
// exactly what the archive will and will not contain.
func warnProposalsMetadataOnly(forumID string) {
	Warnf("%s⚠ No read access to the proposals forum (%s) — archiving thread metadata only:%s\n"+
		"    titles, ids and activity dates, but no post bodies, deadlines, decisions\n"+
		"    or archived threads.\n"+
		"  ↪ Grant the bot View Channel + Read Message History on that forum "+
		"(Discord → Edit Channel → Permissions → add the bot's role) and re-run for the full mirror.",
		Fmt.Yellow, forumID, Fmt.Reset)
}

// proposalsAccessError wraps a 403 with what to actually do about it.
func proposalsAccessError(forumID string) error {
	return fmt.Errorf("%w: Discord answered 403 Missing Access for channel %s.\n"+
		"  ↪ Grant the bot View Channel + Read Message History on that forum "+
		"(Discord → Edit Channel → Permissions → add the bot's role), then re-run",
		errProposalsForumAccess, forumID)
}

// ProposalsSync mirrors the proposals forum. Returns the number of threads
// whose messages were (re-)fetched.
func ProposalsSync(args []string) (int, error) {
	if HasFlag(args, "--help", "-h", "help") {
		printProposalsSyncHelp()
		return 0, nil
	}

	settings, err := LoadSettings()
	if err != nil {
		return 0, fmt.Errorf("failed to load settings: %w", err)
	}
	token := os.Getenv("DISCORD_BOT_TOKEN")
	if token == "" {
		return 0, fmt.Errorf("DISCORD_BOT_TOKEN environment variable required")
	}

	forumID := ProposalsForumChannelID(settings)
	if override := GetOption(args, "--channel"); override != "" {
		forumID = override
	}
	if forumID == "" {
		return 0, fmt.Errorf("no proposals forum configured — set discord.forums.proposals in settings.json")
	}
	guildID := settings.Discord.GuildID
	if guildID == "" {
		return 0, fmt.Errorf("no Discord guild configured in settings.json")
	}

	force := HasFlag(args, "--force")
	_, isHistory := ResolveSinceMonth(args, "")
	now := time.Now()
	windowStart := time.Time{}
	if !isHistory {
		windowStart = now.AddDate(0, -proposalsDefaultWindowMonths, 0)
	}

	fmt.Printf("\n%s🗳  Syncing Discord proposals%s\n", Fmt.Bold, Fmt.Reset)
	fmt.Printf("%sDATA_DIR: %s%s\n", Fmt.Dim, DataDir(), Fmt.Reset)

	pacer := &discordPacer{}

	// metadataOnly starts as an explicit request and is switched on later by
	// any 403: the bot may list the forum's threads without being allowed to
	// read them, and a thread list is still worth archiving.
	metadataOnly := HasFlag(args, "--metadata-only")
	var forum discordsource.ForumChannel
	if metadataOnly {
		forum = discordsource.ForumChannel{ID: forumID, Type: discordsource.ChannelTypeGuildForum}
	} else {
		Progress("fetching forum channel")
		fetched, err := fetchForumChannel(pacer, forumID, token)
		switch {
		case err == nil:
			if fetched.Type != discordsource.ChannelTypeGuildForum {
				return 0, fmt.Errorf("channel %s (#%s) is not a forum channel (type %d) — plain text channels belong in `chb messages sync`",
					forumID, fetched.Name, fetched.Type)
			}
			forum = fetched
			fmt.Printf("%sForum: #%s (%s)%s\n\n", Fmt.Dim, forum.Name, forumID, Fmt.Reset)
		case isDiscordForbidden(err):
			metadataOnly = true
			forum = discordsource.ForumChannel{ID: forumID, Type: discordsource.ChannelTypeGuildForum}
			warnProposalsMetadataOnly(forumID)
		default:
			return 0, fmt.Errorf("fetch forum channel %s: %w", forumID, err)
		}
	}

	Progress("listing active threads")
	threads, err := fetchActiveThreads(pacer, guildID, forumID, token)
	if err != nil {
		if isDiscordForbidden(err) {
			// Not even the guild-wide listing answers: there is nothing to
			// fall back to, so this is the hard access error.
			return 0, proposalsAccessError(forumID)
		}
		return 0, fmt.Errorf("list active threads: %w", err)
	}
	activeCount := len(threads)

	if metadataOnly {
		// The archived-thread endpoint needs the very permission we are
		// missing, so closed threads stay invisible in this mode.
		if len(threads) == 0 {
			return 0, proposalsAccessError(forumID)
		}
		fmt.Printf("  %s%s open (metadata only — archived threads need read access)%s\n",
			Fmt.Dim, Pluralize(len(threads), "thread", ""), Fmt.Reset)
	} else {
		Progress("listing archived threads")
		archived, err := fetchArchivedThreads(pacer, forumID, token, windowStart)
		if err != nil {
			if isDiscordForbidden(err) {
				metadataOnly = true
				warnProposalsMetadataOnly(forumID)
			} else {
				return 0, fmt.Errorf("list archived threads: %w", err)
			}
		}
		threads = mergeThreadsByID(threads, archived)
		fmt.Printf("  %s%d threads (%d open, %d archived)%s\n",
			Fmt.Dim, len(threads), activeCount, len(threads)-activeCount, Fmt.Reset)
	}

	dataDir := DataDir()
	updated, skipped, newMessages := 0, 0, 0

	for _, thread := range threads {
		year, month, ok := threadArchiveMonth(thread)
		if !ok {
			Warnf("%s⚠ Skipping thread %s: no usable creation timestamp%s", Fmt.Yellow, thread.ID, Fmt.Reset)
			continue
		}

		cached, hasCache := readCachedThread(dataDir, forumID, thread.ID)

		var messages []DiscordMessage
		if !metadataOnly {
			if hasCache && !force && len(cached.Messages) > 0 &&
				cached.Thread.LastMessageID == thread.LastMessageID {
				skipped++
				continue
			}
			Progress(fmt.Sprintf("fetching %s", truncate(thread.Name, 40)))
			fetched, err := fetchAllThreadMessages(pacer, thread.ID, token)
			switch {
			case err == nil:
				messages = fetched
			case isDiscordForbidden(err):
				// Every thread inherits the forum's permissions, so one 403
				// here means none of them will read. Keep going in
				// metadata-only mode rather than losing the thread list.
				metadataOnly = true
				warnProposalsMetadataOnly(forumID)
			default:
				fmt.Printf("    %s✗ %s: %v%s\n", Fmt.Red, truncate(thread.Name, 40), err, Fmt.Reset)
				continue
			}
		}

		if metadataOnly {
			// Never downgrade a full archive to metadata: content already
			// mirrored under an earlier permission stays.
			if hasCache && len(cached.Messages) > 0 {
				skipped++
				continue
			}
			if hasCache && !force && cached.MetadataOnly &&
				cached.Thread.LastMessageID == thread.LastMessageID {
				skipped++
				continue
			}
		}

		if hasCache {
			if delta := len(messages) - len(cached.Messages); delta > 0 {
				newMessages += delta
			}
		} else {
			newMessages += len(messages)
		}

		cache := discordsource.ThreadCacheFile{
			Thread:       thread,
			Messages:     messages,
			MetadataOnly: len(messages) == 0 && metadataOnly,
			CachedAt:     time.Now().UTC().Format(time.RFC3339),
			ChannelID:    forumID,
			ThreadID:     thread.ID,
		}
		data, err := json.MarshalIndent(cache, "", "  ")
		if err != nil {
			return updated, err
		}
		relPath := discordsource.ThreadRelPath(forumID, thread.ID)
		if err := writeMonthFile(dataDir, year, month, relPath, data); err != nil {
			fmt.Printf("    %s✗ Failed to write %s: %v%s\n", Fmt.Red, thread.ID, err, Fmt.Reset)
			continue
		}
		updated++
		detail := Pluralize(len(messages), "message", "")
		if cache.MetadataOnly {
			detail = "metadata only"
		}
		fmt.Printf("    %s✓ %s%s %s(%s)%s\n",
			Fmt.Green, truncate(thread.Name, 50), Fmt.Reset,
			Fmt.Dim, detail, Fmt.Reset)
	}

	// Forum snapshot: the channel object carries the tag vocabulary the
	// generator needs to turn applied_tags ids into names. It is current
	// state, not month-scoped, so it only lives in latest/.
	snapshot := discordsource.ForumCacheFile{
		Channel:      forum,
		Threads:      threads,
		MetadataOnly: metadataOnly,
		CachedAt:     time.Now().UTC().Format(time.RFC3339),
		ChannelID:    forumID,
	}
	if data, err := json.MarshalIndent(snapshot, "", "  "); err == nil {
		path := filepath.Join(dataDir, "latest", discordsource.ForumRelPath(forumID))
		if err := writeDataFile(path, data); err != nil {
			Warnf("%s⚠ Failed to write forum snapshot: %v%s", Fmt.Yellow, err, Fmt.Reset)
		}
	}

	if metadataOnly {
		fmt.Printf("\n%s✓ Done!%s %s updated (metadata only), %d unchanged\n\n",
			Fmt.Green, Fmt.Reset, Pluralize(updated, "thread", ""), skipped)
	} else {
		fmt.Printf("\n%s✓ Done!%s %s updated (%s), %d unchanged\n\n",
			Fmt.Green, Fmt.Reset, Pluralize(updated, "thread", ""),
			Pluralize(newMessages, "new message", ""), skipped)
	}
	if updated > 0 {
		fmt.Printf("  %s↪ To rebuild the proposals index: chb proposals generate%s\n\n", Fmt.Dim, Fmt.Reset)
	}
	UpdateSyncSource("proposals", isHistory)
	UpdateSyncActivity(isHistory)
	return updated, nil
}

// ProposalsForumChannelID resolves the forum channel to mirror: settings win,
// the Commons Hub forum is the fallback.
func ProposalsForumChannelID(settings *Settings) string {
	if settings != nil {
		if id := settings.Discord.Forums[proposalsForumSettingsKey]; id != "" {
			return id
		}
	}
	return defaultProposalsForumChannelID
}

// threadArchiveMonth returns the YYYY, MM the thread is archived under: the
// month it was created, in Brussels time. A thread keeps its folder for life —
// later replies do not move it.
func threadArchiveMonth(thread discordsource.Thread) (year, month string, ok bool) {
	created, ok := thread.CreatedAt()
	if !ok {
		return "", "", false
	}
	created = created.In(BrusselsTZ())
	return created.Format("2006"), created.Format("01"), true
}

// readCachedThread finds a thread's archived file. The lookup goes through
// latest/, which writeMonthFile mirrors on every write, so it does not have to
// know which month the thread was filed under.
func readCachedThread(dataDir, forumID, threadID string) (discordsource.ThreadCacheFile, bool) {
	path := filepath.Join(dataDir, "latest", discordsource.ThreadRelPath(forumID, threadID))
	data, err := os.ReadFile(path)
	if err != nil {
		return discordsource.ThreadCacheFile{}, false
	}
	var cache discordsource.ThreadCacheFile
	if json.Unmarshal(data, &cache) != nil {
		return discordsource.ThreadCacheFile{}, false
	}
	return cache, true
}

// mergeThreadsByID unions two thread lists (later wins on conflict) and
// returns them newest-first.
func mergeThreadsByID(lists ...[]discordsource.Thread) []discordsource.Thread {
	byID := map[string]discordsource.Thread{}
	for _, list := range lists {
		for _, t := range list {
			byID[t.ID] = t
		}
	}
	out := make([]discordsource.Thread, 0, len(byID))
	for _, t := range byID {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return snowflakeLess(out[j].ID, out[i].ID) })
	return out
}

func fetchForumChannel(p *discordPacer, channelID, token string) (discordsource.ForumChannel, error) {
	var forum discordsource.ForumChannel
	resp, err := p.get(fmt.Sprintf("%s/channels/%s", discordAPIBase, channelID), token)
	if err != nil {
		return forum, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return forum, fmt.Errorf("Discord API error: %d", resp.StatusCode)
	}
	err = json.NewDecoder(resp.Body).Decode(&forum)
	return forum, err
}

// fetchActiveThreads lists the guild's non-archived threads and keeps the ones
// belonging to this forum. Discord has no per-channel active-thread endpoint;
// the guild-wide one returns everything in a single response.
func fetchActiveThreads(p *discordPacer, guildID, forumID, token string) ([]discordsource.Thread, error) {
	resp, err := p.get(fmt.Sprintf("%s/guilds/%s/threads/active", discordAPIBase, guildID), token)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("Discord API error: %d", resp.StatusCode)
	}
	var payload struct {
		Threads []discordsource.Thread `json:"threads"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, err
	}
	var out []discordsource.Thread
	for _, t := range payload.Threads {
		if t.ParentID == forumID {
			out = append(out, t)
		}
	}
	return out, nil
}

// fetchArchivedThreads pages through the forum's archived threads, newest
// archive first. Pagination is by archive timestamp, not by snowflake. Stops
// once a page is entirely older than stopBefore (zero = no limit).
func fetchArchivedThreads(p *discordPacer, forumID, token string, stopBefore time.Time) ([]discordsource.Thread, error) {
	var all []discordsource.Thread
	before := ""
	for {
		url := fmt.Sprintf("%s/channels/%s/threads/archived/public?limit=100", discordAPIBase, forumID)
		if before != "" {
			url += "&before=" + before
		}
		resp, err := p.get(url, token)
		if err != nil {
			return all, err
		}
		var payload struct {
			Threads []discordsource.Thread `json:"threads"`
			HasMore bool                   `json:"has_more"`
		}
		status := resp.StatusCode
		if status != 200 {
			resp.Body.Close()
			return all, fmt.Errorf("Discord API error: %d", status)
		}
		err = json.NewDecoder(resp.Body).Decode(&payload)
		resp.Body.Close()
		if err != nil {
			return all, err
		}
		if len(payload.Threads) == 0 {
			return all, nil
		}

		reachedWindow := false
		for _, t := range payload.Threads {
			if !stopBefore.IsZero() {
				if ts, ok := threadArchiveTime(t); ok && ts.Before(stopBefore) {
					reachedWindow = true
					continue
				}
			}
			all = append(all, t)
		}
		last := payload.Threads[len(payload.Threads)-1]
		if reachedWindow || !payload.HasMore {
			return all, nil
		}
		next := last.ThreadMetadata.ArchiveTimestamp
		if next == "" || next == before {
			// No cursor to advance on: stop rather than loop forever.
			return all, nil
		}
		before = next
	}
}

// isDiscordForbidden reports whether err is the "Discord API error: 403" the
// fetch helpers return when the bot may not see a channel.
func isDiscordForbidden(err error) bool {
	return err != nil && strings.Contains(err.Error(), "Discord API error: 403")
}

func threadArchiveTime(t discordsource.Thread) (time.Time, bool) {
	ts := t.ThreadMetadata.ArchiveTimestamp
	if ts == "" {
		return time.Time{}, false
	}
	parsed, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return time.Time{}, false
	}
	return parsed, true
}

// fetchAllThreadMessages pages a thread to its first message. A thread is one
// proposal, so partial history is not useful — the opening post IS the
// proposal and it is the oldest message in the thread.
func fetchAllThreadMessages(p *discordPacer, threadID, token string) ([]DiscordMessage, error) {
	var all []DiscordMessage
	before := ""
	for {
		page, err := fetchMessagePage(p, threadID, token, before)
		if err != nil {
			return nil, err
		}
		if len(page) == 0 {
			return all, nil
		}
		all = append(all, page...)
		if len(page) < 100 {
			return all, nil
		}
		before = page[len(page)-1].ID
	}
}

func printProposalsSyncHelp() {
	f := Fmt
	fmt.Printf(`
%schb proposals sync%s — Mirror the Discord proposals forum

%sUSAGE%s
  %schb proposals sync%s [options]

%sOPTIONS%s
  %s--channel%s <id>     Mirror a different forum channel
  %s--history%s          List every archived thread, not just the recent window
  %s--since%s <date>     Same as --history (any date lists the full archive)
  %s--force%s            Re-fetch every thread's messages, even unchanged ones
  %s--metadata-only%s    Archive the thread list without reading any content
  %s--help, -h%s         Show this help

%sBEHAVIOR%s
  A forum channel keeps its content in threads, so this command walks the
  thread list rather than a message list. Open threads are always listed in
  full; archived threads are listed %d months back by default.

  Raw data is archived per thread, under the month the thread was created:
    DATA_DIR/YYYY/MM/providers/discord/<forumId>/threads/<threadId>.json
    DATA_DIR/latest/providers/discord/<forumId>/forum.json

  A thread whose last message is unchanged since the last sync is skipped
  without fetching — a routine sync costs one request per new reply.

  %sMetadata-only mode%s: if the bot may list the forum's threads but not read
  them (403 Missing Access), the sync degrades instead of failing. It archives
  titles, ids, authors' ids and activity dates, marks those files
  metadataOnly, and says so. There are no post bodies, deadlines, decisions or
  archived threads in that mode, and an existing full archive is never
  downgraded to it. Grant the bot View Channel + Read Message History and
  re-run for the complete mirror.

  This command only reads from Discord. Nothing is ever posted back.

%sCONFIGURATION%s
  settings.json → discord.forums.proposals (defaults to the Commons Hub forum)

%sENVIRONMENT%s
  %sDISCORD_BOT_TOKEN%s   Discord bot token (configure via chb setup)

%sEXAMPLES%s
  %schb proposals sync%s                  Mirror new activity
  %schb proposals sync --history%s        Mirror the full archive
  %schb proposals sync --force%s          Re-fetch every thread
  %schb proposals sync --metadata-only%s  Thread list only, read nothing
  %schb proposals%s                       List the proposals afterwards
`,
		f.Bold, f.Reset,
		f.Bold, f.Reset,
		f.Cyan, f.Reset,
		f.Bold, f.Reset,
		f.Yellow, f.Reset,
		f.Yellow, f.Reset,
		f.Yellow, f.Reset,
		f.Yellow, f.Reset,
		f.Yellow, f.Reset,
		f.Yellow, f.Reset,
		f.Bold, f.Reset,
		proposalsDefaultWindowMonths,
		f.Bold, f.Reset,
		f.Bold, f.Reset,
		f.Bold, f.Reset,
		f.Yellow, f.Reset,
		f.Bold, f.Reset,
		f.Cyan, f.Reset,
		f.Cyan, f.Reset,
		f.Cyan, f.Reset,
		f.Cyan, f.Reset,
		f.Cyan, f.Reset,
	)
}

// proposalsForumName returns the mirrored forum's channel name (from the
// snapshot), falling back to the settings key.
func proposalsForumName(dataDir, forumID string) string {
	path := filepath.Join(dataDir, "latest", discordsource.ForumRelPath(forumID))
	data, err := os.ReadFile(path)
	if err != nil {
		return proposalsForumSettingsKey
	}
	var snapshot discordsource.ForumCacheFile
	if json.Unmarshal(data, &snapshot) != nil || strings.TrimSpace(snapshot.Channel.Name) == "" {
		return proposalsForumSettingsKey
	}
	return snapshot.Channel.Name
}
