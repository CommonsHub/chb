package cmd

import (
	"testing"
	"time"

	stripesource "github.com/CommonsHub/chb/providers/stripe"
)

func ts(s string) int64 {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		panic(err)
	}
	return t.Unix()
}

func ptr(v int64) *int64 { return &v }

// TestStripeSubscriptionRanInMonth pins the historical-membership rule. The
// regression it guards: the filter used to require the CURRENT billing period
// to overlap the target month, so a monthly subscription synced in September
// was reported as absent from May, June and July even though it never lapsed —
// 36 members vanishing between August and July.
func TestStripeSubscriptionRanInMonth(t *testing.T) {
	// Target month: May 2026.
	monthStart := ts("2026-05-01")
	lastDay := ts("2026-05-31")

	monthlySyncedInSeptember := stripesource.Subscription{
		Status:             "active",
		Created:            ts("2025-11-01"),
		CurrentPeriodStart: ts("2026-09-05"),
		CurrentPeriodEnd:   ts("2026-10-05"),
	}

	for _, tc := range []struct {
		name string
		sub  stripesource.Subscription
		want bool
	}{
		{"active monthly whose current period is months later", monthlySyncedInSeptember, true},
		{"active yearly spanning the month", stripesource.Subscription{
			Status: "active", Created: ts("2025-09-01"),
			CurrentPeriodStart: ts("2025-09-01"), CurrentPeriodEnd: ts("2026-09-01"),
		}, true},
		{"trialing counts", stripesource.Subscription{Status: "trialing", Created: ts("2026-04-01")}, true},
		{"past_due counts", stripesource.Subscription{Status: "past_due", Created: ts("2026-04-01")}, true},
		{"created after the month ends", stripesource.Subscription{
			Status: "active", Created: ts("2026-06-15"),
		}, false},
		{"created inside the month", stripesource.Subscription{
			Status: "active", Created: ts("2026-05-20"),
		}, true},
		{"canceled during the month still counts", stripesource.Subscription{
			Status: "canceled", Created: ts("2025-01-01"), CanceledAt: ptr(ts("2026-05-14")),
		}, true},
		{"canceled before the month does not", stripesource.Subscription{
			Status: "canceled", Created: ts("2025-01-01"), CanceledAt: ptr(ts("2026-04-14")),
		}, false},
		{"canceled falls back to endedAt", stripesource.Subscription{
			Status: "canceled", Created: ts("2025-01-01"), EndedAt: ptr(ts("2026-05-14")),
		}, true},
		{"incomplete never counts", stripesource.Subscription{
			Status: "incomplete", Created: ts("2025-01-01"),
		}, false},
		{"unpaid never counts", stripesource.Subscription{
			Status: "unpaid", Created: ts("2025-01-01"),
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := stripeSubscriptionRanInMonth(tc.sub, monthStart, lastDay); got != tc.want {
				t.Errorf("stripeSubscriptionRanInMonth() = %v, want %v", got, tc.want)
			}
		})
	}
}
