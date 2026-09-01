// Package money holds how the Commons Hub writes and groups money.
//
// It exists because these are not display details: IsEUR decides which accounts
// are summed into a single balance, and Number is the one definition of how a
// figure is written. Both are used by the terminal report and by the printed
// member poster, on the same underlying numbers. A second copy of either would
// let the two outputs disagree about the same month — which is a bug the reader
// has no way to detect.
//
// Deliberately narrow. It is not a home for general formatting: pluralisation,
// padding and date rendering have no domain content and stay where they are.
package money

import (
	"math"
	"strconv"
	"strings"
)

// IsEUR reports whether a currency code belongs to the euro family.
//
// The hub receives euro on several rails — plain EUR in the bank and through
// Stripe, Monerium EURe on Gnosis, Brussels Pay EURb behind the fridge — and
// treats them as one currency when totalling. An empty code counts as euro:
// older records predate the field.
func IsEUR(currency string) bool {
	return currency == "" || strings.HasPrefix(strings.ToUpper(currency), "EUR")
}

// Number formats a float with thousands separators and two decimals:
// 12,345.67. Negative values keep their sign in front of the digits.
func Number(v float64) string {
	s := strconv.FormatFloat(v, 'f', 2, 64)
	intPart, frac, _ := strings.Cut(s, ".")

	// Strip the sign before grouping. Leaving it in makes it count towards the
	// digit positions, so -123456.78 grouped as "-,123,456.78".
	neg := strings.HasPrefix(intPart, "-")
	intPart = strings.TrimPrefix(intPart, "-")

	var b strings.Builder
	if neg {
		b.WriteByte('-')
	}
	for i := range intPart {
		if i > 0 && (len(intPart)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteByte(intPart[i])
	}
	b.WriteByte('.')
	b.WriteString(frac)
	return b.String()
}

// EUR formats an amount in euro without a sign. Callers that need direction
// say so with a label ("in" / "out") or use EURSigned.
func EUR(v float64) string {
	return Number(math.Abs(v)) + " EUR"
}

// EURSigned formats an amount with an explicit + or -, for deltas and net
// figures where the direction is the point.
func EURSigned(v float64) string {
	if v >= 0 {
		return "+" + EUR(v)
	}
	return "-" + EUR(-v)
}

// Token formats a token amount with its symbol, e.g. "1,234.56 CHT".
func Token(v float64, symbol string) string {
	return Number(math.Abs(v)) + " " + symbol
}
