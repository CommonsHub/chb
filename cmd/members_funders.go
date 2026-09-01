package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// fundersFileName is the operator-maintained list of memberships paid outside
// Stripe and Odoo — a bank transfer, a grant, a membership someone gifted.
// Nothing writes it: entries are added by hand, which is the point. It lives in
// settings/ so it syncs between trusted hosts like the rest of the
// configuration.
const fundersFileName = "funders.json"

// Funder is one membership paid up to a date rather than renewed by a
// subscription. There is no provider to ask, so the file states the term
// directly and chb works out which months it covers.
type Funder struct {
	// EmailHash is the membership id — the same salted digest Stripe and Odoo
	// members are keyed by. `chb members whois <email>` prints it. Preferred
	// over Email: it keeps addresses out of a file that syncs between hosts.
	EmailHash string `json:"emailHash,omitempty"`
	// Email is hashed at sync time when EmailHash is absent. Convenient, but it
	// puts the address in the file — use EmailHash where you can.
	Email     string `json:"email,omitempty"`
	FirstName string `json:"firstName,omitempty"`
	// StartsAt is the first day covered (YYYY-MM-DD). Empty means "covered from
	// the beginning of the history window".
	StartsAt string `json:"startsAt,omitempty"`
	// ExpiresAt is the last day covered (YYYY-MM-DD). Required: a funder
	// without an end date is an open-ended claim nobody reviews.
	ExpiresAt string `json:"expiresAt"`
	// Amount and Interval describe what was paid, for the member's own view.
	Amount   MemberAmount `json:"amount,omitempty"`
	Interval string       `json:"interval,omitempty"` // "month" or "year"
	// Note records why this entry exists — an invoice number, who arranged it.
	// Never shown to the member; it is for whoever reads the file next.
	Note           string `json:"note,omitempty"`
	IsOrganization bool   `json:"isOrganization,omitempty"`
}

type fundersFile struct {
	Description string   `json:"description,omitempty"`
	Funders     []Funder `json:"funders"`
}

// loadFunders reads settings/funders.json. A missing file is not an error —
// most deployments have no funders.
func loadFunders() ([]Funder, error) {
	data, err := os.ReadFile(settingsFilePath(fundersFileName))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var f fundersFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("%s: %w", fundersFileName, err)
	}
	return f.Funders, nil
}

// funderMemberID resolves a funder's membership id, hashing Email when
// EmailHash is absent. Returns "" when the entry identifies nobody.
func funderMemberID(f Funder, salt string) string {
	if id := strings.ToLower(strings.TrimSpace(f.EmailHash)); id != "" {
		if emailHashPattern.MatchString(id) {
			return id
		}
		return ""
	}
	if email := strings.TrimSpace(f.Email); email != "" && salt != "" {
		return hashEmail(email, salt)
	}
	return ""
}

// funderCoversMonth reports whether a funder's term covers the given month.
//
// The membership is paid *until* ExpiresAt, so a month is covered when the
// term is still running on its first day — a funder paid to the 15th is a
// member for that whole month, not two thirds of one. StartsAt, when set,
// excludes months that ended before the term began.
func funderCoversMonth(f Funder, year int, month time.Month) (bool, error) {
	tz := BrusselsTZ()
	monthStart := time.Date(year, month, 1, 0, 0, 0, 0, tz)
	monthEnd := monthStart.AddDate(0, 1, 0).Add(-time.Second)

	expiry, err := parseFunderDate(f.ExpiresAt)
	if err != nil {
		return false, fmt.Errorf("expiresAt: %w", err)
	}
	// The expiry date is itself covered, so compare against its end of day.
	if endOfDay(expiry).Before(monthStart) {
		return false, nil
	}
	if strings.TrimSpace(f.StartsAt) != "" {
		start, err := parseFunderDate(f.StartsAt)
		if err != nil {
			return false, fmt.Errorf("startsAt: %w", err)
		}
		if start.After(monthEnd) {
			return false, nil
		}
	}
	return true, nil
}

func parseFunderDate(value string) (time.Time, error) {
	v := strings.TrimSpace(value)
	if v == "" {
		return time.Time{}, fmt.Errorf("missing date")
	}
	t, err := time.ParseInLocation("2006-01-02", v, BrusselsTZ())
	if err != nil {
		return time.Time{}, fmt.Errorf("%q is not a YYYY-MM-DD date", value)
	}
	return t, nil
}

// buildFundersSnapshot returns the funders covering the given month, as a
// provider snapshot alongside Stripe's and Odoo's.
//
// Unlike those two this needs no network and no reconstruction: the file
// states the term, so a past month is as reliable as the current one. A
// malformed entry is reported and skipped rather than failing the sync — one
// bad date should not cost the operator the whole month's membership.
func buildFundersSnapshot(funders []Funder, year int, month time.Month, salt string) providerSnapshot {
	snap := providerSnapshot{
		Provider:  "funders",
		FetchedAt: time.Now().UTC().Format(time.RFC3339),
	}
	for i, f := range funders {
		id := funderMemberID(f, salt)
		if id == "" {
			Warnf("⚠ %s entry %d identifies nobody (needs a valid emailHash, or an email plus EMAIL_HASH_SALT) — skipped",
				fundersFileName, i+1)
			continue
		}
		covers, err := funderCoversMonth(f, year, month)
		if err != nil {
			Warnf("⚠ %s entry %d (%s…): %v — skipped", fundersFileName, i+1, id[:8], err)
			continue
		}
		if !covers {
			continue
		}

		interval := strings.ToLower(strings.TrimSpace(f.Interval))
		plan := "monthly"
		if interval == "year" {
			plan = "yearly"
		}
		if interval == "" {
			interval = "month"
		}

		snap.Subscriptions = append(snap.Subscriptions, providerSubscription{
			ID:                 "funder:" + id[:12],
			Source:             "funders",
			EmailHash:          id,
			FirstName:          f.FirstName,
			Plan:               plan,
			Amount:             f.Amount,
			Interval:           interval,
			Status:             "active",
			CurrentPeriodStart: firstNonEmpty(f.StartsAt, f.ExpiresAt),
			CurrentPeriodEnd:   f.ExpiresAt,
			CreatedAt:          firstNonEmpty(f.StartsAt, f.ExpiresAt),
			IsOrganization:     f.IsOrganization,
		})
	}
	return snap
}
