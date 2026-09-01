package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func writeGenerated(t *testing.T, dataDir, year, month, name string, v interface{}) {
	t.Helper()
	dir := filepath.Join(dataDir, year, month, "generated")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func eurCategory(slug string, in, out float64) TaggedSummary {
	return TaggedSummary{Slug: slug, Currencies: []CurrencyFlow{
		{Currency: "EUR", In: in, Out: out, Net: in - out},
	}}
}

func TestBuildMemberReportHeadlines(t *testing.T) {
	dataDir := t.TempDir()
	writeGenerated(t, dataDir, "2026", "07", "summary.json", MonthlyReportFile{
		Year: "2026", Month: "07",
		Categories: []TaggedSummary{eurCategory("rental", 1000, 200)},
	})
	writeGenerated(t, dataDir, "2026", "07", "members.json", MembersOutputFile{
		Summary: MembersSummary{ActiveMembers: 50, MRR: MemberAmount{Value: 500}},
	})
	writeGenerated(t, dataDir, "2026", "08", "summary.json", MonthlyReportFile{
		Year: "2026", Month: "08",
		Summary: MonthlyReportSummary{Contributors: 52, Events: 2, Bookings: 19},
		Categories: []TaggedSummary{
			eurCategory("rental", 4524.12, 785.18),
			eurCategory("coworking", 1875.50, 325.85),
			// A non-EUR row must never be folded into euro totals.
			{Slug: "cht-thing", Currencies: []CurrencyFlow{{Currency: "CHT", In: 141, Out: 61}}},
		},
	})
	writeGenerated(t, dataDir, "2026", "08", "members.json", MembersOutputFile{
		Summary:     MembersSummary{ActiveMembers: 53, MRR: MemberAmount{Value: 541.66}},
		OdooDerived: true,
	})

	r, err := BuildMemberReport(dataDir, "2026", "08")
	if err != nil {
		t.Fatal(err)
	}

	if r.MonthLabel != "August 2026" {
		t.Errorf("MonthLabel = %q", r.MonthLabel)
	}
	if r.Income != 6399.62 || r.Expenses != 1111.03 {
		t.Errorf("income/expenses = %.2f / %.2f, want 6399.62 / 1111.03 (CHT excluded)", r.Income, r.Expenses)
	}
	if r.Net != 5288.59 {
		t.Errorf("Net = %.2f, want 5288.59", r.Net)
	}
	if r.ActiveMembers != 53 || r.MemberDelta != 3 {
		t.Errorf("members = %d (delta %d), want 53 (+3)", r.ActiveMembers, r.MemberDelta)
	}
	if r.MRRDelta != 41.66 {
		t.Errorf("MRRDelta = %.2f, want 41.66", r.MRRDelta)
	}
	if !r.OdooDerived {
		t.Error("want the derived-membership caveat carried through to the report")
	}
	if len(r.IncomeLines) == 0 || r.IncomeLines[0].Label != "Rental" {
		t.Errorf("income lines should lead with the largest: %+v", r.IncomeLines)
	}
	if got := r.IncomeLines[0].Share; got < 0.70 || got > 0.71 {
		t.Errorf("largest income share = %.4f, want ~0.7069", got)
	}
	if r.Contributors != 52 || r.Events != 2 || r.Bookings != 19 {
		t.Errorf("summary carry-through wrong: %+v", r)
	}
}

// TestBuildMemberReportTrendSkipsMonthsWithoutMembers guards the "0 members"
// artifact: a month with financial data but no membership snapshot must not be
// plotted as if it had zero members.
func TestBuildMemberReportTrendSkipsMonthsWithoutMembers(t *testing.T) {
	dataDir := t.TempDir()
	// March: money, no membership snapshot.
	writeGenerated(t, dataDir, "2026", "03", "summary.json", MonthlyReportFile{
		Categories: []TaggedSummary{eurCategory("rental", 500, 100)},
	})
	// April: both.
	writeGenerated(t, dataDir, "2026", "04", "summary.json", MonthlyReportFile{
		Categories: []TaggedSummary{eurCategory("rental", 800, 100)},
	})
	writeGenerated(t, dataDir, "2026", "04", "members.json", MembersOutputFile{
		Summary: MembersSummary{ActiveMembers: 59},
	})

	trend := buildMemberReportTrend(dataDir, "2026", "04", 3)
	for _, p := range trend {
		if p.Label == "Mar" && p.HasMembers {
			t.Errorf("March has no members.json but was marked HasMembers")
		}
		if p.Label == "Apr" && !p.HasMembers {
			t.Errorf("April has members.json but was not marked HasMembers")
		}
	}
}

// TestFindMemberReportOutlier covers the caption that stops one extraordinary
// month from being read as the shape of the year.
func TestFindMemberReportOutlier(t *testing.T) {
	dataDir := t.TempDir()
	for _, m := range []struct {
		month   string
		in, out float64
	}{
		{"01", 15554, 22342},
		{"02", 48279, 30711},
		{"03", 20809, 36952},
		{"04", 20074, 30434},
		{"05", 11296, 113630}, // the month that dwarfs the rest
		{"06", 7588, 11820},
	} {
		writeGenerated(t, dataDir, "2026", m.month, "summary.json", MonthlyReportFile{
			Categories: []TaggedSummary{eurCategory("x", m.in, m.out)},
		})
	}

	o := findMemberReportOutlier(dataDir, "2026", "06")
	if o == nil {
		t.Fatal("want May flagged as an outlier")
	}
	if o.Label != "May 2026" {
		t.Errorf("Label = %q, want May 2026", o.Label)
	}
	if o.Outflow != 113630 {
		t.Errorf("Outflow = %.2f, want 113630", o.Outflow)
	}
	// Excluding May: (15554+48279+20809+20074+7588) - (22342+30711+36952+30434+11820)
	if want := 112304.0 - 132259.0; o.NetExcl != want {
		t.Errorf("NetExcl = %.2f, want %.2f", o.NetExcl, want)
	}

	t.Run("a level year has no outlier", func(t *testing.T) {
		flat := t.TempDir()
		for _, m := range []string{"01", "02", "03", "04"} {
			writeGenerated(t, flat, "2026", m, "summary.json", MonthlyReportFile{
				Categories: []TaggedSummary{eurCategory("x", 10000, 9000)},
			})
		}
		if o := findMemberReportOutlier(flat, "2026", "04"); o != nil {
			t.Errorf("flagged an outlier in a level year: %+v", o)
		}
	})
}

func TestBuildMemberReportNeedsGeneratedData(t *testing.T) {
	if _, err := BuildMemberReport(t.TempDir(), "2026", "08"); err == nil {
		t.Error("want a clear error pointing at `chb generate`, got nil")
	}
}
