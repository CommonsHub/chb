package cmd

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// capturePacerSleeps replaces the pacer's sleep with a recorder for the test.
func capturePacerSleeps(t *testing.T) *[]time.Duration {
	t.Helper()
	var sleeps []time.Duration
	origSleep := discordSleep
	discordSleep = func(d time.Duration) { sleeps = append(sleeps, d) }
	t.Cleanup(func() { discordSleep = origSleep })
	return &sleeps
}

func TestDiscordPacerBacksOffExponentially(t *testing.T) {
	sleeps := capturePacerSleeps(t)

	tries := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tries++
		if tries <= 3 {
			w.WriteHeader(429)
			json.NewEncoder(w).Encode(map[string]interface{}{"retry_after": 0.4})
			return
		}
		json.NewEncoder(w).Encode([]DiscordMessage{})
	}))
	defer srv.Close()

	p := &discordPacer{}
	resp, err := p.get(srv.URL, "tok")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if tries != 4 {
		t.Fatalf("tries = %d, want 4 (three 429s then success)", tries)
	}
	// Escalation: retry_after × 1, × 2, × 4.
	want := []time.Duration{400 * time.Millisecond, 800 * time.Millisecond, 1600 * time.Millisecond}
	if len(*sleeps) != len(want) {
		t.Fatalf("sleeps = %v, want %v", *sleeps, want)
	}
	for i := range want {
		if (*sleeps)[i] != want[i] {
			t.Errorf("sleep[%d] = %v, want %v (each consecutive 429 must double the wait)", i, (*sleeps)[i], want[i])
		}
	}
}

func TestDiscordPacerGivesUpEventually(t *testing.T) {
	capturePacerSleeps(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		json.NewEncoder(w).Encode(map[string]interface{}{"retry_after": 0.1})
	}))
	defer srv.Close()

	p := &discordPacer{}
	if _, err := p.get(srv.URL, "tok"); err == nil {
		t.Fatal("an endless stream of 429s must become an error, not an infinite loop")
	}
}

func TestDiscordPacerPacesProactivelyFromHeaders(t *testing.T) {
	sleeps := capturePacerSleeps(t)

	got429 := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Every response says: bucket empty, refills in 350ms.
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset-After", "0.35")
		json.NewEncoder(w).Encode([]DiscordMessage{})
		_ = got429
	}))
	defer srv.Close()

	p := &discordPacer{}
	for i := 0; i < 2; i++ {
		resp, err := p.get(srv.URL, "tok")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}

	// The second request must have waited out the bucket instead of
	// provoking a 429.
	if len(*sleeps) != 1 {
		t.Fatalf("sleeps = %v, want exactly one proactive wait before the second request", *sleeps)
	}
	if (*sleeps)[0] <= 0 || (*sleeps)[0] > 350*time.Millisecond {
		t.Errorf("proactive wait = %v, want ~350ms", (*sleeps)[0])
	}
}

func TestFetchMessagesSinceCachedStopsAtOverlap(t *testing.T) {
	capturePacerSleeps(t)

	// 250 messages, IDs 1000 (oldest) … 1249 (newest), served newest-first in
	// pages of 100 like the real API.
	var all []DiscordMessage
	for id := 1249; id >= 1000; id-- {
		all = append(all, DiscordMessage{ID: fmt.Sprintf("%d", id), Timestamp: "2026-08-01T10:00:00+00:00"})
	}
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		before := r.URL.Query().Get("before")
		start := 0
		if before != "" {
			for i, m := range all {
				if m.ID == before {
					start = i + 1
					break
				}
			}
		}
		end := start + 100
		if end > len(all) {
			end = len(all)
		}
		json.NewEncoder(w).Encode(all[start:end])
	}))
	defer srv.Close()

	origBase := discordAPIBase
	discordAPIBase = srv.URL
	t.Cleanup(func() { discordAPIBase = origBase })

	t.Run("caught up after one page", func(t *testing.T) {
		requests = 0
		// Newest cached = 1200: the first page (1249…1150) overlaps it.
		msgs, err := fetchMessagesSinceCached("chan", "tok", "1200")
		if err != nil {
			t.Fatal(err)
		}
		if requests != 1 {
			t.Errorf("requests = %d, want 1 — a nearly-current channel must cost a single request", requests)
		}
		if len(msgs) != 100 {
			t.Errorf("returned %d messages, want the full overlap page (100)", len(msgs))
		}
	})

	t.Run("paginates only until the overlap", func(t *testing.T) {
		requests = 0
		// Newest cached = 1100 → needs page 1 (…1150) and page 2 (…1050).
		msgs, err := fetchMessagesSinceCached("chan", "tok", "1100")
		if err != nil {
			t.Fatal(err)
		}
		if requests != 2 {
			t.Errorf("requests = %d, want 2", requests)
		}
		if len(msgs) != 200 {
			t.Errorf("returned %d messages, want 200", len(msgs))
		}
	})

	t.Run("empty channel history end", func(t *testing.T) {
		requests = 0
		// Nothing cached would use the backfill path; but an ID older than
		// everything must still terminate at the end of history.
		msgs, err := fetchMessagesSinceCached("chan", "tok", "1")
		if err != nil {
			t.Fatal(err)
		}
		// 3 pages of messages + the empty page that proves history ended:
		// with no overlap to stop at, the end is only knowable that way.
		if requests != 4 {
			t.Errorf("requests = %d, want 4 (3 pages + terminating empty page)", requests)
		}
		if len(msgs) != 250 {
			t.Errorf("returned %d, want all 250", len(msgs))
		}
	})
}

func TestMergeMessagesByID(t *testing.T) {
	existing := []DiscordMessage{
		{ID: "300", Content: "c"},
		{ID: "200", Content: "b", Reactions: nil},
		{ID: "100", Content: "a"},
	}
	fetched := []DiscordMessage{
		{ID: "400", Content: "d"},
		{ID: "200", Content: "b", Reactions: []DiscordReaction{{Count: 3}}}, // refreshed
	}
	out := mergeMessagesByID(existing, fetched)
	if len(out) != 4 {
		t.Fatalf("len = %d, want 4", len(out))
	}
	if out[0].ID != "400" || out[1].ID != "300" || out[2].ID != "200" || out[3].ID != "100" {
		t.Errorf("order = %v, want newest-first by snowflake", []string{out[0].ID, out[1].ID, out[2].ID, out[3].ID})
	}
	if len(out[2].Reactions) != 1 || out[2].Reactions[0].Count != 3 {
		t.Errorf("fetched copy must win: reactions = %+v", out[2].Reactions)
	}
}

func TestSnowflakeLess(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"99", "100", true},   // shorter = smaller
		{"100", "99", false},  // longer = bigger
		{"100", "200", true},  // same length, lexicographic
		{"200", "100", false}, //
		{"100", "100", false}, // equal
		{"", "1", true},       // empty (no cache) is smaller than anything
	}
	for _, c := range cases {
		if got := snowflakeLess(c.a, c.b); got != c.want {
			t.Errorf("snowflakeLess(%q,%q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}
