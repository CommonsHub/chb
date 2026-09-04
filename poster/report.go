// Package poster renders the Commons Hub's one-page monthly member report.
//
// It is deliberately separate from cmd: the figures are assembled there, where
// the provider archives and settings live, and arrive here as a plain Report.
// That split keeps the layout testable without a data directory, and keeps the
// renderer free of everything chb knows about Odoo, Stripe and chains.
//
// The layout is a port of the "Monthly Wall Poster" design, whose canvas is
// 1240 x 1754 px — exactly A4 at 150dpi.
package poster

// Report is everything the one-page member PDF shows, assembled from
// local generated/ files only — no network, per the offline-first rule.
// Rendering is deliberately a separate step so the figures can be asserted
// without parsing a PDF.
type Report struct {
	Year, Month string
	MonthLabel  string // "August 2026"

	// Headline figures for the month.
	ActiveMembers int
	MemberDelta   int // vs the previous month, 0 when there is no previous month
	MRR           float64
	MRRDelta      float64
	Contributors  int
	Events        int
	Bookings      int

	Income   float64
	Expenses float64
	Net      float64

	VAT VATPosition

	// FixedCosts are the commitments that recur every month — rent, energy,
	// connectivity. They matter more to "where do we stand" than any single
	// month's result, and a month that simply hasn't been billed yet looks
	// far healthier than it is.
	FixedCosts         []FixedCost
	FixedCostTypical   float64 // what a normal month costs
	FixedCostThisMonth float64
	// NetAfterFixed is this month's result once every recurring commitment is
	// covered at its usual size, whether or not it was billed in this month.
	NetAfterFixed float64

	// OdooDerived flags a month whose Odoo membership was reconstructed
	// rather than captured live — the figure can undercount.
	OdooDerived bool

	IncomeLines  []Line // biggest income categories, largest first
	ExpenseLines []Line

	// Hub and Events are the two halves of the page. Shared holds the
	// overheads and anything the mapping does not place, so nothing is
	// silently absorbed into a line it does not belong to.
	HubLine    Segment
	EventLine  Segment
	SharedLine Segment

	Trend []TrendPoint // oldest → newest, ending at this month

	YTD YearToDate

	// Outlier names a month whose outflow dwarfs every other month of the
	// year. A single extraordinary month drags the year-to-date and the
	// run-rate with it, and presenting either without saying so would leave
	// members reading a normal year as a catastrophic one.
	Outlier *Outlier

	// Balances is the reconciled position of each account at the month end.
	// Empty when no live balance has been cached to anchor the roll-back.
	Balances []Balance
	// BalanceNote is the rollup's own summary of what reconciled.
	BalanceNote string

	Tokens []Token

	// EURToken is the on-chain euro the hub actually transacts in. It is not
	// issued by the hub, so it is reported as holdings and movement rather
	// than as minting and burning.
	EURToken *EURToken

	// Collectives is the fiscal-hosting picture: the hub holds money for other
	// groups, and a member reading "income" deserves to know how much of it
	// was never the hub's to spend.
	Collectives []Line

	Notables []string

	// Highlights carries the same facts as Notables but unformatted, so the
	// poster can render them in its own money style instead of unpicking
	// strings that were written for a terminal.
	Highlights Highlights
}

// Highlights are the month's few standout movements.
type Highlights struct {
	BiggestIn       float64
	BiggestInWhat   string
	BiggestOut      float64
	BiggestOutWhat  string
	UncategorisedIn float64
	UncategorisedN  int
}

// VATPosition separates the VAT the hub charged its customers from the VAT it
// paid its suppliers. Only the difference is really the hub's to hand over, and
// showing a single blended figure makes the books look worse than they are.
type VATPosition struct {
	Collected float64 // charged on our invoices — owed onward
	Paid      float64 // paid on suppliers' bills — reclaimable
	Net       float64 // Collected - Paid: what is actually due
	HasPaid   bool    // false when no bills were archived for the month
}

// FixedCost is one recurring commitment.
type FixedCost struct {
	Slug      string
	Label     string
	Typical   float64 // median of the months it was actually billed
	ThisMonth float64
	// Missing marks a commitment that normally recurs but has no payment
	// recorded this month — usually a bill not yet entered rather than a cost
	// that went away.
	Missing bool
}

// Segment is one line of business: what it took in, what it cost,
// and the categories that make up each side.
type Segment struct {
	Name     string
	Income   float64
	Expenses float64
	Net      float64
	In       []Line
	Out      []Line
	// Accounts dedicated to this line, if any.
	Accounts []Balance

	// FixedTypical is what this line's standing commitments cost in a normal
	// month; FixedThisMonth is what was actually billed. NetAfterFixed charges
	// every commitment at its usual size, so a month whose rent invoice has
	// not arrived does not read as a month with no rent.
	FixedTypical   float64
	FixedThisMonth float64
	NetAfterFixed  float64
	// Unbilled is the part of this line's standing commitments that has not
	// reached the books this month. It is a liability the line carries, so the
	// segment's headline figures include it rather than reporting a month that
	// merely hasn't been invoiced as a good one.
	Unbilled float64
	// ExpensesInclUnbilled is Expenses plus Unbilled — what the line really
	// cost, as opposed to what has been recorded so far.
	ExpensesInclUnbilled float64
	// FixedCosts are this line's commitments, for the reader to see which.
	FixedCosts []FixedCost
}

type Line struct {
	Label  string
	Amount float64
	Share  float64 // 0..1 of the month's income or expense total
}

type TrendPoint struct {
	Label   string // "Aug"
	Members int
	Net     float64
	// HasData distinguishes a month with no generated report from a month
	// that genuinely had nothing happen. Without it, a missing month plots as
	// a confident zero.
	HasData bool
	// HasMembers is separate: several months have financial data but no
	// membership snapshot at all, and "0 members" is a very different claim
	// from "we don't know".
	HasMembers bool
}

// YearToDate places the month inside the year: what has come in and gone
// out since January, and — when a budget is configured — how that compares.
type YearToDate struct {
	MonthsElapsed int
	Income        float64
	Expenses      float64
	Net           float64
	// ProjectedNet extrapolates the year-to-date run rate over 12 months.
	// It is arithmetic, not a forecast: a single large month moves it a lot.
	ProjectedNet float64
	// Budget figures are present only when settings/budget.json exists.
	HasBudget      bool
	BudgetIncome   float64
	BudgetExpenses float64
	BudgetNet      float64
	// ExpectedShare is the fraction of the year elapsed, the fair yardstick
	// for judging a year-to-date figure against an annual budget.
	ExpectedShare float64
}

// Outlier describes an extraordinary month and what the year looks
// like once it is set aside.
type Outlier struct {
	Label       string // "May 2026"
	Outflow     float64
	TypicalFlow float64 // median monthly outflow of the other months
	NetExcl     float64 // year-to-date net excluding this month
}

// Balance is one account's money at the end of the month.
type Balance struct {
	Label    string
	Currency string
	Ending   float64
	Change   float64 // movement during the month
	Verified bool    // checked against a live balance rather than inferred
}

// EURToken summarises every euro the hub holds — the on-chain
// wallets and the bank and payment accounts alike. Splitting them apart made
// the page's own figures disagree: the wallets alone received 8,809.10 EUR in
// August against 9,971.76 EUR across all accounts.
type EURToken struct {
	Symbol   string
	Received float64
	Spent    float64
	Held     float64
	Wallets  int
	Active   int
	// HeldKnown is false when no wallet had an anchored balance, in which case
	// "held" would be a running total from chb's first record rather than a
	// real figure.
	HeldKnown bool
}

type Token struct {
	Symbol  string
	Minted  float64
	Burnt   float64
	Supply  float64
	Holders int
	Active  int
}
