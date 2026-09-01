package etherscan

import (
	"testing"
	"time"
)

// TestClassifyError pins the three Etherscan failure modes seen in production.
// Etherscan reports all of them as HTTP 200 with status "0", and the useful
// text is usually in `result` rather than `message`.
func TestClassifyError(t *testing.T) {
	for _, tc := range []struct {
		name    string
		message string
		result  string
		want    errorKind
	}{
		{
			"daily community cap",
			"NOTOK",
			"Community Free API Limit reached. Resets 2026-09-01 12:00:00 UTC. For higher, uninterrupted limits, please upgrade to API Pro: https://etherscan.io/apis",
			errQuota,
		},
		{
			"per-second burst",
			"NOTOK",
			"Max rate limit reached, please use API Key for higher rate limit",
			errTransient,
		},
		{
			// Etherscan's own wording, typo included.
			"server-side query timeout",
			"Query Timeout occured. Please select a smaller result dataset",
			"",
			errTransient,
		},
		{
			// A valid key called too fast reports as invalid; treating this as
			// fatal sent us hunting a key problem that did not exist.
			"invalid api key is really throttling",
			"NOTOK",
			"Invalid API Key (#err2)",
			errTransient,
		},
		{
			"genuinely malformed request",
			"NOTOK",
			"Error! Missing or invalid Module name",
			errFatal,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyError(tc.message, tc.result); got != tc.want {
				t.Errorf("classifyError(%q, %q) = %v, want %v", tc.message, tc.result, got, tc.want)
			}
		})
	}
}

// TestQuotaExhaustedIsSticky covers the fast-fail: once one account hits the
// daily wall, the rest of the sync must not each spend four attempts on it.
func TestQuotaExhaustedIsSticky(t *testing.T) {
	ResetQuotaState()
	t.Cleanup(ResetQuotaState)

	if _, blocked := QuotaExhausted(); blocked {
		t.Fatal("quota should start clear")
	}
	noteQuotaExhausted("Community Free API Limit reached. Resets 2026-09-01 12:00:00 UTC")
	notice, blocked := QuotaExhausted()
	if !blocked {
		t.Fatal("quota should be recorded")
	}
	if notice == "" {
		t.Error("want the API's reset notice preserved for the operator")
	}
	// The first notice wins; a later, vaguer one must not overwrite it.
	noteQuotaExhausted("something else")
	if got, _ := QuotaExhausted(); got != notice {
		t.Errorf("notice changed to %q, want the original preserved", got)
	}
}

// TestThrottleSpacesRequests guards the per-second limit that produced the
// "Invalid API Key" responses in the first place.
func TestThrottleSpacesRequests(t *testing.T) {
	rateMu.Lock()
	lastRequest = time.Time{}
	rateMu.Unlock()

	start := time.Now()
	for i := 0; i < 3; i++ {
		throttle()
	}
	// First call is free; the next two must each wait a full interval.
	if elapsed := time.Since(start); elapsed < 2*minRequestInterval {
		t.Errorf("3 throttled calls took %v, want at least %v", elapsed, 2*minRequestInterval)
	}
}

func TestRetryDelayGrows(t *testing.T) {
	if retryDelay(1) >= retryDelay(2) {
		t.Errorf("retryDelay should grow: %v then %v", retryDelay(1), retryDelay(2))
	}
	if retryDelay(1) <= time.Second {
		t.Errorf("retryDelay(1) = %v, want longer than the 1s per-second window", retryDelay(1))
	}
}
