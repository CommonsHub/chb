package cmd

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/CommonsHub/chb/poster"
	odoosource "github.com/CommonsHub/chb/providers/odoo"
)

// AnnualBudget is the optional settings/budget.json. Amounts are for the whole
// year, in EUR. Absent means the report shows a run-rate projection instead of
// a variance.
type AnnualBudget struct {
	Year       string             `json:"year"`
	Income     float64            `json:"income"`
	Expenses   float64            `json:"expenses"`
	Notes      string             `json:"notes,omitempty"`
	ByCategory map[string]float64 `json:"byCategory,omitempty"`
}

// LoadAnnualBudget reads settings/budget.json for the given year. A missing
// file is not an error — most installs won't have one.
func LoadAnnualBudget(year string) (*AnnualBudget, bool) {
	data, err := os.ReadFile(settingsFilePath("budget.json"))
	if err != nil {
		return nil, false
	}
	var b AnnualBudget
	if json.Unmarshal(data, &b) != nil {
		return nil, false
	}
	if b.Year != "" && b.Year != year {
		return nil, false
	}
	if b.Income == 0 && b.Expenses == 0 {
		return nil, false
	}
	return &b, true
}

// BuildMemberReport assembles the month's figures. It reads only generated/
// files; run `chb generate` first if they are stale.
func BuildMemberReport(dataDir, year, month string) (*poster.Report, error) {
	summary, err := loadMonthlyReportFile(dataDir, year, month)
	if err != nil {
		return nil, err
	}

	r := &poster.Report{
		Year:         year,
		Month:        month,
		MonthLabel:   monthLabel(year, month),
		Contributors: summary.Summary.Contributors,
		Events:       summary.Summary.Events,
		Bookings:     summary.Summary.Bookings,
	}

	// Money in/out for the month, split by line of business. Categories
	// already merge the EUR family and fold fees and VAT into `out`, so the
	// three segments sum to the same totals the terminal report prints.
	lines := LoadBusinessLines()
	r.HubLine.Name = "Running the hub"
	r.EventLine.Name = "The event business"
	r.SharedLine.Name = "Shared and unassigned"

	for _, cat := range summary.Categories {
		// The host commission is a synthetic transfer chb derives between
		// collectives, not money anyone paid the hub. Counting it made the
		// business lines total 10,040.26 EUR against 9,971.76 EUR of euro
		// that actually arrived — a 68.50 EUR gap with no transaction behind
		// it.
		if strings.EqualFold(cat.Slug, commissionCategorySlug) {
			continue
		}
		flow, ok := eurFlow(cat)
		if !ok {
			continue
		}
		r.Income += flow.In
		r.Expenses += flow.Out
		label := memberReportCategoryLabel(cat.Slug)
		if flow.In > 0 {
			r.IncomeLines = append(r.IncomeLines, poster.Line{Label: label, Amount: flow.In})
		}
		if flow.Out > 0 {
			r.ExpenseLines = append(r.ExpenseLines, poster.Line{Label: label, Amount: flow.Out})
		}

		seg := segmentFor(r, lines.LineFor(cat.Slug))
		seg.Income += flow.In
		seg.Expenses += flow.Out
		if flow.In > 0 {
			seg.In = append(seg.In, poster.Line{Label: label, Amount: flow.In})
		}
		if flow.Out > 0 {
			seg.Out = append(seg.Out, poster.Line{Label: label, Amount: flow.Out})
		}
	}
	for _, seg := range []*poster.Segment{&r.HubLine, &r.EventLine, &r.SharedLine} {
		seg.Income = roundReportAmount(seg.Income)
		seg.Expenses = roundReportAmount(seg.Expenses)
		seg.Net = roundReportAmount(seg.Income - seg.Expenses)
		finishLines(seg.In, seg.Income)
		finishLines(seg.Out, seg.Expenses)
		sortLines(seg.In)
		sortLines(seg.Out)
	}
	r.Net = roundReportAmount(r.Income - r.Expenses)
	r.Income = roundReportAmount(r.Income)
	r.Expenses = roundReportAmount(r.Expenses)
	finishLines(r.IncomeLines, r.Income)
	finishLines(r.ExpenseLines, r.Expenses)
	sortLines(r.IncomeLines)
	sortLines(r.ExpenseLines)

	for _, tok := range summary.Tokens {
		if tok.Transactions == 0 && tok.Minted == 0 && tok.Burnt == 0 {
			continue
		}
		r.Tokens = append(r.Tokens, poster.Token{
			Symbol: tok.Symbol, Minted: tok.Minted, Burnt: tok.Burnt,
			Supply: tok.TotalSupply, Holders: tok.TokenHolders, Active: tok.ActiveTokenHolders,
		})
	}

	// Membership.
	if mf, ok := loadMembersFile(dataDir, year, month); ok {
		r.ActiveMembers = mf.Summary.ActiveMembers
		r.MRR = mf.Summary.MRR.Value
		r.OdooDerived = mf.OdooDerived
	}
	if py, pm, ok := previousMonth(year, month); ok {
		if prev, ok := loadMembersFile(dataDir, py, pm); ok {
			r.MemberDelta = r.ActiveMembers - prev.Summary.ActiveMembers
			r.MRRDelta = roundReportAmount(r.MRR - prev.Summary.MRR.Value)
		}
	}

	for _, col := range summary.Collectives {
		// commonshub is the hub itself — this report *is* its accounting, so
		// it cannot also be a group the hub holds money for.
		if strings.EqualFold(col.Slug, commissionHostSlug) {
			continue
		}
		if flow, ok := eurFlow(col); ok && (flow.In > 0 || flow.Out > 0) {
			r.Collectives = append(r.Collectives, poster.Line{
				Label:  memberReportCollectiveLabel(col.Slug),
				Amount: roundReportAmount(flow.In - flow.Out),
				Share:  flow.In,
			})
		}
	}
	sort.Slice(r.Collectives, func(i, j int) bool { return r.Collectives[i].Share > r.Collectives[j].Share })

	for _, acct := range summary.Accounts {
		// Only anchored balances go on a page people will read as "what we
		// have". An unanchored one is a running total from chb's first
		// recorded transaction and can be wildly off — the checking account
		// rolls to minus 150,000 EUR that way.
		if acct.Balance.Ending == nil || !acct.Balance.Anchored {
			continue
		}
		label := acct.AccountSlug
		if label == "" {
			label = acct.Account
		}
		r.Balances = append(r.Balances, poster.Balance{
			Label:    memberReportAccountLabel(label, acct.AccountName),
			Currency: acct.Currency,
			Ending:   *acct.Balance.Ending,
			Change:   acct.Balance.Delta,
			Verified: acct.Balance.Verified,
		})
	}
	sort.Slice(r.Balances, func(i, j int) bool { return r.Balances[i].Ending > r.Balances[j].Ending })
	for _, acct := range summary.Accounts {
		if acct.Balance.Ending == nil || !acct.Balance.Anchored {
			continue
		}
		line := lines.AccountLineFor(acct.AccountSlug)
		if line == LineShared {
			continue
		}
		seg := segmentFor(r, line)
		seg.Accounts = append(seg.Accounts, poster.Balance{
			Label:    memberReportAccountLabel(acct.AccountSlug, acct.AccountName),
			Currency: acct.Currency,
			Ending:   *acct.Balance.Ending,
			Change:   acct.Balance.Delta,
			Verified: acct.Balance.Verified,
		})
	}
	for _, n := range summary.Notes {
		if strings.Contains(n, "balance") {
			r.BalanceNote = n
		}
	}

	r.EURToken = buildEURTokenStats(summary)
	r.VAT = buildVATPosition(dataDir, year, month, summary)
	r.FixedCosts, r.FixedCostTypical, r.FixedCostThisMonth = buildFixedCosts(dataDir, year, month)
	r.NetAfterFixed = roundReportAmount(r.Income - r.Expenses + r.FixedCostThisMonth - r.FixedCostTypical)

	// Attribute each commitment to the line that carries it, so "running the
	// hub" is costed with the rent whether or not the invoice arrived this
	// month. Without this the hub reads as comfortably positive in any month
	// its largest bill happens to be late.
	for _, fc := range r.FixedCosts {
		seg := segmentFor(r, lines.LineFor(fc.Slug))
		seg.FixedCosts = append(seg.FixedCosts, fc)
		seg.FixedTypical = roundReportAmount(seg.FixedTypical + fc.Typical)
		seg.FixedThisMonth = roundReportAmount(seg.FixedThisMonth + fc.ThisMonth)
	}
	for _, seg := range []*poster.Segment{&r.HubLine, &r.EventLine, &r.SharedLine} {
		seg.NetAfterFixed = roundReportAmount(seg.Net + seg.FixedThisMonth - seg.FixedTypical)
		if owed := seg.FixedTypical - seg.FixedThisMonth; owed > 0 {
			seg.Unbilled = roundReportAmount(owed)
		}
		seg.ExpensesInclUnbilled = roundReportAmount(seg.Expenses + seg.Unbilled)
	}

	r.Trend = buildMemberReportTrend(dataDir, year, month, 6)
	r.YTD = buildMemberReportYTD(dataDir, year, month)
	r.Outlier = findMemberReportOutlier(dataDir, year, month)
	r.Notables, r.Highlights = buildMemberReportNotables(dataDir, year, month)
	return r, nil
}

func loadMonthlyReportFile(dataDir, year, month string) (*MonthlyReportFile, error) {
	path := filepath.Join(dataDir, year, month, "generated", "summary.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("no generated report for %s/%s — run `chb generate %s/%s` first", year, month, year, month)
	}
	var f MonthlyReportFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("reading %s: %v", path, err)
	}
	return &f, nil
}

func loadMembersFile(dataDir, year, month string) (*MembersOutputFile, bool) {
	data, err := os.ReadFile(filepath.Join(dataDir, year, month, "generated", "members.json"))
	if err != nil {
		return nil, false
	}
	var mf MembersOutputFile
	if json.Unmarshal(data, &mf) != nil {
		return nil, false
	}
	return &mf, true
}

// eurFlow picks the EUR row out of a tagged summary. Non-EUR currencies (CHT)
// have their own section and must not be added to euro totals.
func eurFlow(t TaggedSummary) (CurrencyFlow, bool) {
	for _, c := range t.Currencies {
		if strings.EqualFold(c.Currency, "EUR") {
			return c, true
		}
	}
	return CurrencyFlow{}, false
}

func finishLines(lines []poster.Line, total float64) {
	if total <= 0 {
		return
	}
	for i := range lines {
		lines[i].Share = lines[i].Amount / total
	}
}

func sortLines(lines []poster.Line) {
	sort.Slice(lines, func(i, j int) bool {
		if lines[i].Amount != lines[j].Amount {
			return lines[i].Amount > lines[j].Amount
		}
		return lines[i].Label < lines[j].Label
	})
}

// memberReportCategoryLabel turns a slug into something a member can read.
// Falls back to a title-cased slug when categories.json has no label.
func memberReportCategoryLabel(slug string) string {
	if slug == "(untagged)" {
		return "Not yet categorised"
	}
	if settings, err := LoadSettings(); err == nil {
		if label := NewCategorizer(settings).CategoryLabel(slug); label != "" && label != slug {
			return label
		}
	}
	words := strings.FieldsFunc(slug, func(r rune) bool { return r == '_' || r == '-' })
	for i, w := range words {
		if w == "" {
			continue
		}
		words[i] = strings.ToUpper(w[:1]) + w[1:]
	}
	return strings.Join(words, " ")
}

func buildMemberReportTrend(dataDir, year, month string, months int) []poster.TrendPoint {
	var out []poster.TrendPoint
	y, m := year, month
	for i := 0; i < months; i++ {
		point := poster.TrendPoint{Label: shortMonthLabel(y, m)}
		if in, outAmt, ok := monthlyEURFlow(dataDir, y, m); ok {
			point.Net = roundReportAmount(in - outAmt)
			point.HasData = true
		}
		if mf, ok := loadMembersFile(dataDir, y, m); ok {
			point.Members = mf.Summary.ActiveMembers
			point.HasMembers = true
			point.HasData = true
		}
		out = append(out, point)

		py, pm, ok := previousMonth(y, m)
		if !ok {
			break
		}
		y, m = py, pm
	}
	// Collected newest-first; the chart reads left to right.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

func buildMemberReportYTD(dataDir, year, month string) poster.YearToDate {
	ytd := poster.YearToDate{}
	target, err := strconv.Atoi(month)
	if err != nil {
		return ytd
	}
	for m := 1; m <= target; m++ {
		s, err := loadMonthlyReportFile(dataDir, year, fmt.Sprintf("%02d", m))
		if err != nil {
			continue
		}
		ytd.MonthsElapsed++
		for _, cat := range s.Categories {
			if flow, ok := eurFlow(cat); ok {
				ytd.Income += flow.In
				ytd.Expenses += flow.Out
			}
		}
	}
	ytd.Income = roundReportAmount(ytd.Income)
	ytd.Expenses = roundReportAmount(ytd.Expenses)
	ytd.Net = roundReportAmount(ytd.Income - ytd.Expenses)
	if ytd.MonthsElapsed > 0 {
		ytd.ProjectedNet = roundReportAmount(ytd.Net / float64(ytd.MonthsElapsed) * 12)
	}
	// The year elapsed by the END of the reported month — the honest yardstick
	// for a year-to-date figure against an annual budget.
	ytd.ExpectedShare = float64(target) / 12

	if b, ok := LoadAnnualBudget(year); ok {
		ytd.HasBudget = true
		ytd.BudgetIncome = b.Income
		ytd.BudgetExpenses = b.Expenses
		ytd.BudgetNet = roundReportAmount(b.Income - b.Expenses)
	}
	return ytd
}

// buildMemberReportNotables surfaces the handful of movements a member would
// actually ask about: the largest single amounts in and out, and anything the
// books could not classify.
func buildMemberReportNotables(dataDir, year, month string) ([]string, poster.Highlights) {
	var h poster.Highlights
	txs := loadMonthTransactions(dataDir, year, month)
	if len(txs) == 0 {
		return nil, h
	}

	// Carry the resolved amount with the winner. Recomputing it from a
	// different subset of fields on each side of the comparison silently
	// picked the wrong transaction.
	type candidate struct {
		tx     *TransactionEntry
		amount float64
	}
	var biggestIn, biggestOut candidate
	var untaggedIn float64
	var untaggedCount int
	for i := range txs {
		tx := &txs[i]
		if !isEURCurrency(tx.Currency) {
			continue
		}
		amount := math.Abs(firstNonZeroFloat(tx.GrossAmount, tx.Amount, tx.NormalizedAmount, tx.NetAmount))
		if tx.IsIncoming() {
			if biggestIn.tx == nil || amount > biggestIn.amount {
				biggestIn = candidate{tx, amount}
			}
			if strings.TrimSpace(tx.Category) == "" && !strings.EqualFold(tx.Type, "INTERNAL") {
				untaggedIn += amount
				untaggedCount++
			}
		}
		if tx.IsOutgoing() {
			if biggestOut.tx == nil || amount > biggestOut.amount {
				biggestOut = candidate{tx, amount}
			}
		}
	}

	var out []string
	if biggestIn.tx != nil {
		h.BiggestIn = biggestIn.amount
		h.BiggestInWhat = notableDescription(biggestIn.tx)
		out = append(out, fmt.Sprintf("Largest payment received: %s - %s",
			fmtEUR(biggestIn.amount), h.BiggestInWhat))
	}
	if biggestOut.tx != nil {
		h.BiggestOut = biggestOut.amount
		h.BiggestOutWhat = notableDescription(biggestOut.tx)
		out = append(out, fmt.Sprintf("Largest single expense: %s - %s",
			fmtEUR(biggestOut.amount), h.BiggestOutWhat))
	}
	if untaggedCount > 0 {
		h.UncategorisedIn = untaggedIn
		h.UncategorisedN = untaggedCount
		out = append(out, fmt.Sprintf("%s across %d payments is not yet categorised and is excluded from the category split above",
			fmtEUR(untaggedIn), untaggedCount))
	}
	return out, h
}

func loadMonthTransactions(dataDir, year, month string) []TransactionEntry {
	data, err := os.ReadFile(filepath.Join(dataDir, year, month, "generated", "transactions.json"))
	if err != nil {
		return nil
	}
	var f TransactionsFile
	if json.Unmarshal(data, &f) != nil {
		return nil
	}
	return f.Transactions
}

// notableDescription picks the most human label available, and never leaks a
// raw counterparty name — this page is shared with the whole membership.
func notableDescription(tx *TransactionEntry) string {
	for _, key := range []string{"odooProducts", "description", "memo"} {
		if v := stringMetadata(tx.Metadata, key); v != "" {
			if len(v) > 60 {
				v = v[:57] + "…"
			}
			return v
		}
	}
	if cat := strings.TrimSpace(tx.Category); cat != "" {
		return memberReportCategoryLabel(cat)
	}
	return "no description recorded"
}

func monthLabel(year, month string) string {
	m, err := strconv.Atoi(month)
	if err != nil || m < 1 || m > 12 {
		return year + "-" + month
	}
	return fmt.Sprintf("%s %s", time.Month(m).String(), year)
}

func shortMonthLabel(year, month string) string {
	m, err := strconv.Atoi(month)
	if err != nil || m < 1 || m > 12 {
		return month
	}
	return time.Month(m).String()[:3]
}

func previousMonth(year, month string) (string, string, bool) {
	y, err1 := strconv.Atoi(year)
	m, err2 := strconv.Atoi(month)
	if err1 != nil || err2 != nil {
		return "", "", false
	}
	m--
	if m < 1 {
		m = 12
		y--
	}
	return strconv.Itoa(y), fmt.Sprintf("%02d", m), true
}

// monthlyEURFlow totals a month's categorised euro movement. Returns false when
// the month has no generated report yet.
func monthlyEURFlow(dataDir, year, month string) (in, out float64, ok bool) {
	s, err := loadMonthlyReportFile(dataDir, year, month)
	if err != nil {
		return 0, 0, false
	}
	for _, cat := range s.Categories {
		if flow, found := eurFlow(cat); found {
			in += flow.In
			out += flow.Out
		}
	}
	return roundReportAmount(in), roundReportAmount(out), true
}

// findMemberReportOutlier flags a month whose outflow is at least three times
// the median of the year's other active months. The threshold is deliberately
// blunt: the point is to caption a figure that would otherwise mislead, not to
// classify what happened. It says nothing about the cause.
func findMemberReportOutlier(dataDir, year, month string) *poster.Outlier {
	target, err := strconv.Atoi(month)
	if err != nil {
		return nil
	}
	type monthFlow struct {
		month   string
		in, out float64
	}
	var flows []monthFlow
	for m := 1; m <= target; m++ {
		mm := fmt.Sprintf("%02d", m)
		in, out, ok := monthlyEURFlow(dataDir, year, mm)
		if !ok || (in == 0 && out == 0) {
			continue
		}
		flows = append(flows, monthFlow{mm, in, out})
	}
	if len(flows) < 3 {
		return nil
	}

	worst := 0
	for i := range flows {
		if flows[i].out > flows[worst].out {
			worst = i
		}
	}
	var others []float64
	for i := range flows {
		if i != worst {
			others = append(others, flows[i].out)
		}
	}
	sort.Float64s(others)
	median := others[len(others)/2]
	if median <= 0 || flows[worst].out < median*3 {
		return nil
	}

	var netExcl float64
	for i := range flows {
		if i != worst {
			netExcl += flows[i].in - flows[i].out
		}
	}
	return &poster.Outlier{
		Label:       monthLabel(year, flows[worst].month),
		Outflow:     flows[worst].out,
		TypicalFlow: roundReportAmount(median),
		NetExcl:     roundReportAmount(netExcl),
	}
}

// memberReportCollectiveLabel prefers the display name from collectives.json,
// falling back to a readable form of the slug.
func memberReportCollectiveLabel(slug string) string {
	if c, ok := LoadCollectives()[slug]; ok && strings.TrimSpace(c.Name) != "" {
		return c.Name
	}
	return memberReportCategoryLabel(slug)
}

// buildVATPosition splits VAT into what was charged and what was paid.
//
// Collected comes from the transaction stream, where the generate step records
// vatAmount on incoming payments. Paid comes from the archived supplier bills —
// the transaction side cannot supply it, because a payment out carries no VAT
// breakdown of its own.
func buildVATPosition(dataDir, year, month string, summary *MonthlyReportFile) poster.VATPosition {
	var v poster.VATPosition
	for _, cat := range summary.Categories {
		if flow, ok := eurFlow(cat); ok {
			v.Collected += flow.VAT
		}
	}
	v.Collected = roundReportAmount(v.Collected)

	if bills, ok := loadOdooBills(dataDir, year, month); ok {
		v.HasPaid = true
		for _, b := range bills {
			v.Paid += b.VATAmount
		}
		v.Paid = roundReportAmount(v.Paid)
	}
	v.Net = roundReportAmount(v.Collected - v.Paid)
	return v
}

func loadOdooBills(dataDir, year, month string) ([]OdooOutgoingInvoicePublic, bool) {
	data, err := os.ReadFile(odoosource.Path(dataDir, year, month, odoosource.BillsFile))
	if err != nil {
		return nil, false
	}
	var f struct {
		Bills []OdooOutgoingInvoicePublic `json:"bills"`
	}
	if json.Unmarshal(data, &f) != nil {
		return nil, false
	}
	return f.Bills, true
}

const (
	// fixedCostMinMonths is how often a category must appear as an expense
	// before it counts as a standing commitment rather than a one-off.
	fixedCostMinMonths = 3
	// fixedCostMaxSpread bounds how much a commitment may vary and still be
	// treated as predictable. Recurrence alone is not enough: in 2026 rent
	// varies 1.2x (5,779-6,681) and utilities 1.0x, but equipment swings 249x.
	// Imputing a "usual" bill from a category that lurches would overstate the
	// cost of standing still, which is the opposite of what this is for.
	fixedCostMaxSpread = 2.5
	// fixedCostStaleAfter is how many months a commitment may be absent before
	// it stops counting as current. Consulting ran Jan, Feb and April 2026 at
	// ~1,815 EUR and then stopped; without this it would still be charged
	// against August as a bill "not billed yet", inventing a cost the hub no
	// longer carries. Rent last appears two months back and stays.
	fixedCostStaleAfter = 3
)

// notRunningCosts are categories that belong to a line of business rather than
// to keeping the doors open. Catering is bought for events and billed on to
// them, so charging a "usual" catering bill against a quiet month would invent
// a cost the hub does not carry. The variance filter happens to exclude it in
// 2026, but that is luck — a steady year of events would let it back in.
var notRunningCosts = map[string]bool{
	"catering": true,
}

// buildFixedCosts identifies the recurring commitments from the year's own
// history rather than a hardcoded list: an expense category billed in at least
// fixedCostMinMonths months of the year whose amount stays within
// fixedCostMaxSpread. "Typical" is the median of the months it appeared, so one
// unusual invoice does not set the baseline.
//
// A commitment with nothing recorded this month is reported as missing, not as
// zero. Rent is the case that matters: it was billed in four months of 2026 at
// about 6,176 EUR and does not appear in August at all, which makes August's
// result look roughly 6,000 EUR better than the hub's actual position.
func buildFixedCosts(dataDir, year, month string) ([]poster.FixedCost, float64, float64) {
	target, err := strconv.Atoi(month)
	if err != nil {
		return nil, 0, 0
	}

	perCategory := map[string][]float64{}
	thisMonth := map[string]float64{}
	lastSeen := map[string]int{}
	for m := 1; m <= target; m++ {
		mm := fmt.Sprintf("%02d", m)
		s, err := loadMonthlyReportFile(dataDir, year, mm)
		if err != nil {
			continue
		}
		for _, cat := range s.Categories {
			if cat.Slug == "(untagged)" || notRunningCosts[cat.Slug] {
				continue
			}
			flow, ok := eurFlow(cat)
			if !ok || flow.Out <= 0 {
				continue
			}
			// A category that also earns is a trading line, not a standing
			// cost — coworking and rental both take money in.
			if flow.In > 0 {
				continue
			}
			perCategory[cat.Slug] = append(perCategory[cat.Slug], flow.Out)
			lastSeen[cat.Slug] = m
			if m == target {
				thisMonth[cat.Slug] = flow.Out
			}
		}
	}

	var out []poster.FixedCost
	var typicalTotal, monthTotal float64
	for slug, amounts := range perCategory {
		if len(amounts) < fixedCostMinMonths {
			continue
		}
		sort.Float64s(amounts)
		if lo := amounts[0]; lo <= 0 || amounts[len(amounts)-1]/lo > fixedCostMaxSpread {
			continue // varies too much to call predictable
		}
		if target-lastSeen[slug] >= fixedCostStaleAfter {
			continue // a commitment that has lapsed is not a commitment
		}
		median := amounts[len(amounts)/2]
		fc := poster.FixedCost{
			Slug:      slug,
			Label:     memberReportCategoryLabel(slug),
			Typical:   roundReportAmount(median),
			ThisMonth: roundReportAmount(thisMonth[slug]),
		}
		fc.Missing = fc.ThisMonth == 0
		out = append(out, fc)
		typicalTotal += fc.Typical
		monthTotal += fc.ThisMonth
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Typical != out[j].Typical {
			return out[i].Typical > out[j].Typical
		}
		return out[i].Label < out[j].Label
	})
	return out, roundReportAmount(typicalTotal), roundReportAmount(monthTotal)
}

// memberReportAccountLabel names an account for a reader, not for a ledger.
//
// The stored accountName describes the rail rather than the account — both the
// savings and the checking wallet come through as "Gnosis EURe", so using it
// alone puts two identically-labelled figures side by side. The slug is what
// distinguishes them, so it leads, with the rail kept as a quiet qualifier.
func memberReportAccountLabel(slug, name string) string {
	readable := memberReportCategoryLabel(slug)
	rail := strings.TrimSpace(name)
	if fields := strings.Fields(rail); len(fields) > 1 && !isASCIIWord(fields[0]) {
		rail = strings.Join(fields[1:], " ") // drop the leading emoji
	}
	if rail == "" {
		return readable
	}
	// Same word, different casing: the stored name has the right one. The
	// slug-derived form title-cases blindly and turns "KBC" into "Kbc".
	if strings.EqualFold(rail, readable) {
		return rail
	}
	return readable + " (" + rail + ")"
}

func isASCIIWord(s string) bool {
	for _, r := range s {
		if r > 127 {
			return false
		}
	}
	return s != ""
}

// segmentFor routes a line name to its segment. Anything unrecognised lands in
// Shared, which is the point: an unmapped category should be visible as
// unassigned, not folded into a business line it may not belong to.
func segmentFor(r *poster.Report, line string) *poster.Segment {
	switch line {
	case LineHub:
		return &r.HubLine
	case LineEvents:
		return &r.EventLine
	default:
		return &r.SharedLine
	}
}

// buildEURTokenStats aggregates the on-chain euro wallets — Monerium EURe and
// the Brussels Pay EURb the fridge runs on — into one picture.
//
// Unlike CHT the hub does not issue this token, so the figures are holdings and
// movement: what came in, what went out, what is left, and how many wallets
// were involved. "Held" is only reported when at least one wallet had a live
// balance to anchor it; an unanchored running total would be a number with the
// right shape and an arbitrary level.
func buildEURTokenStats(summary *MonthlyReportFile) *poster.EURToken {
	t := &poster.EURToken{}
	seen := map[string]bool{}
	var symbols []string
	for _, acct := range summary.Accounts {
		// Every euro account, whatever rail it runs on. A member asking "how
		// much money came in" means all of it, not the on-chain share.
		if !isEURCurrency(acct.Currency) {
			continue
		}
		// The euro arrives on several rails — plain EUR in the bank and through
		// Stripe, Monerium EURe on Gnosis, the Brussels Pay EURb behind the
		// fridge — so name every form the figures actually cover.
		if sym := strings.TrimSpace(acct.Currency); sym != "" && !seen[strings.ToUpper(sym)] {
			seen[strings.ToUpper(sym)] = true
			symbols = append(symbols, sym)
		}
		t.Wallets++
		t.Received += acct.Amounts.In
		t.Spent += acct.Amounts.Out
		if acct.Amounts.In > 0 || acct.Amounts.Out > 0 {
			t.Active++
		}
		if acct.Balance.Anchored && acct.Balance.Ending != nil {
			t.Held += *acct.Balance.Ending
			t.HeldKnown = true
		}
	}
	if t.Wallets == 0 {
		return nil
	}
	sort.Strings(symbols)
	for i, sym := range symbols {
		symbols[i] = prettyEURSymbol(sym)
	}
	t.Symbol = strings.Join(symbols, ", ")
	t.Received = roundReportAmount(t.Received)
	t.Spent = roundReportAmount(t.Spent)
	t.Held = roundReportAmount(t.Held)
	return t
}

// prettyEURSymbol restores the casing these tokens are written with. The
// summary uppercases currency codes for grouping, which turns EURe into EURE
// and EURb into EURB — neither is how anyone writes them.
func prettyEURSymbol(sym string) string {
	switch strings.ToUpper(strings.TrimSpace(sym)) {
	case "EURE":
		return "EURe"
	case "EURB":
		return "EURb"
	default:
		return strings.ToUpper(strings.TrimSpace(sym))
	}
}
