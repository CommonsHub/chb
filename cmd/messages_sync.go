package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	discordsource "github.com/CommonsHub/chb/providers/discord"
)

// var (not const) so tests can point it at a local fake.
var discordAPIBase = "https://discord.com/api/v10"

type DiscordMessage = discordsource.Message
type DiscordAuthor = discordsource.Author
type DiscordAttachment = discordsource.Attachment
type DiscordReaction = discordsource.Reaction
type DiscordEmoji = discordsource.Emoji
type MessagesCacheFile = discordsource.CacheFile

func MessagesSync(args []string) (int, error) {
	if HasFlag(args, "--help", "-h", "help") {
		printMessagesSyncHelp()
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

	force := HasFlag(args, "--force")
	monthFilter := GetOption(args, "--month")
	channelFilter := GetOption(args, "--channel")

	// Positional date/month/year range arg (e.g. "2025", "2025/03", "2025/Q1")
	posStartMonth, posEndMonth, posFound := ParseMonthRangeArg(args)
	monthStart, monthEnd := "", ""
	if monthFilter != "" {
		var ok bool
		monthStart, monthEnd, ok = ParseMonthRangeValue(monthFilter)
		if !ok {
			return 0, fmt.Errorf("invalid --month value %q (expected %s)", monthFilter, DateRangeFormatHelp)
		}
	}

	// Check --since / --history
	resolvedSince, isSince := ResolveSinceMonth(args, "messages")
	now := time.Now()
	recentStartMonth := DefaultRecentStartMonth(now)
	defaultRecentWindow := !isSince && !posFound && monthFilter == ""

	// Populate the guild-name cache so the sync header can render the
	// server name (e.g. "CommonsHub") instead of the numeric ID. Cheap
	// no-op on subsequent runs.
	_ = FetchAndCacheDiscordGuildName(settings.Discord.GuildID)

	fmt.Printf("\n%s💬 Syncing Discord messages%s\n", Fmt.Bold, Fmt.Reset)
	fmt.Printf("%sDATA_DIR: %s%s\n", Fmt.Dim, DataDir(), Fmt.Reset)
	guildLabel := settings.Discord.GuildID
	if name := DiscordGuildName(settings.Discord.GuildID); name != "" {
		guildLabel = name + " (" + settings.Discord.GuildID + ")"
	}
	fmt.Printf("%sGuild: %s%s\n\n", Fmt.Dim, guildLabel, Fmt.Reset)

	// Get all channel IDs from settings
	channels := GetDiscordChannelIDs(settings)
	if len(channels) == 0 {
		return 0, fmt.Errorf("no Discord channels configured in settings.json")
	}

	totalMessages := 0
	for name, channelID := range channels {
		if channelFilter != "" && channelID != channelFilter && name != channelFilter {
			continue
		}

		fmt.Printf("  #%s (%s)\n", name, channelID)
		Progress(fmt.Sprintf("fetching #%s", name))

		var messages []DiscordMessage
		var err error

		if isSince {
			// --history or --since: paginate backwards
			stopMonth := ""
			if !force {
				stopMonth = findOldestCachedMonthForChannel(channelID)
			}
			messages, err = fetchAllChannelMessages(channelID, token, stopMonth)
		} else if defaultRecentWindow {
			// Incremental: stop paginating at the first already-cached
			// message and merge with the existing window instead of
			// re-downloading the whole window every sync. A quiet channel
			// costs one request. Falls back to the full window pull when
			// nothing is cached yet (first sync of a channel).
			windowMonths := monthsInWindow(recentStartMonth, now)
			if newestCached := newestCachedMessageID(channelID, windowMonths); newestCached != "" {
				var fetched []DiscordMessage
				fetched, err = fetchMessagesSinceCached(channelID, token, newestCached)
				if err == nil {
					var existing []DiscordMessage
					for _, ym := range windowMonths {
						parts := strings.Split(ym, "-")
						existing = append(existing, readCachedChannelMonth(channelID, parts[0], parts[1])...)
					}
					newCount := 0
					for _, m := range fetched {
						if snowflakeLess(newestCached, m.ID) {
							newCount++
						}
					}
					messages = mergeMessagesByID(existing, fetched)
					if newCount == 0 {
						// Nothing new: skip the rewrite entirely — no reason
						// to touch mtimes and re-trigger generate.
						fmt.Printf("    %sup to date%s\n", Fmt.Dim, Fmt.Reset)
						continue
					}
					fmt.Printf("    %s%d new%s\n", Fmt.Dim, newCount, Fmt.Reset)
				}
			} else {
				messages, err = fetchAllChannelMessages(channelID, token, recentStartMonth)
			}
		} else {
			// Explicit month/year filters only need the latest page unless the user
			// requested a broader historical sync.
			messages, err = fetchLatestMessages(channelID, token)
		}

		if err != nil {
			fmt.Printf("    %s✗ Error: %v%s\n", Fmt.Red, err, Fmt.Reset)
			continue
		}

		fmt.Printf("    %sFetched %d messages%s\n", Fmt.Dim, len(messages), Fmt.Reset)

		// Group by month
		byMonth := groupMessagesByMonth(messages)

		// Determine which months to save.
		// Even quick syncs should persist every month represented in the fetched page,
		// so latest/ and the canonical YYYY/MM cache stay aligned.
		var monthsToSave []string
		for ym := range byMonth {
			monthsToSave = append(monthsToSave, ym)
		}
		sort.Strings(monthsToSave)

		saved := 0
		for _, ym := range monthsToSave {
			monthMsgs := byMonth[ym]

			if monthFilter != "" && (ym < monthStart || ym > monthEnd) {
				continue
			}
			if defaultRecentWindow && ym < recentStartMonth {
				continue
			}
			// --since / --history filter
			if isSince && ym < resolvedSince {
				continue
			}
			// Positional year/month filter
			if posFound && (ym < posStartMonth || ym > posEndMonth) {
				continue
			}

			parts := strings.Split(ym, "-")
			if len(parts) != 2 {
				continue
			}
			year, month := parts[0], parts[1]

			// Save to data/YYYY/MM/providers/discord/{channelId}/messages.json
			dataDir := DataDir()
			relPath := discordsource.ChannelRelPath(channelID)

			cache := MessagesCacheFile{
				Messages:  monthMsgs,
				CachedAt:  time.Now().UTC().Format(time.RFC3339),
				ChannelID: channelID,
			}

			data, _ := json.MarshalIndent(cache, "", "  ")
			if err := writeMonthFile(dataDir, year, month, relPath, data); err != nil {
				fmt.Printf("    %s✗ Failed to write: %v%s\n", Fmt.Red, err, Fmt.Reset)
				continue
			}

			saved++
			totalMessages += len(monthMsgs)
		}

		if saved > 0 {
			fmt.Printf("    %s✓ Saved %d months%s\n", Fmt.Green, saved, Fmt.Reset)
		}

		// Write ALL fetched messages to latest/ (the full batch, not split by month).
		// This ensures latest/ has every message the API returned for this channel.
		if len(messages) > 0 {
			dataDir := DataDir()
			relPath := discordsource.ChannelRelPath(channelID)
			cache := MessagesCacheFile{
				Messages:  messages,
				CachedAt:  time.Now().UTC().Format(time.RFC3339),
				ChannelID: channelID,
			}
			data, _ := json.MarshalIndent(cache, "", "  ")
			latestPath := filepath.Join(dataDir, "latest", relPath)
			if err := writeDataFile(latestPath, data); err != nil {
				fmt.Printf("    %s✗ Failed to update latest cache: %v%s\n", Fmt.Red, err, Fmt.Reset)
			}
		}

		// No fixed inter-channel sleep: the pacer reads the rate-limit
		// headers and waits exactly when Discord says the bucket is empty.
	}

	fmt.Printf("\n%s✓ Done!%s %d messages synced\n\n", Fmt.Green, Fmt.Reset, totalMessages)
	UpdateSyncSource("messages", isSince)
	UpdateSyncActivity(isSince)
	return totalMessages, nil
}

// fetchMessagePage fetches one page (up to 100 messages, newest first) from a
// Discord channel through the rate-limit pacer. before="" starts at the top.
func fetchMessagePage(p *discordPacer, channelID, token, before string) ([]DiscordMessage, error) {
	url := fmt.Sprintf("%s/channels/%s/messages?limit=100", discordAPIBase, channelID)
	if before != "" {
		url += "&before=" + before
	}
	resp, err := p.get(url, token)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("Discord API error: %d", resp.StatusCode)
	}
	var messages []DiscordMessage
	if err := json.NewDecoder(resp.Body).Decode(&messages); err != nil {
		return nil, err
	}
	return messages, nil
}

// fetchLatestMessages fetches one page (100 messages) from a Discord channel.
// No pagination — used for quick sync of latest data.
func fetchLatestMessages(channelID, token string) ([]DiscordMessage, error) {
	return fetchMessagePage(&discordPacer{}, channelID, token, "")
}

// fetchMessagesSinceCached paginates backwards only until it meets a message
// we already have, and returns everything fetched (newest first).
//
// This is what keeps a routine `chb sync` cheap: a quiet channel costs exactly
// ONE request (its newest page overlaps the cache immediately) instead of
// re-downloading the whole recent window every run. The overlap page is
// returned in full on purpose — merging it refreshes reactions/edits on the
// ~100 most recent messages.
func fetchMessagesSinceCached(channelID, token, newestCachedID string) ([]DiscordMessage, error) {
	p := &discordPacer{}
	var all []DiscordMessage
	before := ""
	for {
		page, err := fetchMessagePage(p, channelID, token, before)
		if err != nil {
			return nil, err
		}
		if len(page) == 0 {
			return all, nil
		}
		all = append(all, page...)
		for _, msg := range page {
			if !snowflakeLess(newestCachedID, msg.ID) {
				// Reached (or passed) the newest cached message: caught up.
				return all, nil
			}
		}
		before = page[len(page)-1].ID
	}
}

// mergeMessagesByID overlays fetched onto existing (fetched wins — it carries
// the current reaction counts and edits) and returns the union newest-first.
func mergeMessagesByID(existing, fetched []DiscordMessage) []DiscordMessage {
	byID := make(map[string]DiscordMessage, len(existing)+len(fetched))
	for _, m := range existing {
		byID[m.ID] = m
	}
	for _, m := range fetched {
		byID[m.ID] = m
	}
	out := make([]DiscordMessage, 0, len(byID))
	for _, m := range byID {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return snowflakeLess(out[j].ID, out[i].ID) })
	return out
}

// newestCachedMessageID returns the highest message ID cached for a channel
// in the given months ("" when nothing is cached yet).
func newestCachedMessageID(channelID string, months []string) string {
	newest := ""
	for _, ym := range months {
		parts := strings.Split(ym, "-")
		if len(parts) != 2 {
			continue
		}
		for _, msg := range readCachedChannelMonth(channelID, parts[0], parts[1]) {
			if snowflakeLess(newest, msg.ID) {
				newest = msg.ID
			}
		}
	}
	return newest
}

// readCachedChannelMonth loads one month's cached messages for a channel
// (nil when the month has no cache).
func readCachedChannelMonth(channelID, year, month string) []DiscordMessage {
	path := discordsource.ChannelPath(DataDir(), year, month, channelID)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var cache MessagesCacheFile
	if json.Unmarshal(data, &cache) != nil {
		return nil
	}
	return cache.Messages
}

// monthsInWindow lists YYYY-MM strings from startMonth to the current month.
func monthsInWindow(startMonth string, now time.Time) []string {
	var months []string
	t, err := time.Parse("2006-01", startMonth)
	if err != nil {
		return months
	}
	end := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	for !t.After(end) {
		months = append(months, t.Format("2006-01"))
		t = t.AddDate(0, 1, 0)
	}
	return months
}

// fetchAllChannelMessages fetches messages from Discord, paginating backwards.
// If stopBeforeMonth is set (e.g. "2025-06"), stop paginating once we hit messages
// older than that month (they're already cached).
func fetchAllChannelMessages(channelID, token, stopBeforeMonth string) ([]DiscordMessage, error) {
	var allMessages []DiscordMessage
	var before string
	tz := BrusselsTZ()
	pacer := &discordPacer{}

	for {
		messages, err := fetchMessagePage(pacer, channelID, token, before)
		if err != nil {
			return nil, err
		}

		if len(messages) == 0 {
			break
		}

		// Check if we've reached cached data
		hitCached := false
		if stopBeforeMonth != "" {
			for _, msg := range messages {
				t, err := time.Parse(time.RFC3339Nano, msg.Timestamp)
				if err != nil {
					t, _ = time.Parse("2006-01-02T15:04:05+00:00", msg.Timestamp)
				}
				t = t.In(tz)
				msgYM := fmt.Sprintf("%d-%02d", t.Year(), t.Month())
				if msgYM < stopBeforeMonth {
					hitCached = true
					break
				}
			}
		}

		allMessages = append(allMessages, messages...)

		if hitCached {
			fmt.Printf("    %sReached cached data at %s, stopping%s\n", Fmt.Dim, stopBeforeMonth, Fmt.Reset)
			break
		}

		before = messages[len(messages)-1].ID

		// Rate limit
		time.Sleep(300 * time.Millisecond)
	}

	return allMessages, nil
}

func groupMessagesByMonth(messages []DiscordMessage) map[string][]DiscordMessage {
	byMonth := make(map[string][]DiscordMessage)
	tz := BrusselsTZ()

	for _, msg := range messages {
		t, err := time.Parse(time.RFC3339Nano, msg.Timestamp)
		if err != nil {
			t, err = time.Parse("2006-01-02T15:04:05+00:00", msg.Timestamp)
			if err != nil {
				continue
			}
		}
		t = t.In(tz)
		ym := fmt.Sprintf("%d-%02d", t.Year(), t.Month())
		byMonth[ym] = append(byMonth[ym], msg)
	}

	// Sort messages within each month by timestamp
	for ym := range byMonth {
		sort.Slice(byMonth[ym], func(i, j int) bool {
			return byMonth[ym][i].Timestamp < byMonth[ym][j].Timestamp
		})
	}

	return byMonth
}

func printMessagesSyncHelp() {
	f := Fmt
	fmt.Printf(`
%schb messages sync%s — Fetch Discord messages

%sUSAGE%s
  %schb messages sync%s [year[/month]] [options]

%sTIME RANGE%s
  %s(no args)%s              Fetch current month + previous month
  %s<date-range>%s           Only save messages from that range (e.g. 2025/03, 2025/Q1)
  %s--since%s <date>         Only save messages from that date onward
  %s--history%s              Paginate backwards, stop at oldest cached month

%sFILTERING%s
  %s--channel%s <id|name>    Fetch a specific channel only
  %s--month%s <date-range>   Alias for date-range positional arg

%sOPTIONS%s
  %s--force%s                Re-fetch and overwrite cached months
  %s--help, -h%s             Show this help

%sBEHAVIOR%s
  Messages are fetched from newest to oldest (Discord API pagination).
  Each page returns 100 messages. Source data is saved per month to:
    DATA_DIR/YYYY/MM/providers/discord/{channelId}/messages.json

  %s--history%s: paginates backwards until hitting a month with cached
  data, then stops. Saves everything from that point forward.
  Use %s--history --force%s to re-fetch and overwrite all cached months.

  If a sync fails mid-way (e.g. network error), re-run with:
    chb messages sync --channel <id> --force

%sENVIRONMENT%s
  %sDISCORD_BOT_TOKEN%s      Discord bot token (configure via chb setup)

%sEXAMPLES%s
  %schb messages sync%s                             Fetch all channels for the recent 2-month window
  %schb messages sync --history%s                   Fetch new messages since last sync
  %schb messages sync --channel general%s           Fetch only #general
  %schb messages sync --channel 129796 --force%s    Re-fetch a specific channel
  %schb messages sync --since 2024/06%s             Save messages from Jun 2024 onward
  %schb messages sync 2025%s                        Save only 2025 messages
`,
		f.Bold, f.Reset,
		f.Bold, f.Reset,
		f.Cyan, f.Reset,
		f.Bold, f.Reset,
		f.Dim, f.Reset,
		f.Yellow, f.Reset,
		f.Yellow, f.Reset,
		f.Yellow, f.Reset,
		f.Bold, f.Reset,
		f.Yellow, f.Reset,
		f.Yellow, f.Reset,
		f.Bold, f.Reset,
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
		f.Cyan, f.Reset,
		f.Cyan, f.Reset,
		f.Cyan, f.Reset,
	)
}

// findOldestCachedMonthForChannel finds the oldest month that has cached
// Discord messages for any channel. Used as a stop point during pagination.
func findOldestCachedMonthForChannel(channelID string) string {
	dataDir := DataDir()
	oldest := ""

	years, err := os.ReadDir(dataDir)
	if err != nil {
		return ""
	}

	for _, yd := range years {
		if !yd.IsDir() || len(yd.Name()) != 4 {
			continue
		}
		year := yd.Name()
		if _, err := strconv.Atoi(year); err != nil {
			continue
		}

		months, _ := os.ReadDir(filepath.Join(dataDir, year))
		for _, md := range months {
			if !md.IsDir() || len(md.Name()) != 2 {
				continue
			}
			month := md.Name()

			msgPath := discordsource.ChannelPath(dataDir, year, month, channelID)
			if _, err := os.Stat(msgPath); err == nil {
				ym := year + "-" + month
				if oldest == "" || ym < oldest {
					oldest = ym
				}
			}
		}
	}

	return oldest
}
