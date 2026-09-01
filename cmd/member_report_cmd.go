package cmd

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/CommonsHub/chb/poster"
)

// MemberReportCommand implements `chb pdf <YYYY/MM>` — a one-page PDF of the
// month, written for the membership rather than for the bookkeeper.
//
// Local-only: it reads generated/ and writes a file. Run `chb pull` (or
// `chb generate <month> --force`) first if the underlying data is stale.
func MemberReportCommand(args []string) error {
	if HasFlag(args, "--help", "-h", "help") {
		printMemberReportHelp()
		return nil
	}

	if HasFlag(args, "--fetch-logo") {
		path, err := FetchCompanyLogoFromOdoo()
		if err != nil {
			return fmt.Errorf("fetching the logo from Odoo: %w", err)
		}
		fmt.Printf("  %s✓ logo saved to %s%s\n", Fmt.Green, path, Fmt.Reset)
		if _, _, ok := ParseYearMonthArg(args); !ok {
			return nil // `chb pdf --fetch-logo` on its own just fetches
		}
	}

	dataDir := DataDir()
	year, month, ok := ParseYearMonthArg(args)
	if !ok || month == "" {
		return fmt.Errorf("usage: chb pdf <YYYY/MM>  (e.g. chb pdf 2026/08)")
	}

	report, err := BuildMemberReport(dataDir, year, month)
	if err != nil {
		return err
	}

	outPath := GetOption(args, "--out")
	if outPath == "" {
		outPath = filepath.Join(dataDir, year, month, "generated",
			fmt.Sprintf("commons-hub-%s-%s.pdf", year, month))
	}
	logoPath := resolveLogoPath(GetOption(args, "--logo"))
	if err := poster.Render(report, outPath, logoPath); err != nil {
		return fmt.Errorf("writing PDF: %w", err)
	}

	info, statErr := os.Stat(outPath)
	size := ""
	if statErr == nil {
		size = fmt.Sprintf(" (%.0f KB)", float64(info.Size())/1024)
	}
	fmt.Printf("\n%s📄 %s%s\n", Fmt.Bold, report.MonthLabel, Fmt.Reset)
	fmt.Printf("   %d members · %s MRR · net %s\n",
		report.ActiveMembers, fmtEUR(report.MRR), fmtEUR(report.Net))
	fmt.Printf("   %s%s%s%s\n\n", Fmt.Cyan, outPath, Fmt.Reset, size)
	if logoPath == "" {
		fmt.Printf("   %sNo logo yet — run `chb pdf --fetch-logo` to pull it from Odoo, or drop a PNG at %s%s\n\n",
			Fmt.Dim, memberReportLogoPath(), Fmt.Reset)
	}
	if report.OdooDerived {
		Warnf("  %s⚠ Membership for this month was reconstructed from a later snapshot and may undercount.%s", Fmt.Yellow, Fmt.Reset)
	}
	return nil
}

func printMemberReportHelp() {
	f := Fmt
	fmt.Printf(`
%schb pdf%s — One-page PDF report of a month, written for members

%sUSAGE%s
  %schb pdf%s <YYYY/MM> [--out <path>]

%sDESCRIPTION%s
  Renders a single A4 page covering one month: headline membership and
  financial figures, where the money came in and went out, a six-month
  trend, the year so far, and anything worth flagging.

  Reads only local generated/ data — run %schb pull%s first if it is stale.
  Nothing is sent anywhere; the PDF is written to disk for you to share.

  By default it lands next to the month's other generated files:
    DATA_DIR/<YYYY>/<MM>/generated/commons-hub-<YYYY>-<MM>.pdf

%sBUDGET%s
  Drop a %ssettings/budget.json%s to compare the year to date against a plan
  instead of a run-rate projection:

    {"year": "2026", "income": 120000, "expenses": 110000}

%sFLAGS%s
  %s--out <path>%s     Write the PDF somewhere else
  %s--logo <path>%s    Use a specific PNG logo for this run
  %s--fetch-logo%s     Copy the company logo out of Odoo (read-only) into
                   settings/logo.png, then reuse it on every report
  %s--help, -h%s       Show this help
`,
		f.Bold, f.Reset,
		f.Bold, f.Reset,
		f.Cyan, f.Reset,
		f.Bold, f.Reset,
		f.Cyan, f.Reset,
		f.Bold, f.Reset,
		f.Cyan, f.Reset,
		f.Bold, f.Reset,
		f.Yellow, f.Reset,
		f.Yellow, f.Reset,
		f.Yellow, f.Reset,
		f.Yellow, f.Reset,
	)
}
