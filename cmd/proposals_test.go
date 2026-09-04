package cmd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	discordsource "github.com/CommonsHub/chb/providers/discord"
)

func proposalMsg(id, ts, content string, author discordsource.Author) discordsource.Message {
	return discordsource.Message{ID: id, Author: author, Content: content, Timestamp: ts}
}

func TestParseProposalDeadline(t *testing.T) {
	// 2026-09-15 12:00 Brussels.
	stamp := time.Date(2026, 9, 15, 12, 0, 0, 0, BrusselsTZ()).Unix()

	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "discord timestamp markup wins",
			body: "Let's repot the plants.\nDeadline for objections: <t:" + strconv.FormatInt(stamp, 10) + ":F>",
			want: "2026-09-15",
		},
		{"iso date", "Deadline: 2026-09-15 please react before then", "2026-09-15"},
		{"day month year", "Objections by 15 September 2026 at the latest", "2026-09-15"},
		{"month day year", "Decide by September 15, 2026", "2026-09-15"},
		{"numeric dmy", "deadline 15/09/2026", "2026-09-15"},
		{
			// A date that is part of the proposal's content, not a deadline,
			// must not be picked up.
			name: "date without a deadline word is ignored",
			body: "Let's hold the repotting day on 15 September 2026 in the garden.",
			want: "",
		},
		{"no dates at all", "We should buy a new kettle.", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, note, ok := parseProposalDeadline(tc.body)
			if tc.want == "" {
				if ok {
					t.Fatalf("parsed %q from %q, want no deadline", got, tc.body)
				}
				return
			}
			if !ok {
				t.Fatalf("no deadline parsed from %q", tc.body)
			}
			if got != tc.want {
				t.Errorf("deadline = %q, want %q", got, tc.want)
			}
			if note == "" {
				t.Error("expected the source line to be kept for review")
			}
		})
	}
}

func TestProposalStatusFromTags(t *testing.T) {
	cases := map[string]string{
		"✅ Agreed":      ProposalStatusAgreed,
		"accepted":      ProposalStatusAgreed,
		"Decision made": "",
		"decided":       ProposalStatusAgreed,
		"Rejected":      ProposalStatusRejected,
		"withdrawn":     ProposalStatusWithdrawn,
		"in discussion": "",
	}
	for tag, want := range cases {
		got, ok := proposalStatusFromTags([]string{tag})
		if want == "" {
			if ok {
				t.Errorf("tag %q → %q, want no status", tag, got)
			}
			continue
		}
		if !ok || got != want {
			t.Errorf("tag %q → %q (ok=%v), want %q", tag, got, ok, want)
		}
	}
}

func TestProposalStatusFromMessagesNeedsADecisionSentence(t *testing.T) {
	author := discordsource.Author{ID: "1", Username: "alice"}
	tz := BrusselsTZ()

	// A single member's opinion is not a decision.
	opinion := []discordsource.Message{
		proposalMsg("1", "2026-06-01T10:00:00.000000+00:00", "I agree with this", author),
	}
	if _, _, ok := proposalStatusFromMessages(opinion, tz); ok {
		t.Error("\"I agree\" from one member must not count as a decision")
	}

	decided := []discordsource.Message{
		proposalMsg("1", "2026-06-01T10:00:00.000000+00:00", "I agree with this", author),
		proposalMsg("2", "2026-06-05T09:30:00.000000+00:00", "No objections raised, so we agreed to go ahead.", author),
	}
	status, at, ok := proposalStatusFromMessages(decided, tz)
	if !ok || status != ProposalStatusAgreed {
		t.Fatalf("status = %q (ok=%v), want agreed", status, ok)
	}
	if !strings.HasPrefix(at, "2026-06-05T11:30") {
		t.Errorf("agreedAt = %q, want the decision message in Brussels time (11:30 +02:00)", at)
	}
}

func TestBuildProposal(t *testing.T) {
	alice := discordsource.Author{ID: "111", Username: "alice_dc", GlobalName: strPtr("Alice")}
	bob := discordsource.Author{ID: "222", Username: "bob_dc"}

	cache := discordsource.ThreadCacheFile{
		ChannelID: "forum1",
		ThreadID:  "555",
		Thread: discordsource.Thread{
			ID:            "555",
			ParentID:      "forum1",
			OwnerID:       "111",
			Name:          "Repotting Day",
			AppliedTags:   []string{"tagA"},
			LastMessageID: "3",
			ThreadMetadata: discordsource.ThreadMetadata{
				Archived:         true,
				ArchiveTimestamp: "2026-06-20T10:00:00+00:00",
				CreateTimestamp:  "2026-06-01T08:00:00+00:00",
			},
		},
		// Stored newest-first, the way the Discord API returns them.
		Messages: []discordsource.Message{
			proposalMsg("3", "2026-06-10T07:00:00.000000+00:00", "We agreed to do it on the 21st.", bob),
			proposalMsg("2", "2026-06-02T09:00:00.000000+00:00", "Sounds good", bob),
			{
				ID:        "1",
				Author:    alice,
				Content:   "Let's repot the plants.\nDeadline: 2026-06-08",
				Timestamp: "2026-06-01T08:00:00.000000+00:00",
				Reactions: []discordsource.Reaction{{Emoji: discordsource.Emoji{Name: "✅"}, Count: 4}},
			},
		},
	}

	p := buildProposal(cache, map[string]string{"tagA": "✅ agreed"}, nil, "guild1")

	if p.Title != "Repotting Day" {
		t.Errorf("title = %q", p.Title)
	}
	if p.Author != "Alice" {
		t.Errorf("author = %q, want the opening post's author (oldest message)", p.Author)
	}
	if p.Messages != 3 {
		t.Errorf("messages = %d, want 3", p.Messages)
	}
	if p.URL != "https://discord.com/channels/guild1/555" {
		t.Errorf("url = %q", p.URL)
	}
	if p.Deadline != "2026-06-08" {
		t.Errorf("deadline = %q, want 2026-06-08", p.Deadline)
	}
	if p.Status != ProposalStatusAgreed {
		t.Errorf("status = %q, want agreed (forum tag)", p.Status)
	}
	// Agreed on 10 June → review due 10 September.
	if !strings.HasPrefix(p.ReviewDue, "2026-09-10") {
		t.Errorf("reviewDue = %q, want 2026-09-10 (agreed + 3 months)", p.ReviewDue)
	}
	if len(p.Participants) != 2 || p.Participants[0] != "Alice" {
		t.Errorf("participants = %v, want Alice first", p.Participants)
	}
	if len(p.Reactions) != 1 || p.Reactions[0].Count != 4 {
		t.Errorf("reactions = %+v", p.Reactions)
	}
	if !strings.HasPrefix(p.CreatedAt, "2026-06-01T10:00") {
		t.Errorf("createdAt = %q, want Brussels time (+02:00 in June)", p.CreatedAt)
	}
	if !strings.HasPrefix(p.LastActivityAt, "2026-06-10T09:00") {
		t.Errorf("lastActivityAt = %q, want the newest message in Brussels time", p.LastActivityAt)
	}
}

func TestFilterProposalsWeekAndReview(t *testing.T) {
	now := time.Date(2026, 9, 2, 12, 0, 0, 0, BrusselsTZ())
	rfc := func(t time.Time) string { return t.Format(time.RFC3339) }

	all := []Proposal{
		{
			ID: "recent", Title: "Recent chatter", Status: ProposalStatusOpen,
			LastActivityAt: rfc(now.AddDate(0, 0, -2)),
		},
		{
			ID: "stale", Title: "Quiet one", Status: ProposalStatusOpen,
			LastActivityAt: rfc(now.AddDate(0, 0, -40)),
		},
		{
			ID: "duesoon", Title: "Deadline next week", Status: ProposalStatusOpen,
			LastActivityAt: rfc(now.AddDate(0, 0, -30)),
			Deadline:       now.AddDate(0, 0, 5).Format("2006-01-02"),
		},
		{
			ID: "overdue", Title: "Deadline gone", Status: ProposalStatusOpen,
			LastActivityAt: rfc(now.AddDate(0, 0, -1)),
			Deadline:       now.AddDate(0, 0, -3).Format("2006-01-02"),
		},
		{
			ID: "review", Title: "Agreed in June", Status: ProposalStatusAgreed,
			LastActivityAt: rfc(now.AddDate(0, -3, 0)),
			AgreedAt:       rfc(now.AddDate(0, -3, 0)),
		},
		{
			ID: "tooOld", Title: "Agreed last year", Status: ProposalStatusAgreed,
			LastActivityAt: rfc(now.AddDate(-1, 0, 0)),
			AgreedAt:       rfc(now.AddDate(-1, 0, 0)),
		},
	}

	week := ids(filterProposals(all, []string{"--week"}, now))
	if !containsID(week, "recent") || !containsID(week, "duesoon") {
		t.Errorf("--week = %v, want recent activity and the upcoming deadline", week)
	}
	if containsID(week, "overdue") {
		t.Error("--week must drop proposals whose deadline has already passed")
	}
	if containsID(week, "stale") {
		t.Errorf("--week = %v, want the 40-day-quiet proposal dropped", week)
	}

	review := ids(filterProposals(all, []string{"--review"}, now))
	if len(review) != 1 || review[0] != "review" {
		t.Errorf("--review = %v, want only the proposal agreed ~3 months ago", review)
	}

	open := ids(filterProposals(all, nil, now))
	if containsID(open, "review") {
		t.Errorf("default view = %v, want open proposals only", open)
	}
}

func ids(ps []Proposal) []string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.ID)
	}
	return out
}

func containsID(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func TestGenerateProposalsWritesRestrictedIndex(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("DATA_DIR", dataDir)
	forumID := "forum1"

	// Forum snapshot (tag vocabulary) + one archived thread.
	snapshot := discordsource.ForumCacheFile{
		Channel: discordsource.ForumChannel{
			ID: forumID, Name: "🙋proposals", Type: discordsource.ChannelTypeGuildForum,
			AvailableTags: []discordsource.ForumTag{{ID: "t1", Name: "✅ agreed"}},
		},
		ChannelID: forumID,
	}
	writeProposalsFixture(t, filepath.Join(dataDir, "latest", "providers", "discord", forumID, "forum.json"), snapshot)

	thread := discordsource.ThreadCacheFile{
		ChannelID: forumID,
		ThreadID:  "900",
		Thread: discordsource.Thread{
			ID: "900", ParentID: forumID, Name: "Buy a kettle", AppliedTags: []string{"t1"},
			ThreadMetadata: discordsource.ThreadMetadata{CreateTimestamp: "2026-06-01T08:00:00+00:00"},
		},
		Messages: []discordsource.Message{
			proposalMsg("1", "2026-06-01T08:00:00.000000+00:00", "Deadline: 2026-06-08 — we need a kettle.",
				discordsource.Author{ID: "1", Username: "alice"}),
		},
	}
	writeProposalsFixture(t, filepath.Join(dataDir, "2026", "06", "providers", "discord", forumID, "threads", "900.json"), thread)

	seedProposalsSettings(t, "guild1", forumID)

	if err := GenerateProposals(nil); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(filepath.Join(dataDir, "latest", "generated", "restricted", "proposals.json"))
	if err != nil {
		t.Fatalf("proposals.json not written: %v", err)
	}
	var out ProposalsFile
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Proposals) != 1 || out.Proposals[0].Title != "Buy a kettle" {
		t.Fatalf("proposals = %+v", out.Proposals)
	}
	if out.Proposals[0].Status != ProposalStatusAgreed {
		t.Errorf("status = %q, want agreed (from the forum tag)", out.Proposals[0].Status)
	}
	if out.Channel != "🙋proposals" {
		t.Errorf("channel = %q", out.Channel)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "latest", "generated", "restricted", "proposals.md")); err != nil {
		t.Errorf("proposals.md not written: %v", err)
	}
}

func writeProposalsFixture(t *testing.T, path string, v interface{}) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// seedProposalsSettings pins a settings.json naming the guild and the forum,
// in a temporary APP_DATA_DIR the bootstrap will not overwrite.
func seedProposalsSettings(t *testing.T, guildID, forumID string) {
	t.Helper()
	t.Setenv("APP_DATA_DIR", t.TempDir())
	seedSettingsFixture(t, "settings.json", `{"discord":{"guildId":"`+guildID+`",`+
		`"channels":{},"forums":{"proposals":"`+forumID+`"}}}`)
}

// TestProposalsSyncMirrorsForum drives the sync against a fake Discord API:
// the forum object, the guild's active threads, the archived page, and one
// thread's messages.
func TestProposalsSyncMirrorsForum(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("DATA_DIR", dataDir)
	t.Setenv("DISCORD_BOT_TOKEN", "test-token")

	forumID := "forum1"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/channels/"+forumID:
			json.NewEncoder(w).Encode(discordsource.ForumChannel{
				ID: forumID, Name: "proposals", Type: discordsource.ChannelTypeGuildForum,
			})
		case r.URL.Path == "/guilds/guild1/threads/active":
			json.NewEncoder(w).Encode(map[string]interface{}{
				"threads": []discordsource.Thread{
					{
						ID: "900", ParentID: forumID, Name: "Open proposal", LastMessageID: "2",
						ThreadMetadata: discordsource.ThreadMetadata{CreateTimestamp: "2026-06-01T08:00:00+00:00"},
					},
					// A thread in another channel must be ignored.
					{ID: "901", ParentID: "other", Name: "Not a proposal"},
				},
			})
		case r.URL.Path == "/channels/"+forumID+"/threads/archived/public":
			json.NewEncoder(w).Encode(map[string]interface{}{
				"threads": []discordsource.Thread{}, "has_more": false,
			})
		case strings.HasPrefix(r.URL.Path, "/channels/900/messages"):
			json.NewEncoder(w).Encode([]discordsource.Message{
				proposalMsg("2", "2026-06-02T08:00:00.000000+00:00", "reply", discordsource.Author{ID: "2", Username: "bob"}),
				proposalMsg("1", "2026-06-01T08:00:00.000000+00:00", "the proposal", discordsource.Author{ID: "1", Username: "alice"}),
			})
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()

	origBase := discordAPIBase
	discordAPIBase = srv.URL
	t.Cleanup(func() { discordAPIBase = origBase })

	seedProposalsSettings(t, "guild1", forumID)

	n, err := ProposalsSync(nil)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("updated = %d, want 1", n)
	}

	// Filed under the thread's creation month, and mirrored to latest/.
	path := filepath.Join(dataDir, "2026", "06", "providers", "discord", forumID, "threads", "900.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("thread not archived: %v", err)
	}
	var cache discordsource.ThreadCacheFile
	if err := json.Unmarshal(data, &cache); err != nil {
		t.Fatal(err)
	}
	if len(cache.Messages) != 2 {
		t.Errorf("messages = %d, want 2", len(cache.Messages))
	}
	if _, err := os.Stat(filepath.Join(dataDir, "latest", "providers", "discord", forumID, "threads", "900.json")); err != nil {
		t.Errorf("thread not mirrored to latest/: %v", err)
	}

	// Second run: the thread's last message is unchanged, so nothing is
	// re-fetched and nothing is rewritten.
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	n, err = ProposalsSync(nil)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("second run updated = %d, want 0 (unchanged thread must be skipped)", n)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !before.ModTime().Equal(after.ModTime()) {
		t.Error("unchanged thread was rewritten — that would re-trigger generate on every sync")
	}
}

func TestProposalsSyncReportsMissingAccess(t *testing.T) {
	t.Setenv("DATA_DIR", t.TempDir())
	t.Setenv("DISCORD_BOT_TOKEN", "test-token")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
		w.Write([]byte(`{"message":"Missing Access","code":50001}`))
	}))
	defer srv.Close()

	origBase := discordAPIBase
	discordAPIBase = srv.URL
	t.Cleanup(func() { discordAPIBase = origBase })

	seedProposalsSettings(t, "guild1", "forum1")

	_, err := ProposalsSync(nil)
	if err == nil {
		t.Fatal("expected an error on 403")
	}
	if !isDiscordForbidden(err) && !strings.Contains(err.Error(), "View Channel") {
		t.Errorf("error = %v, want the permission fix spelled out", err)
	}
}

func TestSnowflakeTime(t *testing.T) {
	// 1280931158254682134 is the Commons Hub proposals forum; its snowflake
	// encodes September 2024.
	got, ok := discordsource.SnowflakeTime("1280931158254682134")
	if !ok {
		t.Fatal("snowflake not parsed")
	}
	if got.Year() != 2024 || got.Month() != time.September {
		t.Errorf("snowflake time = %s, want September 2024", got)
	}
	if _, ok := discordsource.SnowflakeTime("not-a-snowflake"); ok {
		t.Error("expected a non-numeric id to fail")
	}
}

// TestProposalsSyncFallsBackToMetadata covers the degraded path: the forum
// itself is forbidden, but the guild-wide active-thread listing answers, so the
// sync archives what it can see instead of failing.
func TestProposalsSyncFallsBackToMetadata(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("DATA_DIR", dataDir)
	t.Setenv("DISCORD_BOT_TOKEN", "test-token")

	forumID := "forum1"
	// 1544376265572614236 is a real-shaped snowflake; the thread's last
	// message is what dates its activity in this mode.
	lastMessageID := "1544376266868658226"
	// Flipped once the bot is granted the permission: View Channel governs
	// both the forum object and its threads' messages, so they move together.
	hasAccess := false

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/channels/"+forumID:
			if !hasAccess {
				w.WriteHeader(403)
				w.Write([]byte(`{"message":"Missing Access","code":50001}`))
				return
			}
			json.NewEncoder(w).Encode(discordsource.ForumChannel{
				ID: forumID, Name: "proposals", Type: discordsource.ChannelTypeGuildForum,
			})
		case r.URL.Path == "/channels/"+forumID+"/threads/archived/public":
			if !hasAccess {
				w.WriteHeader(403)
				w.Write([]byte(`{"message":"Missing Access","code":50001}`))
				return
			}
			json.NewEncoder(w).Encode(map[string]interface{}{"threads": []discordsource.Thread{}, "has_more": false})
		case r.URL.Path == "/guilds/guild1/threads/active":
			json.NewEncoder(w).Encode(map[string]interface{}{
				"threads": []discordsource.Thread{{
					ID: "900", ParentID: forumID, Name: "Update Furniture",
					OwnerID: "111", MessageCount: 2, LastMessageID: lastMessageID,
					ThreadMetadata: discordsource.ThreadMetadata{CreateTimestamp: "2026-06-01T08:00:00+00:00"},
				}},
			})
		case strings.HasPrefix(r.URL.Path, "/channels/900/messages"):
			if !hasAccess {
				w.WriteHeader(403)
				w.Write([]byte(`{"message":"Missing Access","code":50001}`))
				return
			}
			json.NewEncoder(w).Encode([]discordsource.Message{
				proposalMsg("1", "2026-06-01T08:00:00.000000+00:00", "the proposal body",
					discordsource.Author{ID: "111", Username: "alice"}),
			})
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()

	origBase := discordAPIBase
	discordAPIBase = srv.URL
	t.Cleanup(func() { discordAPIBase = origBase })

	seedProposalsSettings(t, "guild1", forumID)

	n, err := ProposalsSync(nil)
	if err != nil {
		t.Fatalf("metadata-only sync must not fail: %v", err)
	}
	if n != 1 {
		t.Fatalf("updated = %d, want 1", n)
	}

	path := filepath.Join(dataDir, "2026", "06", "providers", "discord", forumID, "threads", "900.json")
	var cache discordsource.ThreadCacheFile
	readJSONFile(t, path, &cache)
	if !cache.MetadataOnly {
		t.Error("archive must be marked metadataOnly")
	}
	if len(cache.Messages) != 0 {
		t.Errorf("messages = %d, want none (the bot cannot read them)", len(cache.Messages))
	}
	if cache.Thread.Name != "Update Furniture" {
		t.Errorf("thread name = %q", cache.Thread.Name)
	}

	// The snapshot records the degraded mode too, so a later reader knows the
	// tag vocabulary is missing rather than empty.
	var snapshot discordsource.ForumCacheFile
	readJSONFile(t, filepath.Join(dataDir, "latest", "providers", "discord", forumID, "forum.json"), &snapshot)
	if !snapshot.MetadataOnly {
		t.Error("forum snapshot must be marked metadataOnly")
	}

	// A metadata-only thread knows only its owner's id. The name comes from
	// the channel mirrors `chb messages sync` already wrote.
	writeProposalsFixture(t,
		filepath.Join(dataDir, "latest", "providers", "discord", "chat1", "messages.json"),
		discordsource.CacheFile{ChannelID: "chat1", Messages: []discordsource.Message{
			proposalMsg("7", "2026-06-01T08:00:00.000000+00:00", "hi",
				discordsource.Author{ID: "111", Username: "alice_dc", GlobalName: strPtr("Alice")}),
		}})

	// Generate: the proposal is listed, flagged, and dated from the thread.
	if err := GenerateProposals(nil); err != nil {
		t.Fatal(err)
	}
	var out ProposalsFile
	readJSONFile(t, filepath.Join(dataDir, "latest", "generated", "restricted", "proposals.json"), &out)
	if len(out.Proposals) != 1 {
		t.Fatalf("proposals = %+v", out.Proposals)
	}
	p := out.Proposals[0]
	if !p.MetadataOnly {
		t.Error("proposal must be flagged metadataOnly")
	}
	if p.Messages != 2 {
		t.Errorf("messages = %d, want Discord's own count (2)", p.Messages)
	}
	if p.Body != "" || p.Deadline != "" {
		t.Errorf("expected no body/deadline, got body=%q deadline=%q", p.Body, p.Deadline)
	}
	wantActivity, _ := discordsource.SnowflakeTime(lastMessageID)
	if !strings.HasPrefix(p.LastActivityAt, wantActivity.In(BrusselsTZ()).Format("2006-01-02")) {
		t.Errorf("lastActivityAt = %q, want the last message's snowflake date %s",
			p.LastActivityAt, wantActivity.In(BrusselsTZ()).Format("2006-01-02"))
	}
	if p.AuthorID != "111" {
		t.Errorf("authorId = %q, want the thread owner", p.AuthorID)
	}
	if p.Author != "Alice" {
		t.Errorf("author = %q, want the name resolved from the local message mirror", p.Author)
	}

	// Once the permission lands, the next sync replaces metadata with content.
	hasAccess = true
	if _, err := ProposalsSync(nil); err != nil {
		t.Fatal(err)
	}
	// A fresh value: metadataOnly is omitempty, so decoding into the old
	// struct would keep the stale true and hide a regression.
	var refreshed discordsource.ThreadCacheFile
	readJSONFile(t, path, &refreshed)
	if refreshed.MetadataOnly {
		t.Error("full sync must clear the metadataOnly marker")
	}
	if len(refreshed.Messages) != 1 {
		t.Errorf("messages = %d, want the thread's content", len(refreshed.Messages))
	}
}

// TestProposalsSyncKeepsFullArchiveOnPermissionLoss guards the one-way rule: a
// mirror that already has content must never be overwritten by a metadata-only
// pass (e.g. the bot loses the permission again).
func TestProposalsSyncKeepsFullArchiveOnPermissionLoss(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("DATA_DIR", dataDir)
	t.Setenv("DISCORD_BOT_TOKEN", "test-token")

	forumID := "forum1"
	full := discordsource.ThreadCacheFile{
		ChannelID: forumID, ThreadID: "900",
		Thread: discordsource.Thread{
			ID: "900", ParentID: forumID, Name: "Update Furniture", LastMessageID: "5",
			ThreadMetadata: discordsource.ThreadMetadata{CreateTimestamp: "2026-06-01T08:00:00+00:00"},
		},
		Messages: []discordsource.Message{
			proposalMsg("1", "2026-06-01T08:00:00.000000+00:00", "the proposal body",
				discordsource.Author{ID: "111", Username: "alice"}),
		},
	}
	for _, dir := range []string{
		filepath.Join(dataDir, "2026", "06", "providers", "discord", forumID, "threads"),
		filepath.Join(dataDir, "latest", "providers", "discord", forumID, "threads"),
	} {
		writeProposalsFixture(t, filepath.Join(dir, "900.json"), full)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/guilds/guild1/threads/active":
			json.NewEncoder(w).Encode(map[string]interface{}{
				"threads": []discordsource.Thread{{
					ID: "900", ParentID: forumID, Name: "Update Furniture", LastMessageID: "6",
					ThreadMetadata: discordsource.ThreadMetadata{CreateTimestamp: "2026-06-01T08:00:00+00:00"},
				}},
			})
		default:
			w.WriteHeader(403)
			w.Write([]byte(`{"message":"Missing Access","code":50001}`))
		}
	}))
	defer srv.Close()

	origBase := discordAPIBase
	discordAPIBase = srv.URL
	t.Cleanup(func() { discordAPIBase = origBase })

	seedProposalsSettings(t, "guild1", forumID)

	if _, err := ProposalsSync(nil); err != nil {
		t.Fatal(err)
	}

	var cache discordsource.ThreadCacheFile
	readJSONFile(t, filepath.Join(dataDir, "2026", "06", "providers", "discord", forumID, "threads", "900.json"), &cache)
	if cache.MetadataOnly || len(cache.Messages) != 1 {
		t.Errorf("existing content was downgraded to metadata: metadataOnly=%v messages=%d",
			cache.MetadataOnly, len(cache.Messages))
	}
}

func readJSONFile(t *testing.T, path string, v interface{}) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if err := json.Unmarshal(data, v); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
}
