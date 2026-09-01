package cmd

import (
	"encoding/json"
	"os"
	"strings"
)

// The hub runs two things that behave quite differently: the space itself —
// memberships, coworking, the premises and their bills — and an event business
// that hires rooms out and caters them. Blending both into one set of totals
// hides which is carrying which, so the member report reports them separately.
const (
	LineHub    = "hub"
	LineEvents = "events"
	LineShared = "shared" // overheads and money that belongs to neither alone
)

// BusinessLines maps a category or account slug to the line it belongs to.
// Loaded from settings/business-lines.json when present, so the split can be
// retuned without a rebuild; the defaults below are the starting point.
type BusinessLines struct {
	Categories map[string]string `json:"categories,omitempty"`
	Accounts   map[string]string `json:"accounts,omitempty"`
}

// defaultBusinessLines is the split as the books read today.
//
// The event side is deliberately narrow: hiring a room out and catering it.
// Everything that exists whether or not a single event happens — the lease, the
// energy bill, the coworking desks, the community fridge — is the hub. Anything
// unlisted falls to shared rather than being guessed into a line, so a new
// category shows up as unassigned instead of quietly distorting a total.
var defaultBusinessLines = BusinessLines{
	Categories: map[string]string{
		// The space and its community.
		"membership": LineHub,
		"coworking":  LineHub,
		"donation":   LineHub,
		"subsidy":    LineHub,
		"grant":      LineHub,
		"sponsoring": LineHub,
		"fridge":     LineHub,
		"drinks":     LineHub,
		"rent":       LineHub,
		"utilities":  LineHub,
		"internet":   LineHub,
		"insurance":  LineHub,
		"furniture":  LineHub,
		"equipment":  LineHub,
		"supplies":   LineHub,
		"salaries":   LineHub,
		"debt":       LineHub,

		// Hiring the rooms out, feeding the people in them, and the outside
		// help bought to run them.
		"rental":        LineEvents,
		"consulting":    LineEvents,
		"rentals":       LineEvents,
		"ticket":        LineEvents,
		"event_tickets": LineEvents,
		"events":        LineEvents,
		"catering":      LineEvents,

		// Overheads that serve both, and payment plumbing.
		"accounting": LineShared,
		"marketing":  LineShared,
		"webservice": LineShared,
		"services":   LineShared,
		"taxes":      LineShared,
		"stripe_fee": LineShared,
		"commission": LineShared,
	},
	Accounts: map[string]string{
		// Wallets dedicated to one activity. The general bank and Stripe
		// accounts serve both and stay shared.
		"fridge": LineHub,
		"coffee": LineHub,
	},
}

func businessLinesPath() string { return settingsFilePath("business-lines.json") }

// LoadBusinessLines merges settings/business-lines.json over the defaults, so a
// local file only has to name what it wants to change.
func LoadBusinessLines() BusinessLines {
	merged := BusinessLines{
		Categories: map[string]string{},
		Accounts:   map[string]string{},
	}
	for k, v := range defaultBusinessLines.Categories {
		merged.Categories[k] = v
	}
	for k, v := range defaultBusinessLines.Accounts {
		merged.Accounts[k] = v
	}

	data, err := os.ReadFile(businessLinesPath())
	if err != nil {
		return merged
	}
	var local BusinessLines
	if json.Unmarshal(data, &local) != nil {
		return merged
	}
	for k, v := range local.Categories {
		merged.Categories[strings.ToLower(k)] = normalizeLine(v)
	}
	for k, v := range local.Accounts {
		merged.Accounts[strings.ToLower(k)] = normalizeLine(v)
	}
	return merged
}

// LineFor returns the line a category belongs to. Unknown categories are shared
// rather than assumed: an unrecognised slug is a gap in the mapping, and
// silently filing it under one line would misstate that line's result.
func (b BusinessLines) LineFor(categorySlug string) string {
	if line, ok := b.Categories[strings.ToLower(strings.TrimSpace(categorySlug))]; ok {
		return line
	}
	return LineShared
}

// AccountLineFor returns the line an account is dedicated to, or LineShared
// when it serves both.
func (b BusinessLines) AccountLineFor(accountSlug string) string {
	if line, ok := b.Accounts[strings.ToLower(strings.TrimSpace(accountSlug))]; ok {
		return line
	}
	return LineShared
}

func normalizeLine(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case LineHub:
		return LineHub
	case LineEvents:
		return LineEvents
	default:
		return LineShared
	}
}
