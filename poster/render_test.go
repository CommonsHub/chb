package poster

import (
	"math"
	"os"
	"path/filepath"
	"testing"
)

// TestRenderMemberReportPDFWritesAPage is a smoke test: the renderer must
// produce a real, single-page PDF without a font or layout panic.
func TestRenderMemberReportPDFWritesAPage(t *testing.T) {
	out := filepath.Join(t.TempDir(), "report.pdf")
	r := &Report{
		Year: "2026", Month: "08", MonthLabel: "August 2026",
		ActiveMembers: 53, MRR: 541.66, Contributors: 52,
		Income: 10040.26, Expenses: 4090.46, Net: 5949.80,
		// Non-ASCII must survive the Latin-1 core fonts.
		IncomeLines:  []Line{{Label: "Réservations · salle", Amount: 4524.12, Share: 0.45}},
		ExpenseLines: []Line{{Label: "Fridge", Amount: 1088.09, Share: 0.27}},
		Trend: []TrendPoint{
			{Label: "Jul", Members: 52, Net: 8271, HasData: true, HasMembers: true},
			{Label: "Aug", Members: 53, Net: 5949, HasData: true, HasMembers: true},
		},
		YTD:      YearToDate{MonthsElapsed: 8, Income: 155118, Expenses: 263185, Net: -108066, ProjectedNet: -162100, ExpectedShare: 8.0 / 12},
		Outlier:  &Outlier{Label: "May 2026", Outflow: 113630, TypicalFlow: 22342, NetExcl: 5732},
		Notables: []string{"Largest payment received: 1,804.01 EUR"},
	}
	if err := Render(r, out, ""); err != nil {
		t.Fatalf("RenderMemberReportPDF: %v", err)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) < 1000 {
		t.Errorf("PDF is only %d bytes, likely empty", len(data))
	}
	if string(data[:5]) != "%PDF-" {
		t.Errorf("output is not a PDF: %q", string(data[:8]))
	}
}

// TestEURFormatsForAPoster pins the poster's money format: a euro sign, no
// cents, and a real minus sign. Losing the sign printed a year-to-date loss of
// 108,143 as a surplus on a page sent to the whole membership; cents at 100px
// are noise.
func TestEURFormatsForAPoster(t *testing.T) {
	for _, tc := range []struct {
		in   float64
		want string
	}{
		{-108142.81, "\u2212€108,143"},
		{5949.80, "€5,950"},
		{0, "€0"},
		{-0.4, "€0"},
	} {
		if got := eur(tc.in); got != tc.want {
			t.Errorf("eur(%.2f) = %q, want %q", tc.in, got, tc.want)
		}
	}
	if got := eurSigned(541.66); got != "+€542" {
		t.Errorf("eurSigned(541.66) = %q, want +€542", got)
	}
}

// TestMMMapsTheDesignCanvas guards the one conversion the whole layout rests
// on: the design is 1240px wide and the page is 210mm, so a design pixel is a
// fixed fraction of a millimetre. If this drifts, every coordinate drifts.
func TestMMMapsTheDesignCanvas(t *testing.T) {
	if got := mm(designW); math.Abs(got-pageW) > 0.001 {
		t.Errorf("mm(%.0f) = %.4f, want the full page width %.1f", designW, got, pageW)
	}
	// 1754px is the design's height and A4 is 297mm; the canvas is A4 at 150dpi.
	if got := mm(1754); math.Abs(got-pageH) > 0.2 {
		t.Errorf("mm(1754) = %.4f, want ~%.1f", got, pageH)
	}
}
