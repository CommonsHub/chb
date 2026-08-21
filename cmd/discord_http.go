package cmd

// Rate-limit-aware HTTP layer for the Discord REST API.
//
// Discord budgets ~5 requests/sec per route bucket and answers 429 with a
// retry_after when it is exceeded. The old fetch loop slept exactly that
// retry_after (~400ms) and immediately fired again — a busy-loop that burned
// one wasted request per 429, printed one log line each, and never backed
// off. Repeated 429s can escalate to a Cloudflare-level ban of the bot's IP,
// so "hammer until it sticks" is the one strategy Discord explicitly asks
// clients not to use.
//
// discordPacer does three things instead:
//
//  1. Proactive pacing: every 200 response carries the bucket state in
//     X-RateLimit-Remaining / X-RateLimit-Reset-After. When the bucket is
//     empty the pacer notes when it refills and sleeps *before* the next
//     request — the happy path never sees a 429 at all.
//  2. Honest backoff: a 429's retry_after is honoured, and consecutive 429s
//     double the wait each time (capped at 30s) — if Discord is still saying
//     no, asking louder is not the answer.
//  3. Quiet logging: short waits update the in-place progress line; only
//     unusual waits (≥2s) get a printed line. Ten consecutive 429s abort
//     with a real error instead of looping forever.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

// Injection points for tests.
var (
	discordSleep = time.Sleep
	discordNow   = time.Now
)

const (
	discordMaxConsecutive429 = 10
	discordMaxBackoff        = 30 * time.Second
)

type discordPacer struct {
	consecutive429 int
	nextAllowed    time.Time
}

// get performs one GET against the Discord API, waiting out the rate-limit
// bucket before the request and backing off (with escalation) on 429s.
// The caller owns the returned body.
func (p *discordPacer) get(url, token string) (*http.Response, error) {
	for {
		if wait := p.nextAllowed.Sub(discordNow()); wait > 0 {
			p.logWait(wait, "pacing")
			discordSleep(wait)
		}

		req, err := http.NewRequest("GET", url, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bot "+token)
		req.Header.Set("Content-Type", "application/json")

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, err
		}

		if resp.StatusCode != 429 {
			p.consecutive429 = 0
			p.noteBucket(resp)
			return resp, nil
		}

		var rl struct {
			RetryAfter float64 `json:"retry_after"`
			Global     bool    `json:"global"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&rl)
		resp.Body.Close()

		p.consecutive429++
		if p.consecutive429 >= discordMaxConsecutive429 {
			return nil, fmt.Errorf("Discord kept rate-limiting after %d retries — try again later", p.consecutive429)
		}

		wait := time.Duration(rl.RetryAfter * float64(time.Second))
		if wait <= 0 {
			wait = 500 * time.Millisecond
		}
		// Escalate on repeats: 1×, 2×, 4×, … capped. A single 429 costs its
		// retry_after; a stream of them means we should genuinely back up.
		wait *= time.Duration(1) << min(p.consecutive429-1, 6)
		if wait > discordMaxBackoff {
			wait = discordMaxBackoff
		}
		p.logWait(wait, "rate limited")
		discordSleep(wait)
	}
}

// noteBucket records when the route's bucket refills so the next get() can
// wait it out instead of provoking a 429.
func (p *discordPacer) noteBucket(resp *http.Response) {
	if resp.Header.Get("X-RateLimit-Remaining") != "0" {
		return
	}
	after, err := strconv.ParseFloat(resp.Header.Get("X-RateLimit-Reset-After"), 64)
	if err != nil || after <= 0 {
		return
	}
	p.nextAllowed = discordNow().Add(time.Duration(after * float64(time.Second)))
}

func (p *discordPacer) logWait(wait time.Duration, why string) {
	if wait >= 2*time.Second {
		fmt.Printf("    %s%s — backing off %s%s\n", Fmt.Yellow, why, wait.Round(100*time.Millisecond), Fmt.Reset)
		return
	}
	// Sub-second waits are business as usual: keep them on the in-place
	// progress line instead of scrolling the terminal.
	Progress(fmt.Sprintf("%s, waiting %s", why, wait.Round(10*time.Millisecond)))
}

// snowflakeLess compares two Discord snowflake IDs numerically (they are
// decimal strings too large for int64 to be assumed; length-then-lex compare
// is exact for non-padded decimals).
func snowflakeLess(a, b string) bool {
	if len(a) != len(b) {
		return len(a) < len(b)
	}
	return a < b
}
