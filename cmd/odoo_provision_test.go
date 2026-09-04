package cmd

import "testing"

// TestApproveAnalyticAccountCreationReadOnly pins the split between
// `chb odoo pull` and `chb odoo provision`: a read-only caller must never be
// talked into creating, not even by --yes. Creation used to ride along on
// `chb odoo pull --yes`, which meant a routine fetch was one flag away from
// mutating the Odoo instance.
func TestApproveAnalyticAccountCreationReadOnly(t *testing.T) {
	missing := []analyticAccountSpec{
		{Slug: "coworking", Name: "Coworking", PlanID: 8, Kind: "category"},
		{Slug: "zinne", Name: "Zinne", PlanID: 3, Kind: "collective"},
	}
	plans := OdooAnalyticPlanIDs{Collective: 3, Costs: 8, Income: 13}

	for _, tc := range []struct {
		name                           string
		assumeYes, dryRun, allowCreate bool
		want                           bool
	}{
		{"read-only ignores --yes", true, false, false, false},
		{"read-only plain", false, false, false, false},
		{"provision --dry-run", false, true, true, false},
		{"provision --yes", true, false, true, true},
		{"provision --yes wins over --dry-run? no", true, true, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := approveAnalyticAccountCreation(missing, nil, plans, tc.assumeYes, tc.dryRun, tc.allowCreate)
			if got != tc.want {
				t.Errorf("approveAnalyticAccountCreation(assumeYes=%v, dryRun=%v, allowCreate=%v) = %v, want %v",
					tc.assumeYes, tc.dryRun, tc.allowCreate, got, tc.want)
			}
		})
	}
}
