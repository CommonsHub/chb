package cmd

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

// AccountBalance prints the balance of a single account at the end of
// the supplied period:
//
//	chb accounts <slug> balance              → today (end of day)
//	chb accounts <slug> balance 2025         → end of 2025
//	chb accounts <slug> balance 2025/12      → end of December 2025
//	chb accounts <slug> balance 2025/12/31   → end of that day
//
// All timestamps are interpreted in Europe/Brussels. The balance is
// computed from the locally-cached transactions (the same ones that
// feed `chb accounts <slug>` and the Odoo sync), so it reflects what
// `chb accounts <slug> sync` last produced — re-run sync first if a
// fresh on-chain reading is needed.
func AccountBalance(slug string, args []string) error {
	if HasFlag(args, "--help", "-h", "help") {
		printAccountBalanceHelp()
		return nil
	}

	var acc *AccountConfig
	for _, a := range LoadAccountConfigs() {
		if strings.EqualFold(a.Slug, slug) {
			acc = &a
			break
		}
	}
	if acc == nil {
		return fmt.Errorf("account %q not found", slug)
	}

	cutoff, scope, err := parseAccountBalanceCutoff(args)
	if err != nil {
		return err
	}

	balance, counted, future, latest := accountBalanceAtCutoff(acc, cutoff)

	currency := accCurrency(acc)
	fmt.Printf("\n%s%s — %s%s\n", Fmt.Bold, acc.Slug, acc.Name, Fmt.Reset)
	fmt.Printf("  %sBalance at end of %s:%s %s%s %s\n",
		Fmt.Dim, scope, Fmt.Reset, signPrefix(balance), fmtNumber(math.Abs(balance)), currency)
	if opening, ok := acc.OpeningBalanceAt(cutoff); ok {
		asOf := "the start of the archive"
		if t, dated := acc.OpeningBalanceAsOfTime(); dated {
			asOf = t.Format("2006-01-02")
		}
		fmt.Printf("  %sOpening balance at %s:%s %s%s %s\n",
			Fmt.Dim, asOf, Fmt.Reset, signPrefix(opening), fmtNumber(math.Abs(opening)), currency)
	}
	fmt.Printf("  %sBased on %s",
		Fmt.Dim, Pluralize(counted, "local transaction", ""))
	if latest.Unix() > 0 {
		fmt.Printf(" (latest %s)", latest.In(BrusselsTZ()).Format("2006-01-02 15:04"))
	}
	if future > 0 {
		fmt.Printf(", %s ignored after the cutoff", Pluralize(future, "tx", ""))
	}
	fmt.Printf("%s\n", Fmt.Reset)

	if HasFlag(args, "--onchain") {
		printOnchainComparison(acc, cutoff, balance, currency)
	}
	if HasFlag(args, "--derive-opening") {
		printDerivedOpeningBalance(acc, currency)
	}
	fmt.Println()
	return nil
}

// printDerivedOpeningBalance works backwards from the account's live balance to
// the opening balance that would reconcile it with chb's archive: whatever the
// account already held before the earliest transaction chb recorded. It only
// reports the figure — the value belongs in accounts.json, which is a
// deliberate, reviewable decision rather than something a balance query should
// write on its own.
func printDerivedOpeningBalance(acc *AccountConfig, currency string) {
	live, key, err := refreshAccountBalance(acc)
	if err != nil {
		Warnf("  %s⚠ cannot derive an opening balance: %v%s", Fmt.Yellow, err, Fmt.Reset)
		return
	}
	if key == "" {
		Warnf("  %s⚠ cannot derive an opening balance: %s has no live balance source%s",
			Fmt.Yellow, acc.Slug, Fmt.Reset)
		return
	}

	// Everything chb has archived, with any configured opening removed — the
	// derivation must not build on the number it is deriving.
	now := time.Now().In(BrusselsTZ())
	booked, counted, _, _ := accountBalanceAtCutoff(acc, endOfDay(now))
	if opening, ok := acc.OpeningBalanceAt(endOfDay(now)); ok {
		booked = roundCents(booked - opening)
	}
	implied := roundCents(live - booked)
	asOf := earliestAccountTransaction(acc)

	fmt.Printf("\n  %sDeriving the opening balance%s\n", Fmt.Bold, Fmt.Reset)
	fmt.Printf("    %sLive balance:%s        %s%s %s\n",
		Fmt.Dim, Fmt.Reset, signPrefix(live), fmtNumber(math.Abs(live)), currency)
	fmt.Printf("    %sBooked in archive:%s   %s%s %s  %s(%s",
		Fmt.Dim, Fmt.Reset, signPrefix(booked), fmtNumber(math.Abs(booked)), currency,
		Fmt.Dim, Pluralize(counted, "tx", ""))
	if !asOf.IsZero() {
		fmt.Printf(", earliest %s", asOf.Format("2006-01-02"))
	}
	fmt.Printf(")%s\n", Fmt.Reset)
	fmt.Printf("    %s→ openingBalance:%s    %s%s%s %s %s\n",
		Fmt.Dim, Fmt.Reset, Fmt.Bold, signPrefix(implied), fmtNumber(math.Abs(implied)), currency, Fmt.Reset)

	if counted == 0 {
		fmt.Printf("    %sNo local transactions — this is just the live balance, not a derivation.%s\n", Fmt.Dim, Fmt.Reset)
		return
	}
	// The live balance is current; the archive is only as current as the last
	// pull. Anything that happened since lands in the live figure alone and is
	// silently absorbed into the opening balance, which would then be wrong by
	// exactly that amount — and wrong permanently, since it gets written down.
	fmt.Printf("\n    %s⚠ Pull the account first: activity newer than the archive is%s\n", Fmt.Yellow, Fmt.Reset)
	fmt.Printf("    %s  counted into this figure instead of being booked.%s\n", Fmt.Yellow, Fmt.Reset)
	fmt.Printf("\n    %sAdd to this account in accounts.json (the shipped default —%s\n", Fmt.Dim, Fmt.Reset)
	fmt.Printf("    %sa local-only edit is overwritten on the next run):%s\n", Fmt.Dim, Fmt.Reset)
	fmt.Printf("      %s\"openingBalance\": %.2f,%s\n", Fmt.Cyan, implied, Fmt.Reset)
	if !asOf.IsZero() {
		fmt.Printf("      %s\"openingBalanceAsOf\": \"%s\"%s\n", Fmt.Cyan, asOf.Format("2006-01-02"), Fmt.Reset)
	}
}

// earliestAccountTransaction returns the timestamp of the oldest transaction
// chb holds for the account — the point its archive begins, and so the date an
// opening balance is measured at. Zero when there are none.
func earliestAccountTransaction(acc *AccountConfig) time.Time {
	var earliest time.Time
	for _, tx := range loadAccountTransactionsForOdoo(acc) {
		t := time.Unix(tx.Timestamp, 0).In(BrusselsTZ())
		if earliest.IsZero() || t.Before(earliest) {
			earliest = t
		}
	}
	return earliest
}

// printOnchainComparison reads the account's balance from the chain at the same
// cutoff and reports the difference. This is the reconciliation question the
// local figure cannot answer on its own: whether chb's archive actually
// accounts for everything the wallet did.
func printOnchainComparison(acc *AccountConfig, cutoff time.Time, local float64, currency string) {
	reading, err := onchainBalanceAt(acc, cutoff)
	if err != nil {
		Warnf("  %s⚠ on-chain reading unavailable: %v%s", Fmt.Yellow, err, Fmt.Reset)
		return
	}

	at := "latest"
	if reading.Block != "latest" {
		at = "block " + reading.Block
	}
	fmt.Printf("  %sOn-chain at %s:%s %s%s %s  %s(%s)%s\n",
		Fmt.Dim, at, Fmt.Reset,
		signPrefix(reading.Balance), fmtNumber(math.Abs(reading.Balance)), currency,
		Fmt.Dim, reading.Source, Fmt.Reset)

	diff := roundCents(reading.Balance - local)
	if diff == 0 {
		fmt.Printf("  %s✓ matches the local archive%s\n", Fmt.Green, Fmt.Reset)
		return
	}
	fmt.Printf("  %s⚠ differs by %s%s %s — the local archive is missing or double-counting transactions%s\n",
		Fmt.Yellow, signPrefix(diff), fmtNumber(math.Abs(diff)), currency, Fmt.Reset)

	// A pruned node answers a query for state it no longer holds with a zero
	// instead of an error, which would otherwise read as "the wallet was
	// empty". Say so rather than letting the number stand unqualified.
	if reading.Historical && reading.Balance == 0 && local != 0 && reading.Source != "etherscan" {
		fmt.Printf("    %sA zero from %s may mean the node has pruned state that old rather than an empty wallet.%s\n",
			Fmt.Dim, reading.Source, Fmt.Reset)
	}
}

// accountBalanceAtCutoff sums the signed amounts of an account's locally-cached
// transactions up to and including the cutoff. It returns the rounded balance,
// the number of transactions counted, the number ignored after the cutoff, and
// the timestamp of the latest counted transaction.
//
// A configured opening balance seeds the running total, so an account whose
// archive starts partway through its life reports what it actually holds
// rather than the movement chb happens to have recorded.
func accountBalanceAtCutoff(acc *AccountConfig, cutoff time.Time) (balance float64, counted, future int, latest time.Time) {
	if opening, ok := acc.OpeningBalanceAt(cutoff); ok {
		balance = opening
	}
	for _, tx := range loadAccountTransactionsForOdoo(acc) {
		t := time.Unix(tx.Timestamp, 0)
		if t.After(cutoff) {
			future++
			continue
		}
		balance += signedOdooAmountForTransaction(acc, tx)
		counted++
		if t.After(latest) {
			latest = t
		}
	}
	return roundCents(balance), counted, future, latest
}

// AccountsBalance prints the balance of every configured account at the end of
// the supplied period, plus a per-currency total. It is the aggregate form of
// `chb accounts <slug> balance` and accepts the same date argument:
//
//	chb accounts balance              → today (end of day)
//	chb accounts balance 2025         → end of 2025
//	chb accounts balance 2025/12      → end of December 2025
//	chb accounts balance 2025/12/31   → end of that day
//
// Balances come from locally-cached transactions (the same source as the
// single-account view) — run `chb accounts pull` first for fresh data.
func AccountsBalance(args []string) error {
	if HasFlag(args, "--help", "-h", "help") {
		printAccountsBalanceHelp()
		return nil
	}

	cutoff, scope, err := parseAccountBalanceCutoff(args)
	if err != nil {
		return err
	}

	configs := LoadAccountConfigs()
	if len(configs) == 0 {
		fmt.Printf("\n%sNo accounts configured.%s\n\n", Fmt.Dim, Fmt.Reset)
		return nil
	}

	verbose := HasFlag(args, "--verbose", "-v")

	// --refresh fetches each account's live balance from its own source
	// (on-chain balanceOf, the Stripe balance, the bank's running balance) and
	// caches it. Those cached figures are what anchor the monthly report's
	// opening/ending balances: without them the report can only roll a total
	// forward from chb's first recorded transaction, which is wrong by
	// whatever the account already held on that day.
	if HasFlag(args, "--refresh") {
		fetched := 0
		for i := range configs {
			before := len(liveBalanceKeys())
			refreshAndPersistAccountBalance(&configs[i])
			if len(liveBalanceKeys()) > before {
				fetched++
			}
		}
		fmt.Printf("  %sRefreshed live balances for %s%s\n",
			Fmt.Dim, Pluralize(fetched, "account", ""), Fmt.Reset)
	}

	type balanceRow struct {
		code     string
		slug     string
		currency string
		balance  float64
		counted  int
		latest   time.Time
	}

	rows := make([]balanceRow, 0, len(configs))
	totals := map[string]float64{}
	labelWidth := 0
	codeWidth := 0

	for i := range configs {
		acc := &configs[i]
		balance, counted, _, latest := accountBalanceAtCutoff(acc, cutoff)
		currency := accCurrency(acc)
		code := acc.OdooGlAccountCode
		if code == "" {
			code = "-"
		}
		rows = append(rows, balanceRow{code, acc.Slug, currency, balance, counted, latest})
		totals[currency] += balance
		if len(acc.Slug) > labelWidth {
			labelWidth = len(acc.Slug)
		}
		if len(code) > codeWidth {
			codeWidth = len(code)
		}
	}

	currencies := make([]string, 0, len(totals))
	for c := range totals {
		currencies = append(currencies, c)
	}
	sort.Strings(currencies)
	multiCurrency := len(currencies) > 1

	// "Total <CUR>" labels can be wider than the longest slug.
	for _, c := range currencies {
		if w := len("Total " + c); w > labelWidth {
			labelWidth = w
		}
	}

	// Right-align amounts on their plain (un-coloured) width.
	amtWidth := 0
	plainAmount := func(v float64, currency string) string {
		return signPrefix(v) + fmtNumber(math.Abs(v)) + " " + currency
	}
	for _, r := range rows {
		if w := len(plainAmount(r.balance, r.currency)); w > amtWidth {
			amtWidth = w
		}
	}
	for _, c := range currencies {
		if w := len(plainAmount(totals[c], c)); w > amtWidth {
			amtWidth = w
		}
	}

	// Rows lead with the GL account number (dim), then the slug, then the
	// right-aligned balance: "<number> <slug> <balance>". Total rows pass an
	// empty code so they align under the slug column.
	printRow := func(code, label string, v float64, currency string) {
		plain := plainAmount(v, currency)
		pad := strings.Repeat(" ", amtWidth-len(plain))
		colour := Fmt.Green
		if v < 0 {
			colour = Fmt.Red
		}
		fmt.Printf("  %s%-*s%s  %s%-*s%s  %s%s%s%s\n",
			Fmt.Dim, codeWidth, code, Fmt.Reset,
			Fmt.Bold, labelWidth, label, Fmt.Reset,
			pad, colour, plain, Fmt.Reset)
	}

	fmt.Printf("\n%s💰 Balances at end of %s%s  %s(%s)%s\n\n",
		Fmt.Bold, scope, Fmt.Reset, Fmt.Dim, Pluralize(len(configs), "account", ""), Fmt.Reset)

	for _, r := range rows {
		printRow(r.code, r.slug, r.balance, r.currency)
		if verbose {
			detail := fmt.Sprintf("%s based on %s", Fmt.Dim, Pluralize(r.counted, "local transaction", ""))
			if !r.latest.IsZero() {
				detail += fmt.Sprintf(" (latest %s)", r.latest.In(BrusselsTZ()).Format("2006-01-02"))
			}
			fmt.Printf("    %s%s\n", detail, Fmt.Reset)
		} else if r.counted == 0 {
			fmt.Printf("    %sno local transactions — run `chb accounts %s pull`%s\n", Fmt.Dim, r.slug, Fmt.Reset)
		}
	}

	fmt.Println()
	for _, c := range currencies {
		label := "Total"
		if multiCurrency {
			label = "Total " + c
		}
		printRow("", label, totals[c], c)
	}
	fmt.Printf("\n  %sBased on locally-cached transactions. Run %schb accounts <slug> balance %s%s%s for per-account detail.%s\n\n",
		Fmt.Dim, Fmt.Reset+Fmt.Cyan, scope, Fmt.Reset, Fmt.Dim, Fmt.Reset)
	return nil
}

// parseAccountBalanceCutoff turns the positional date argument into a
// concrete cutoff time (end-of-period in Brussels TZ). With no arg, the
// cutoff is end of today.
func parseAccountBalanceCutoff(args []string) (time.Time, string, error) {
	tz := BrusselsTZ()
	now := time.Now().In(tz)

	raw := ""
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			continue
		}
		raw = a
		break
	}
	if raw == "" {
		eod := endOfDay(now)
		return eod, eod.Format("2006-01-02"), nil
	}

	// Accept YYYY, YYYY/MM, YYYY/MM/DD with `/`, `-`, or no separator.
	canonical := strings.ReplaceAll(raw, "-", "/")
	if !strings.Contains(canonical, "/") && len(canonical) == 8 {
		canonical = canonical[:4] + "/" + canonical[4:6] + "/" + canonical[6:]
	} else if !strings.Contains(canonical, "/") && len(canonical) == 6 {
		canonical = canonical[:4] + "/" + canonical[4:]
	}
	parts := strings.Split(canonical, "/")

	yr, err := strconv.Atoi(parts[0])
	if err != nil || len(parts[0]) != 4 {
		return time.Time{}, "", fmt.Errorf("bad year %q — expected YYYY[/MM[/DD]]", raw)
	}

	switch len(parts) {
	case 1:
		eoy := time.Date(yr, 12, 31, 23, 59, 59, 0, tz)
		return eoy, eoy.Format("2006"), nil
	case 2:
		mo, err := strconv.Atoi(parts[1])
		if err != nil || mo < 1 || mo > 12 {
			return time.Time{}, "", fmt.Errorf("bad month %q in %q", parts[1], raw)
		}
		// Last day of month, 23:59:59 — Go normalises Year-Mo-32 → next month.
		firstOfNext := time.Date(yr, time.Month(mo)+1, 1, 0, 0, 0, 0, tz)
		eom := firstOfNext.Add(-time.Second)
		return eom, eom.Format("2006-01"), nil
	case 3:
		mo, err := strconv.Atoi(parts[1])
		if err != nil || mo < 1 || mo > 12 {
			return time.Time{}, "", fmt.Errorf("bad month %q in %q", parts[1], raw)
		}
		day, err := strconv.Atoi(parts[2])
		if err != nil || day < 1 || day > 31 {
			return time.Time{}, "", fmt.Errorf("bad day %q in %q", parts[2], raw)
		}
		t := time.Date(yr, time.Month(mo), day, 23, 59, 59, 0, tz)
		// Reject obviously-bad days like 2025/02/30 — Go will roll them
		// over silently otherwise.
		if t.Month() != time.Month(mo) || t.Day() != day {
			return time.Time{}, "", fmt.Errorf("invalid date %q", raw)
		}
		return t, t.Format("2006-01-02"), nil
	}
	return time.Time{}, "", fmt.Errorf("unrecognised date %q — expected YYYY[/MM[/DD]]", raw)
}

func endOfDay(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 23, 59, 59, 0, t.Location())
}

func printAccountBalanceHelp() {
	f := Fmt
	fmt.Printf(`
%schb accounts <slug> balance%s — Historical balance of one account

%sUSAGE%s
  %schb accounts <slug> balance%s              End of today
  %schb accounts <slug> balance%s 2025         End of 2025
  %schb accounts <slug> balance%s 2025/12      End of December 2025
  %schb accounts <slug> balance%s 2025/12/31   End of that day

%sNOTES%s
  • Computed from locally-cached transactions (run %schb accounts <slug> sync%s
    first for fresh data). Dates parsed in Europe/Brussels.
  • Accepts %sYYYY%s, %sYYYY/MM%s, %sYYYY/MM/DD%s (or %s-%s / %sYYYYMMDD%s).

%sOPTIONS%s
  %s--onchain%s            Also read the balance from the chain at the same
                       cutoff and show the difference (etherscan accounts)
  %s--derive-opening%s     Work backwards from the live balance to the opening
                       balance that would reconcile it with the archive
  %s--help, -h%s           Show this help

%sON-CHAIN READINGS%s
  A past date is resolved to the last block before it, then read at that
  block. Etherscan serves historical balances only to API Pro keys, so a
  free key falls back to an archive-node %seth_call%s — public RPCs keep
  recent history and prune older state.

%sOPENING BALANCE%s
  An account whose archive starts partway through its life is short by
  whatever it already held. Set %sopeningBalance%s (and %sopeningBalanceAsOf%s)
  on the account to seed the running total; %s--derive-opening%s computes the
  figure from the live balance.
`,
		f.Bold, f.Reset,
		f.Bold, f.Reset,
		f.Cyan, f.Reset,
		f.Cyan, f.Reset,
		f.Cyan, f.Reset,
		f.Cyan, f.Reset,
		f.Bold, f.Reset,
		f.Cyan, f.Reset,
		f.Yellow, f.Reset, f.Yellow, f.Reset, f.Yellow, f.Reset, f.Yellow, f.Reset, f.Yellow, f.Reset,
		f.Bold, f.Reset,
		f.Yellow, f.Reset,
		f.Yellow, f.Reset,
		f.Yellow, f.Reset,
		f.Bold, f.Reset,
		f.Cyan, f.Reset,
		f.Bold, f.Reset,
		f.Cyan, f.Reset, f.Cyan, f.Reset, f.Cyan, f.Reset,
	)
}

func printAccountsBalanceHelp() {
	f := Fmt
	fmt.Printf(`
%schb accounts balance%s — Historical balance of every account

%sUSAGE%s
  %schb accounts balance%s              End of today
  %schb accounts balance%s 2025         End of 2025
  %schb accounts balance%s 2025/12      End of December 2025
  %schb accounts balance%s 2025/12/31   End of that day

%sNOTES%s
  • Aggregates %schb accounts <slug> balance%s across all accounts, with a
    per-currency total. Computed from locally-cached transactions (run
    %schb accounts pull%s first for fresh data). Dates parsed in Europe/Brussels.
  • Accepts %sYYYY%s, %sYYYY/MM%s, %sYYYY/MM/DD%s (or %s-%s / %sYYYYMMDD%s).

%sOPTIONS%s
  %s--verbose, -v%s        Show tx count and latest tx date per account
  %s--help, -h%s           Show this help
`,
		f.Bold, f.Reset,
		f.Bold, f.Reset,
		f.Cyan, f.Reset,
		f.Cyan, f.Reset,
		f.Cyan, f.Reset,
		f.Cyan, f.Reset,
		f.Bold, f.Reset,
		f.Cyan, f.Reset,
		f.Cyan, f.Reset,
		f.Yellow, f.Reset, f.Yellow, f.Reset, f.Yellow, f.Reset, f.Yellow, f.Reset, f.Yellow, f.Reset,
		f.Bold, f.Reset,
		f.Cyan, f.Reset,
		f.Cyan, f.Reset,
	)
}

// signPrefix returns "+" / "-" / "" so we can build "<sign><amount> <currency>"
// without the hard-coded "EUR" suffix that fmtEURSigned tacks on.
func signPrefix(v float64) string {
	if v > 0 {
		return "+"
	}
	if v < 0 {
		return "-"
	}
	return ""
}

// liveBalanceKeys lists the accounts currently present in the live-balance
// cache. Used to report how many a refresh actually managed to fetch.
func liveBalanceKeys() []string {
	cache := loadBalanceCache()
	if cache == nil {
		return nil
	}
	keys := make([]string, 0, len(cache.Balances))
	for k := range cache.Balances {
		keys = append(keys, k)
	}
	return keys
}
