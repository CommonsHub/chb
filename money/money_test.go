package money

import "testing"

// TestNumberGroupsNegatives covers the bug the shared package fixes: the
// previous implementation grouped digits without stripping the sign first, so
// the minus counted towards a digit position and -123456.78 came out as
// "-,123,456.78". It never showed up because every caller happened to pass an
// absolute value.
func TestNumberGroupsNegatives(t *testing.T) {
	for _, tc := range []struct {
		in   float64
		want string
	}{
		{-123456.78, "-123,456.78"},
		{-1234.5, "-1,234.50"},
		{-999, "-999.00"},
		{123456.78, "123,456.78"},
		{1234567.891, "1,234,567.89"},
		{999, "999.00"},
		{0, "0.00"},
		{-0.004, "-0.00"},
	} {
		if got := Number(tc.in); got != tc.want {
			t.Errorf("Number(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestEURDropsTheSignAndEURSignedKeepsIt(t *testing.T) {
	if got := EUR(-5949.8); got != "5,949.80 EUR" {
		t.Errorf("EUR(-5949.8) = %q, want the magnitude only", got)
	}
	if got := EURSigned(-5949.8); got != "-5,949.80 EUR" {
		t.Errorf("EURSigned(-5949.8) = %q", got)
	}
	if got := EURSigned(10); got != "+10.00 EUR" {
		t.Errorf("EURSigned(10) = %q", got)
	}
}

// TestIsEUR pins the domain rule, not a formatting choice: these codes are one
// currency when the hub totals what it holds.
func TestIsEUR(t *testing.T) {
	for _, c := range []string{"EUR", "EURe", "EURb", "eure", ""} {
		if !IsEUR(c) {
			t.Errorf("IsEUR(%q) = false, want true", c)
		}
	}
	for _, c := range []string{"CHT", "USD", "USDC"} {
		if IsEUR(c) {
			t.Errorf("IsEUR(%q) = true, want false", c)
		}
	}
}

func TestToken(t *testing.T) {
	if got := Token(-1234.56, "CHT"); got != "1,234.56 CHT" {
		t.Errorf("Token() = %q", got)
	}
}
