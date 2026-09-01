package etherscan

import (
	"strings"
	"sync"
	"time"
)

// Etherscan's free tier allows 5 requests per second. Exceeding it does not
// return a clean 429 — the API replies 200 OK with a JSON body whose message is
// "NOTOK" and whose result is "Max rate limit reached" or, confusingly,
// "Invalid API Key (#err2)". A key that looks revoked is very often just a
// burst, which is why the classifier below treats that message as transient.
const (
	minRequestInterval = 250 * time.Millisecond // ≤4 req/s, one under the cap
	maxAttempts        = 4
)

var (
	rateMu       sync.Mutex
	lastRequest  time.Time
	quotaBlocked string // non-empty once the daily cap is hit: the reset notice
)

// throttle spaces out calls so a burst of accounts never trips the per-second
// limit. Every request in this package goes through it.
func throttle() {
	rateMu.Lock()
	defer rateMu.Unlock()
	if wait := minRequestInterval - time.Since(lastRequest); wait > 0 {
		time.Sleep(wait)
	}
	lastRequest = time.Now()
}

// noteQuotaExhausted records that the daily community quota is gone, so the
// remaining accounts in a sync fail fast with the reset time instead of each
// burning three retries against a limit that cannot lift until tomorrow.
func noteQuotaExhausted(detail string) {
	rateMu.Lock()
	defer rateMu.Unlock()
	if quotaBlocked == "" {
		quotaBlocked = detail
	}
}

// QuotaExhausted reports whether the daily free-tier cap has been hit during
// this process, and the API's own reset notice if so.
func QuotaExhausted() (string, bool) {
	rateMu.Lock()
	defer rateMu.Unlock()
	return quotaBlocked, quotaBlocked != ""
}

// ResetQuotaState clears the recorded daily-cap state. Tests use it; a
// long-lived process would call it after the reset time passes.
func ResetQuotaState() {
	rateMu.Lock()
	defer rateMu.Unlock()
	quotaBlocked = ""
}

// isProEndpointNotice detects the API's refusal to serve a Pro-only endpoint
// ("Sorry, it looks like you are trying to access an API Pro endpoint"). It is
// neither transient nor a malformed request: the call is correct, the key just
// isn't entitled, so callers fall back to another source instead of retrying.
func isProEndpointNotice(detail string) bool {
	return strings.Contains(strings.ToLower(detail), "api pro endpoint")
}

// errorKind classifies an Etherscan API-level error (HTTP 200 with status "0").
type errorKind int

const (
	// errFatal won't change on retry — a genuinely malformed request.
	errFatal errorKind = iota
	// errTransient is throttling or a server-side timeout: retry with backoff.
	errTransient
	// errQuota is the daily community cap: retrying today cannot help.
	errQuota
)

// classifyError decides how to treat an Etherscan error response. The
// actionable text often lives in `result` ("Max rate limit reached") rather
// than `message` (a bare "NOTOK"), so both are inspected.
func classifyError(message, result string) errorKind {
	s := strings.ToLower(message + " " + result)
	switch {
	// "Community Free API Limit reached. Resets 2026-09-01 12:00:00 UTC" —
	// a day-long wall, not a burst. Checked before the generic "rate limit"
	// case because the daily notice also reads as a limit.
	case strings.Contains(s, "resets") && strings.Contains(s, "limit reached"):
		return errQuota
	case strings.Contains(s, "rate limit"):
		return errTransient
	// "Query Timeout occured. Please select a smaller result dataset" — an
	// Etherscan-side timeout on a heavy query. Load-dependent: the identical
	// request succeeds moments later.
	case strings.Contains(s, "query timeout"):
		return errTransient
	// Returned for a valid key that is simply being called too fast. Treated
	// as transient; if it survives every attempt the caller says so plainly.
	case strings.Contains(s, "invalid api key"):
		return errTransient
	default:
		return errFatal
	}
}

// retryDelay backs off between attempts. Deliberately longer than the
// per-second window so a throttled key has time to recover.
func retryDelay(attempt int) time.Duration {
	return time.Duration(attempt) * 2 * time.Second
}
