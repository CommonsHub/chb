package cmd

import (
	"sort"
	"strings"
)

// Account balance reconciliation.
//
// Until now every account row in the monthly report printed "start n/a end
// n/a": MonthlyReportBalance carried Opening/Ending/Computed/Verified fields
// that nothing ever filled, and the report shipped a note admitting it.
//
// The transaction history alone cannot produce a balance — it only produces a
// *delta* per month, and a delta is meaningless without an anchor. The anchor
// is the live balance chb already fetches per account (on-chain balanceOf, the
// Stripe balance, the bank's own running balance) and caches in
// latest/balances.json. Rolling that anchor backwards through the monthly
// deltas gives every month boundary:
//
//	ending(M)  = anchor - sum(net of every month after M)
//	opening(M) = ending(M) - net(M)
//
// The reconciliation proper is the comparison this makes possible. Summing
// every transaction chb holds for an account gives how much of the live balance
// the records account for; the remainder is the balance the account already
// carried before chb's history begins.
//
// That residual is NOT by itself an error. chb's data starts at a particular
// month, and everything the account held on that day is legitimately outside
// it. It only signals a problem when the history is supposed to be complete —
// so it is reported as "predates our records" rather than as a discrepancy,
// and the reader is left to judge which it is.

// accountReconciliation is one account's balance picture across all months.
type accountReconciliation struct {
	// Anchor is the live balance and whether one was available at all.
	Anchor    float64
	HasAnchor bool
	// BookedTotal is the sum of every recorded transaction for the account.
	BookedTotal float64
	// PreHistory is Anchor - BookedTotal: the part of today's balance that
	// chb's transaction history does not cover. Normally the opening balance
	// from before the records start; a surprise only if the history was
	// expected to be complete.
	PreHistory float64
}

// reconciledAccountKey identifies an account across months. It mirrors the
// grouping key used when the report's account rows are built, so a row and its
// balance always refer to the same thing.
type reconciledAccountKey struct {
	source, chain, identity, currency string
}

func accountRowKey(a MonthlyReportAccount) reconciledAccountKey {
	identity := a.AccountSlug
	if identity == "" {
		identity = a.Account
	}
	return reconciledAccountKey{
		source:   a.Source,
		chain:    a.Chain,
		identity: identity,
		currency: strings.ToUpper(a.Currency),
	}
}

// liveAccountAnchors maps each configured account onto its cached live balance.
// Accounts with no cached balance are absent: an anchor that does not exist is
// different from an anchor of zero, and guessing zero would invent a
// reconciliation failure out of nothing.
func liveAccountAnchors() map[reconciledAccountKey]float64 {
	cache := loadBalanceCache()
	if cache == nil || len(cache.Balances) == 0 {
		return nil
	}
	out := map[reconciledAccountKey]float64{}
	for i := range LoadAccountConfigs() {
		acc := LoadAccountConfigs()[i]
		currency := accountConfigCurrency(&acc)
		for _, key := range accountBalanceLookupKeys(&acc) {
			v, ok := cache.Balances[key]
			if !ok {
				continue
			}
			out[reconciledAccountKey{
				source:   acc.Provider,
				chain:    acc.Chain,
				identity: acc.Slug,
				currency: strings.ToUpper(currency),
			}] = v
			break
		}
	}
	return out
}

// monthlyAccountDeltas is the per-month net movement of one account, in
// chronological order.
type monthlyAccountDelta struct {
	ym  string
	net float64
}

// reconcileAccountBalances walks the ordered per-month deltas for every account
// and fills in Opening/Ending. Returns the per-account reconciliation so the
// caller can surface what the books do not explain.
//
// Anchored accounts are rolled backwards from the live balance. Accounts
// without an anchor fall back to a running total from the first month chb
// holds, which is still useful — the shape of the balance is right even when
// its absolute level is not — and is marked Computed but never Verified.
func reconcileAccountBalances(
	deltas map[reconciledAccountKey][]monthlyAccountDelta,
	anchors map[reconciledAccountKey]float64,
) (map[reconciledAccountKey]map[string]MonthlyReportBalance, map[reconciledAccountKey]accountReconciliation) {

	balances := map[reconciledAccountKey]map[string]MonthlyReportBalance{}
	recon := map[reconciledAccountKey]accountReconciliation{}

	for key, series := range deltas {
		sort.Slice(series, func(i, j int) bool { return series[i].ym < series[j].ym })

		var booked float64
		for _, d := range series {
			booked += d.net
		}
		booked = roundReportAmount(booked)

		anchor, hasAnchor := anchors[key]
		r := accountReconciliation{Anchor: anchor, HasAnchor: hasAnchor, BookedTotal: booked}
		if hasAnchor {
			r.PreHistory = roundReportAmount(anchor - booked)
		}
		recon[key] = r

		// Walk newest to oldest when anchored: each month's ending balance is
		// the anchor minus everything that happened after it.
		months := map[string]MonthlyReportBalance{}
		if hasAnchor {
			running := anchor
			for i := len(series) - 1; i >= 0; i-- {
				d := series[i]
				ending := roundReportAmount(running)
				opening := roundReportAmount(ending - d.net)
				months[d.ym] = MonthlyReportBalance{
					Opening:  floatPtr(opening),
					Ending:   floatPtr(ending),
					Delta:    roundReportAmount(d.net),
					Computed: true,
					Anchored: true,
					// Verified means an external source vouched for this exact
					// month end. The live balance vouches for the most recent
					// month and nothing else: every earlier boundary is
					// inferred by subtracting deltas from it.
					Verified: i == len(series)-1,
				}
				running = opening
			}
		} else {
			running := 0.0
			for _, d := range series {
				opening := roundReportAmount(running)
				ending := roundReportAmount(opening + d.net)
				months[d.ym] = MonthlyReportBalance{
					Opening:  floatPtr(opening),
					Ending:   floatPtr(ending),
					Delta:    roundReportAmount(d.net),
					Computed: true,
					Verified: false,
				}
				running = ending
			}
		}
		balances[key] = months
	}
	return balances, recon
}

func floatPtr(v float64) *float64 { return &v }
