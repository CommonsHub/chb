package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	odoosource "github.com/CommonsHub/chb/providers/odoo"
)

// deriveOdooSnapshotForMonth reconstructs a past month's Odoo membership from
// the newest snapshot on disk.
//
// Odoo's subscription API returns live state, not history, so `chb members
// sync` only ever fetches it for the *current* month (see members_sync.go).
// Any month that went by without a sync has no Odoo snapshot of its own — which
// is why May–August 2026 show Stripe-only members while the instance was
// unreachable. But each subscription records the span from its start date to
// its next invoice date, and that span is enough to answer "was this running in
// month M".
//
// The reconstruction is necessarily incomplete, and only in one direction: a
// subscription that was cancelled before the newest snapshot was taken is
// absent from that snapshot entirely, so a derived month can undercount but
// never overcount. Callers mark the result so a reader can tell a derived month
// from one captured at the time.
func deriveOdooSnapshotForMonth(dataDir, year, month string) (providerSnapshot, bool) {
	source, sourceYM, ok := newestOdooSnapshot(dataDir)
	if !ok {
		return providerSnapshot{}, false
	}
	// Only ever look backwards. A month later than the snapshot is the future
	// as far as this data is concerned, and a subscription's span says nothing
	// about renewals that have not happened yet.
	if targetYM := year + "-" + month; targetYM >= sourceYM {
		return providerSnapshot{}, false
	}

	var kept []providerSubscription
	for _, sub := range source.Subscriptions {
		if subscriptionCoversMonth(sub, year, month) {
			kept = append(kept, sub)
		}
	}
	if len(kept) == 0 {
		return providerSnapshot{}, false
	}

	return providerSnapshot{
		Provider:      source.Provider,
		FetchedAt:     source.FetchedAt,
		Subscriptions: kept,
		Derived:       true,
		DerivedFrom:   sourceYM,
	}, true
}

// subscriptionCoversMonth reports whether a subscription's active span overlaps
// the given month. Dates are ISO-8601 (YYYY-MM-DD…), so a lexicographic compare
// against the month's first and last day is exact and needs no parsing.
//
// A missing end date means open-ended — an active subscription with no next
// invoice scheduled still covers every month from its start onwards.
func subscriptionCoversMonth(sub providerSubscription, year, month string) bool {
	start := isoDatePart(sub.CurrentPeriodStart)
	if start == "" {
		start = isoDatePart(sub.CreatedAt)
	}
	if start == "" {
		return false
	}
	firstDay := fmt.Sprintf("%s-%s-01", year, month)
	lastDay := fmt.Sprintf("%s-%s-31", year, month)

	if start > lastDay {
		return false
	}
	end := isoDatePart(sub.CurrentPeriodEnd)
	if end == "" {
		return true
	}
	return end >= firstDay
}

// isoDatePart trims an ISO timestamp down to its YYYY-MM-DD prefix, so a plain
// date and an RFC3339 timestamp compare the same way.
func isoDatePart(s string) string {
	s = strings.TrimSpace(s)
	if len(s) < 10 {
		return ""
	}
	return s[:10]
}

// newestOdooSnapshot returns the most recent Odoo membership snapshot on disk
// along with the YYYY-MM it belongs to. "Most recent" is by month, not by file
// mtime: a re-run of an old month must not become the source of truth for every
// month before it.
func newestOdooSnapshot(dataDir string) (providerSnapshot, string, bool) {
	years, err := os.ReadDir(dataDir)
	if err != nil {
		return providerSnapshot{}, "", false
	}
	var months []string
	for _, y := range years {
		if !y.IsDir() || !isYearDir(y.Name()) {
			continue
		}
		entries, err := os.ReadDir(fmt.Sprintf("%s/%s", dataDir, y.Name()))
		if err != nil {
			continue
		}
		for _, m := range entries {
			if m.IsDir() && len(m.Name()) == 2 {
				months = append(months, y.Name()+"-"+m.Name())
			}
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(months)))

	for _, ym := range months {
		parts := strings.SplitN(ym, "-", 2)
		path := odoosource.Path(dataDir, parts[0], parts[1], odoosource.SubscriptionsFile)
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var snap providerSnapshot
		if json.Unmarshal(data, &snap) != nil || len(snap.Subscriptions) == 0 {
			continue
		}
		// A snapshot that is itself derived can't seed another derivation —
		// that would let one month's approximation propagate silently.
		if snap.Derived {
			continue
		}
		return snap, ym, true
	}
	return providerSnapshot{}, "", false
}
