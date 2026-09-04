package discord

// Forum channels (Discord type 15) hold their content in threads, not in the
// channel itself: GET /channels/<forum>/messages returns nothing useful. Each
// post is a thread whose first message is the post body and whose remaining
// messages are the discussion.
//
// The archive keeps one file per thread — a raw thread object plus its raw
// messages — under the month the thread was created:
//
//	providers/discord/<forumID>/threads/<threadID>.json
//
// plus a single forum snapshot (channel object + current thread list) under
//
//	providers/discord/<forumID>/forum.json
//
// Nothing here transforms: timestamps stay in the shape Discord sent them.

import (
	"path/filepath"
	"strconv"
	"time"
)

const (
	ForumFile      = "forum.json"
	ThreadsDirName = "threads"

	// ChannelTypeGuildForum is Discord's numeric type for a forum channel.
	ChannelTypeGuildForum = 15
)

// ForumChannel is the raw channel object of a forum channel. Only the fields
// we archive are named; the rest of the payload is dropped on purpose (a
// channel object carries permission overwrites for the whole guild).
type ForumChannel struct {
	ID            string     `json:"id"`
	GuildID       string     `json:"guild_id,omitempty"`
	Name          string     `json:"name"`
	Type          int        `json:"type"`
	Topic         string     `json:"topic,omitempty"`
	AvailableTags []ForumTag `json:"available_tags,omitempty"`
}

// ForumTag is one of the tags a forum offers; threads reference them by ID in
// Thread.AppliedTags.
type ForumTag struct {
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	Moderated bool    `json:"moderated,omitempty"`
	EmojiName *string `json:"emoji_name,omitempty"`
}

// Thread is a forum post (a thread channel object).
type Thread struct {
	ID               string         `json:"id"`
	GuildID          string         `json:"guild_id,omitempty"`
	ParentID         string         `json:"parent_id,omitempty"`
	OwnerID          string         `json:"owner_id,omitempty"`
	Name             string         `json:"name"`
	Type             int            `json:"type"`
	MessageCount     int            `json:"message_count,omitempty"`
	TotalMessageSent int            `json:"total_message_sent,omitempty"`
	LastMessageID    string         `json:"last_message_id,omitempty"`
	AppliedTags      []string       `json:"applied_tags,omitempty"`
	ThreadMetadata   ThreadMetadata `json:"thread_metadata"`
}

type ThreadMetadata struct {
	Archived            bool   `json:"archived"`
	ArchiveTimestamp    string `json:"archive_timestamp,omitempty"`
	AutoArchiveDuration int    `json:"auto_archive_duration,omitempty"`
	Locked              bool   `json:"locked,omitempty"`
	CreateTimestamp     string `json:"create_timestamp,omitempty"`
}

// ThreadCacheFile is one archived forum thread: the thread object as Discord
// returned it plus every message in it (newest first, the API's own order).
type ThreadCacheFile struct {
	Thread   Thread    `json:"thread"`
	Messages []Message `json:"messages"`
	// MetadataOnly marks an archive written without the thread's content:
	// the bot could list the thread but not read it. Messages is empty and
	// callers must not treat that as "an empty discussion". A later sync with
	// read access replaces the file with the full mirror.
	MetadataOnly bool   `json:"metadataOnly,omitempty"`
	CachedAt     string `json:"cachedAt"`
	ChannelID    string `json:"channelId"`
	ThreadID     string `json:"threadId"`
}

// ForumCacheFile is the forum snapshot: the channel object (which carries the
// tag vocabulary) and the thread list as of the last sync.
type ForumCacheFile struct {
	Channel ForumChannel `json:"channel"`
	Threads []Thread     `json:"threads"`
	// MetadataOnly marks a snapshot taken without read access to the forum:
	// Channel holds only the id (no name, no tag vocabulary) and Threads
	// lists open threads only, since the archived-thread endpoint needs the
	// same permission the bot is missing.
	MetadataOnly bool   `json:"metadataOnly,omitempty"`
	CachedAt     string `json:"cachedAt"`
	ChannelID    string `json:"channelId"`
}

func ForumRelPath(channelID string) string {
	return RelPath(channelID, ForumFile)
}

func ForumPath(dataDir, year, month, channelID string) string {
	return filepath.Join(dataDir, year, month, ForumRelPath(channelID))
}

func ThreadRelPath(channelID, threadID string) string {
	return RelPath(channelID, ThreadsDirName, threadID+".json")
}

func ThreadPath(dataDir, year, month, channelID, threadID string) string {
	return filepath.Join(dataDir, year, month, ThreadRelPath(channelID, threadID))
}

func ThreadsDirPath(dataDir, year, month, channelID string) string {
	return filepath.Join(dataDir, year, month, RelPath(channelID, ThreadsDirName))
}

// discordEpoch is the first millisecond of 2015, the origin every snowflake
// counts from.
const discordEpoch = 1420070400000

// SnowflakeTime returns the creation time encoded in a Discord ID. Old threads
// (created before Discord added create_timestamp) carry their creation time
// only here, so this is the fallback CreatedAt for a thread.
func SnowflakeTime(id string) (time.Time, bool) {
	n, err := strconv.ParseInt(id, 10, 64)
	if err != nil || n <= 0 {
		return time.Time{}, false
	}
	ms := (n >> 22) + discordEpoch
	return time.UnixMilli(ms).UTC(), true
}

// LastActivityAt returns when the thread last saw a message, read from the
// last message's snowflake. It is the only activity signal available when the
// bot may list a thread but not read it.
func (t Thread) LastActivityAt() (time.Time, bool) {
	if t.LastMessageID == "" {
		return time.Time{}, false
	}
	return SnowflakeTime(t.LastMessageID)
}

// CreatedAt returns the thread's creation time, preferring the explicit
// metadata timestamp and falling back to the snowflake.
func (t Thread) CreatedAt() (time.Time, bool) {
	if ts := t.ThreadMetadata.CreateTimestamp; ts != "" {
		if parsed, err := time.Parse(time.RFC3339Nano, ts); err == nil {
			return parsed.UTC(), true
		}
	}
	return SnowflakeTime(t.ID)
}
