package poster

import (
	_ "embed"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"

	"github.com/CommonsHub/chb/money"
	"github.com/go-pdf/fpdf"
)

// The page is a direct port of the "Monthly Wall Poster" design, whose canvas
// is 1240 x 1754 px — exactly A4 at 150dpi. Every measurement below is the
// design's own pixel value put through mm() or pt(), so the layout can be
// checked against the source without doing arithmetic in your head.
const (
	designW = 1240.0
	pageW   = 210.0
	pageH   = 297.0
)

// mm converts a design pixel to millimetres, fpdf's unit.
func mm(px float64) float64 { return px * pageW / designW }

// pt converts a design pixel to a font size in points.
func pt(px float64) float64 { return mm(px) * 72.0 / 25.4 }

// Nunito is vendored rather than fetched: the renderer must work offline, and
// typography that depends on a network call is not reproducible. Three weights
// cover the design's 400 / 600-700 / 800-900 usage.
var (
	//go:embed fonts/Nunito-Regular.ttf
	nunitoRegular []byte
	//go:embed fonts/Nunito-Bold.ttf
	nunitoBold []byte
	//go:embed fonts/Nunito-Black.ttf
	nunitoBlack []byte
)

// fpdf keys faces by (family, style). The design's three weights are
// registered against three style slots; "BI" is simply an unused slot, not
// italic.
const (
	fontFamily  = "Nunito"
	fontRegular = ""
	fontBold    = "B"
	fontBlack   = "BI"
)

// Palette, straight from the design's hex values. Its rgba() borders and bar
// tracks are pre-composited against the background they sit on.
var (
	colInk      = [3]int{36, 26, 20}    // #241A14
	colInkSoft  = [3]int{61, 48, 42}    // #3D302A
	colBody     = [3]int{92, 78, 70}    // #5C4E46
	colMuted    = [3]int{138, 119, 108} // #8A776C
	colFaint    = [3]int{162, 143, 132} // #A28F84
	colAccent   = [3]int{255, 76, 2}    // #FF4C02
	colPage     = [3]int{255, 246, 241} // #FFF6F1
	colCard     = [3]int{255, 253, 252} // #FFFDFC
	colCardEdge = [3]int{229, 222, 218} // rgba(36,26,20,0.12) over #FFF6F1
	colTrack    = [3]int{235, 229, 225} // rgba(36,26,20,0.09) over #FFF6F1
	colElse     = [3]int{192, 170, 158} // #C0AA9E
	colOnAccent = [3]int{255, 246, 241} // #FFF6F1 on orange
	colDimLabel = [3]int{255, 178, 143} // 0.8 opacity label on orange
	colBarDark  = [3]int{43, 26, 16}    // #2B1A10
	colBarTrack = [3]int{209, 163, 140} // rgba(36,26,20,0.18) over orange
)

// Layout constants from the design's padding and gaps.
var (
	padX     = mm(80)
	padTop   = mm(56)
	padBot   = mm(44)
	innerW   = pageW - 2*padX
	sectionG = mm(26)
)

// Render writes the one-page member report to outPath. logoPath may be empty,
// in which case the header falls back to type alone.
func Render(r *Report, outPath, logoPath string) error {
	pdf := fpdf.New("P", "mm", "A4", "")
	pdf.AddUTF8FontFromBytes(fontFamily, fontRegular, nunitoRegular)
	pdf.AddUTF8FontFromBytes(fontFamily, fontBold, nunitoBold)
	pdf.AddUTF8FontFromBytes(fontFamily, fontBlack, nunitoBlack)
	pdf.SetMargins(padX, padTop, padX)
	pdf.SetAutoPageBreak(false, 0) // one page, by construction
	pdf.AddPage()

	// The design's page colour covers the whole sheet, not just the content.
	setFill(pdf, colPage)
	pdf.Rect(0, 0, pageW, pageH, "F")

	y := padTop
	y = drawPosterHeader(pdf, r, y, logoPath)
	y = drawPeople(pdf, r, y+sectionG)
	y = drawMoneyPanel(pdf, r, y+sectionG)
	y = drawTwoSides(pdf, r, y+sectionG)
	y = drawHubLineByLine(pdf, r, y+sectionG)
	y = drawTokenCards(pdf, r, y+sectionG)
	drawCaveatsAndFooter(pdf, r, y+sectionG)

	if dir := filepath.Dir(outPath); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return pdf.OutputFileAndClose(outPath)
}

func setInk(pdf *fpdf.Fpdf, c [3]int)  { pdf.SetTextColor(c[0], c[1], c[2]) }
func setFill(pdf *fpdf.Fpdf, c [3]int) { pdf.SetFillColor(c[0], c[1], c[2]) }
func setDraw(pdf *fpdf.Fpdf, c [3]int) { pdf.SetDrawColor(c[0], c[1], c[2]) }

// face sets family, style, size and colour in one call, since every text run
// in the design specifies all four.
func face(pdf *fpdf.Fpdf, style string, sizePx float64, colour [3]int) {
	pdf.SetFont(fontFamily, style, pt(sizePx))
	setInk(pdf, colour)
}

// fontH returns the current font size in millimetres.
//
// GetFontSize returns (points, units) — the second value is already in the
// document's unit. Converting it again through PointConvert shrank every
// baseline offset and line height by a factor of 2.83, which is what stacked
// wrapped paragraphs on top of each other.
func fontH(pdf *fpdf.Fpdf) float64 {
	_, unitSize := pdf.GetFontSize()
	return unitSize
}

// text draws one line whose *visual top* is y.
//
// CellFormat centres text vertically inside the cell it is given, which for a
// 104px number means the glyphs climb well above the y that was asked for and
// collide with whatever sits above. pdf.Text places an explicit baseline
// instead, so every offset below means what the design says it means. 0.76em
// is Nunito's cap-top to baseline distance.
func text(pdf *fpdf.Fpdf, x, y float64, s string) {
	pdf.Text(x, y+fontH(pdf)*0.76, s)
}

// textR draws one line right-aligned to x+w, top-anchored like text.
func textR(pdf *fpdf.Fpdf, x, y, w float64, s string) {
	pdf.Text(x+w-pdf.GetStringWidth(s), y+fontH(pdf)*0.76, s)
}

// para draws wrapped text with its top at y and returns the y below it.
//
// It splits and draws line by line rather than calling MultiCell, so wrapped
// text is top-anchored exactly like text() and the caller's y arithmetic keeps
// working. MultiCell's own vertical placement disagreed with pdf.Text by half
// a line, which stacked paragraphs on top of each other.
func para(pdf *fpdf.Fpdf, x, y, w float64, s string) float64 {
	return paraLead(pdf, x, y, w, 1.38, s)
}

// paraLead is para with an explicit leading multiplier, for the places where
// the design sets a tighter line-height than running text.
func paraLead(pdf *fpdf.Fpdf, x, y, w, lead float64, s string) float64 {
	lh := fontH(pdf) * lead
	for _, line := range pdf.SplitLines([]byte(s), w) {
		text(pdf, x, y, string(line))
		y += lh
	}
	return y
}

// eur renders an amount the way the poster does: euro sign, thousands
// separated, no cents. A wall poster is read from across the room, and cents
// are noise at 100px. U+2212 is used for the minus — a hyphen reads as a dash
// at these sizes.
func eur(v float64) string {
	// Decide the sign *after* rounding: -0.40 rounds to zero, and "−€0" is
	// both wrong and faintly absurd on a wall poster.
	rounded := math.Round(v)
	s := "€" + trimCents(money.Number(math.Abs(rounded)))
	if rounded < 0 {
		return "−" + s
	}
	return s
}

func eurSigned(v float64) string {
	if v >= 0 {
		return "+" + eur(v)
	}
	return eur(v)
}

func trimCents(s string) string {
	if i := strings.Index(s, "."); i >= 0 {
		return s[:i]
	}
	return s
}

func drawPosterHeader(pdf *fpdf.Fpdf, r *Report, y float64, logoPath string) float64 {
	const logoPx = 104.0
	x := padX
	if logoPath != "" {
		pdf.ImageOptions(logoPath, x, y, mm(logoPx), mm(logoPx),
			false, fpdf.ImageOptions{ImageType: "PNG", ReadDpi: true}, 0, "")
		x += mm(logoPx + 26)
	}

	face(pdf, fontBlack, 42, colInk)
	text(pdf, x, y+mm(20), "Commons Hub Brussels")
	face(pdf, fontBold, 27, colMuted)
	text(pdf, x, y+mm(58), "Our month, in numbers")

	label := strings.ToUpper(r.MonthLabel)
	face(pdf, fontBlack, 27, colOnAccent)
	// fpdf has no letter-spacing, so the design's 0.08em tracking is
	// approximated by padding the pill rather than the glyphs.
	w := pdf.GetStringWidth(label) + mm(48)
	h := mm(52)
	px := padX + innerW - w
	setFill(pdf, colAccent)
	pdf.RoundedRect(px, y+mm(16), w, h, h/2, "1234", "F")
	setInk(pdf, colOnAccent)
	pdf.Text(px+(w-pdf.GetStringWidth(label))/2, y+mm(16)+h/2+fontH(pdf)*0.36, label)

	return y + mm(logoPx)
}

func drawPeople(pdf *fpdf.Fpdf, r *Report, y float64) float64 {
	y = drawSectionLabel(pdf, y, "THE PEOPLE")

	cols := []struct{ big, delta, caption string }{
		{fmt.Sprintf("%d", r.ActiveMembers), signedInt(r.MemberDelta), "people are members"},
		{fmt.Sprintf("%d", r.Contributors), "", "took part this month"},
		{eur(r.MRR), deltaOrEmpty(r.MRRDelta), "in memberships monthly"},
	}
	colW := (innerW - mm(64)) / 3
	for i, c := range cols {
		x := padX + float64(i)*(colW+mm(32))
		if i > 0 {
			setDraw(pdf, colCardEdge)
			pdf.SetLineWidth(mm(2))
			pdf.Line(x-mm(16), y, x-mm(16), y+mm(92))
			x += mm(16)
		}
		face(pdf, fontBlack, 78, colInk)
		text(pdf, x, y, c.big)
		if c.delta != "" {
			w := pdf.GetStringWidth(c.big) + mm(14)
			face(pdf, fontBlack, 25, colAccent)
			text(pdf, x+w, y+mm(24), c.delta)
		}
		face(pdf, fontBold, 26, colBody)
		text(pdf, x, y+mm(70), c.caption)
	}
	return y + mm(92)
}

func deltaOrEmpty(v float64) string {
	if math.Abs(v) < 0.5 {
		return ""
	}
	return eurSigned(v)
}

// drawMoneyPanel is the orange block: what the hub holds, and how the month
// closed. It is the loudest thing on the page because it answers the two
// questions members actually ask.
func drawMoneyPanel(pdf *fpdf.Fpdf, r *Report, y float64) float64 {
	// Tall enough for both bars plus the design's inner padding: the label,
	// gap and bar of each row come to mm(44), and the second row previously
	// ran past the rounded corner.
	h := mm(258)
	setFill(pdf, colAccent)
	pdf.RoundedRect(padX, y, innerW, h, mm(26), "1234", "F")

	leftW := (innerW - mm(120)) * 1.08 / 2.08
	lx := padX + mm(40)

	var held float64
	for _, b := range r.Balances {
		if money.IsEUR(b.Currency) {
			held += b.Ending
		}
	}

	face(pdf, fontBold, 23, colDimLabel)
	text(pdf, lx, y+mm(26), "WE HOLD RIGHT NOW")
	face(pdf, fontBlack, 104, colOnAccent)
	text(pdf, lx, y+mm(74), eur(held))

	var parts []string
	for i, b := range r.Balances {
		if i >= 5 {
			break
		}
		label := b.Label
		if j := strings.Index(label, " ("); j > 0 {
			label = label[:j]
		}
		parts = append(parts, label+" "+eur(b.Ending))
	}
	face(pdf, fontBold, 22, colOnAccent)
	para(pdf, lx, y+mm(178), leftW, strings.Join(parts, " · "))

	rx := lx + leftW + mm(80)
	rw := padX + innerW - mm(40) - rx
	setDraw(pdf, colBarTrack)
	pdf.SetLineWidth(mm(2))
	pdf.Line(rx-mm(40), y+mm(26), rx-mm(40), y+h-mm(26))

	face(pdf, fontBold, 23, colDimLabel)
	text(pdf, rx, y+mm(26), "LEFT OVER THIS MONTH")
	face(pdf, fontBlack, 68, colOnAccent)
	text(pdf, rx, y+mm(74), eur(r.NetAfterFixed))

	// Money out includes what the hub owes but has not been invoiced, so both
	// bars answer the same question on the same basis.
	moneyIn := r.Income
	moneyOut := r.Income - r.NetAfterFixed
	scale := math.Max(moneyIn, moneyOut)
	by := y + mm(134)
	by = drawPanelBar(pdf, rx, by, rw, "Money in", moneyIn, scale, colOnAccent)
	drawPanelBar(pdf, rx, by+mm(16), rw, "Money out", moneyOut, scale, colBarDark)

	return y + h
}

func drawPanelBar(pdf *fpdf.Fpdf, x, y, w float64, label string, amount, scale float64, fill [3]int) float64 {
	face(pdf, fontBold, 22, colOnAccent)
	text(pdf, x, y, label)
	textR(pdf, x, y, w, eur(amount))
	barY := y + mm(28)
	setFill(pdf, colBarTrack)
	pdf.RoundedRect(x, barY, w, mm(16), mm(8), "1234", "F")
	if scale > 0 && amount > 0 {
		setFill(pdf, fill)
		pdf.RoundedRect(x, barY, math.Max(w*amount/scale, mm(16)), mm(16), mm(8), "1234", "F")
	}
	return barY + mm(16)
}

func drawTwoSides(pdf *fpdf.Fpdf, r *Report, y float64) float64 {
	y = drawSectionLabel(pdf, y, "THE TWO SIDES OF THE HUB")

	hubNote := "the space and everything that keeps it open"
	if r.HubLine.Unbilled > 0 {
		hubNote = fmt.Sprintf("includes %s rent owed, not yet invoiced", eur(r.HubLine.Unbilled))
	}
	cards := []struct{ title, net, flows, note string }{
		{r.HubLine.Name, eur(r.HubLine.NetAfterFixed),
			fmt.Sprintf("In %s · Out %s", eur(r.HubLine.Income), eur(r.HubLine.ExpensesInclUnbilled)), hubNote},
		{r.EventLine.Name, eurSigned(r.EventLine.Net),
			fmt.Sprintf("In %s · Out %s", eur(r.EventLine.Income), eur(r.EventLine.Expenses)), eventsNote(r)},
		{"Shared, not yet split", eurSigned(r.SharedLine.Net),
			fmt.Sprintf("In %s · Out %s", eur(r.SharedLine.Income), eur(r.SharedLine.Expenses)),
			"belongs to one side or the other"},
	}

	cardW := (innerW - mm(36)) / 3
	cardH := mm(190)
	for i, c := range cards {
		x := padX + float64(i)*(cardW+mm(18))
		setFill(pdf, colCard)
		setDraw(pdf, colCardEdge)
		pdf.SetLineWidth(mm(2))
		pdf.RoundedRect(x, y, cardW, cardH, mm(20), "1234", "FD")

		face(pdf, fontBold, 25, colInk)
		text(pdf, x+mm(20), y+mm(16), c.title)
		face(pdf, fontBlack, 46, colInk)
		text(pdf, x+mm(20), y+mm(52), c.net)
		face(pdf, fontBold, 22, colBody)
		text(pdf, x+mm(20), y+mm(106), c.flows)
		face(pdf, fontBold, 20, colMuted)
		paraLead(pdf, x+mm(20), y+mm(136), cardW-mm(40), 1.2, c.note)
	}
	return y + cardH
}

// eventsNote states the relationship between the two sides only when the data
// supports it, rather than asserting the design's copy every month.
func eventsNote(r *Report) string {
	if r.EventLine.Net > 0 && r.HubLine.NetAfterFixed < 0 {
		return "events carry the hub this month"
	}
	return "hiring the rooms out, and catering them"
}

func drawHubLineByLine(pdf *fpdf.Fpdf, r *Report, y float64) float64 {
	face(pdf, fontBold, 23, colMuted)
	text(pdf, padX, y, "RUNNING THE HUB, LINE BY LINE")
	w := pdf.GetStringWidth("RUNNING THE HUB, LINE BY LINE")
	face(pdf, fontBold, 20, colFaint)
	text(pdf, padX+w+mm(16), y+mm(2), "events counted separately")
	y += mm(44)

	colW := (innerW - mm(40)) / 2

	var inRows []detailRow
	for _, l := range r.HubLine.In {
		inRows = append(inRows, detailRow{l.Label, l.Amount, false})
	}
	// The unbilled commitment leads the outgoings — it is the largest of them
	// — and is drawn as an outline, because it is owed rather than spent.
	var outRows []detailRow
	for _, fc := range r.HubLine.FixedCosts {
		if fc.Missing {
			outRows = append(outRows, detailRow{fc.Label + " (not yet invoiced)", fc.Typical, true})
		}
	}
	for _, l := range r.HubLine.Out {
		outRows = append(outRows, detailRow{l.Label, l.Amount, false})
	}

	left := drawDetailColumn(pdf, padX, y, colW, "CAME IN", r.HubLine.Income, inRows, colAccent)
	right := drawDetailColumn(pdf, padX+colW+mm(40), y, colW, "WENT OUT", r.HubLine.ExpensesInclUnbilled, outRows, colInk)
	return math.Max(left, right)
}

type detailRow struct {
	label   string
	amount  float64
	outline bool // owed rather than paid
}

func drawDetailColumn(pdf *fpdf.Fpdf, x, y, w float64, title string, total float64, rows []detailRow, bar [3]int) float64 {
	face(pdf, fontBlack, 24, colInk)
	text(pdf, x, y+mm(6), title)
	face(pdf, fontBlack, 30, colInk)
	textR(pdf, x, y, w, eur(total))

	ruleY := y + mm(36)
	setDraw(pdf, colInk)
	pdf.SetLineWidth(mm(3))
	pdf.Line(x, ruleY, x+w, ruleY)

	// Five rows keeps the poster legible; the tail folds into one line.
	const maxRows = 5
	var rest float64
	if len(rows) > maxRows {
		for _, rw := range rows[maxRows:] {
			rest += rw.amount
		}
		rows = rows[:maxRows]
	}

	labelW := mm(250)
	amountW := mm(108)
	barW := w - labelW - amountW - mm(28)
	cur := ruleY + mm(16)

	draw := func(rw detailRow, muted bool) {
		colour, fill := colInk, bar
		if muted {
			colour, fill = colMuted, colElse
		}
		face(pdf, fontBold, 21, colour)
		text(pdf, x, cur, rw.label)
		textR(pdf, x+w-amountW, cur, amountW, eur(rw.amount))

		share := 0.0
		if total > 0 {
			share = math.Min(rw.amount/total, 1)
		}
		bx := x + labelW + mm(14)
		byy := cur + mm(12)
		setFill(pdf, colTrack)
		pdf.RoundedRect(bx, byy, barW, mm(10), mm(5), "1234", "F")
		if share > 0 {
			bw := math.Max(barW*share, mm(10))
			if rw.outline {
				// Owed, not paid: an outline states the amount without
				// claiming the money moved.
				setDraw(pdf, colInk)
				pdf.SetLineWidth(mm(2))
				pdf.RoundedRect(bx, byy, bw, mm(10), mm(5), "1234", "D")
			} else {
				setFill(pdf, fill)
				pdf.RoundedRect(bx, byy, bw, mm(10), mm(5), "1234", "F")
			}
		}
		cur += mm(29)
	}
	for _, rw := range rows {
		draw(rw, false)
	}
	if rest > 0 {
		draw(detailRow{"Everything else", rest, false}, true)
	}
	return cur
}

func drawTokenCards(pdf *fpdf.Fpdf, r *Report, y float64) float64 {
	if r.EURToken == nil && len(r.Tokens) == 0 {
		return y
	}
	y = drawSectionLabel(pdf, y, "THE TOKENS WE USE")

	type stat struct{ value, label string }
	var cards []struct {
		title string
		stats []stat
	}
	if t := r.EURToken; t != nil {
		held := eur(t.Held)
		if !t.HeldKnown {
			held = "—"
		}
		cards = append(cards, struct {
			title string
			stats []stat
		}{"Euro · " + t.Symbol, []stat{
			{held, "held"}, {eur(t.Received), "in"}, {eur(t.Spent), "out"},
			{fmt.Sprintf("%d", t.Wallets), "accounts"},
		}})
	}
	if len(r.Tokens) > 0 {
		t := r.Tokens[0]
		cards = append(cards, struct {
			title string
			stats []stat
		}{"Commons Hub token · " + t.Symbol, []stat{
			{fmtNumberInt(t.Supply), "circulating"},
			{fmtNumberInt(t.Minted), "earned"},
			{fmtNumberInt(t.Burnt), "spent"},
			{fmt.Sprintf("%d / %d", t.Holders, t.Active), "hold / active"},
		}})
	}

	cardW := (innerW - mm(16)*float64(len(cards)-1)) / float64(len(cards))
	cardH := mm(118)
	for i, c := range cards {
		x := padX + float64(i)*(cardW+mm(16))
		setFill(pdf, colCard)
		setDraw(pdf, colCardEdge)
		pdf.SetLineWidth(mm(2))
		pdf.RoundedRect(x, y, cardW, cardH, mm(20), "1234", "FD")

		face(pdf, fontBold, 22, colBody)
		text(pdf, x+mm(18), y+mm(14), c.title)

		sw := (cardW - mm(36)) / float64(len(c.stats))
		for j, st := range c.stats {
			sx := x + mm(18) + float64(j)*sw
			if j > 0 {
				setDraw(pdf, colCardEdge)
				pdf.SetLineWidth(mm(2))
				pdf.Line(sx-mm(6), y+mm(52), sx-mm(6), y+mm(100))
			}
			face(pdf, fontBlack, 26, colInk)
			text(pdf, sx, y+mm(56), st.value)
			face(pdf, fontBold, 18, colMuted)
			text(pdf, sx, y+mm(84), st.label)
		}
	}
	return y + cardH
}

func drawCaveatsAndFooter(pdf *fpdf.Fpdf, r *Report, y float64) {
	// The footer is pinned to the bottom margin and the caveats hang off it,
	// mirroring the design's margin-top:auto. Laying either out from wherever
	// the flow happened to end leaves a hole in a quiet month and runs off the
	// sheet in a busy one.
	face(pdf, fontBold, 18, colFaint)
	footTop := pageH - padBot - fontH(pdf)*1.38*3
	ruleY := footTop - mm(22)
	top := ruleY - mm(200)
	if y > top {
		top = y
	}

	setDraw(pdf, colCardEdge)
	pdf.SetLineWidth(mm(2))
	pdf.Line(padX, top, padX+innerW, top)
	top += mm(14)

	colW := (innerW - mm(44)) / 2
	rx := padX + colW + mm(44)

	face(pdf, fontBold, 23, colMuted)
	text(pdf, padX, top, "WORTH KNOWING")
	cur := top + mm(40)
	for i, note := range posterHighlights(r.Highlights) {
		if i >= 3 {
			break
		}
		face(pdf, fontBold, 22, colInkSoft)
		// Measure before drawing: a note that wraps to two lines would
		// otherwise clear the guard on its first line and land on the footer
		// rule with its second.
		need := float64(len(pdf.SplitLines([]byte(note), colW))) * fontH(pdf) * 1.38
		if cur+need > ruleY-mm(10) {
			break
		}
		cur = para(pdf, padX, cur, colW, note) + mm(4)
	}

	face(pdf, fontBold, 23, colMuted)
	text(pdf, rx, top, "THE YEAR SO FAR")
	face(pdf, fontBlack, 46, colInk)
	text(pdf, rx, top+mm(36), eur(r.YTD.Net))
	if o := r.Outlier; o != nil {
		face(pdf, fontBold, 22, colInkSoft)
		para(pdf, rx, top+mm(104), colW, fmt.Sprintf(
			"%s alone accounts for %s of that. Without %s: %s.",
			o.Label, eur(-o.Outflow), strings.Fields(o.Label)[0], eur(o.NetExcl)))
	}

	setDraw(pdf, colCardEdge)
	pdf.SetLineWidth(mm(2))
	pdf.Line(padX, ruleY, padX+innerW, ruleY)

	face(pdf, fontBold, 18, colFaint)
	para(pdf, padX, footTop, colW, fmt.Sprintf(
		"Generated by chb from the Commons Hub books. Figures cover %s, rounded to the nearest euro. Expenses include VAT and payment fees.",
		r.MonthLabel))
	if len(r.Collectives) > 0 {
		var total float64
		var parts []string
		for _, c := range r.Collectives {
			total += c.Amount
			parts = append(parts, c.Label+" ("+eur(c.Amount)+")")
		}
		face(pdf, fontBold, 18, colFaint)
		para(pdf, rx, footTop, colW, fmt.Sprintf(
			"Of what we hold, %s is not ours to spend — we look after it for %s.",
			eur(total), strings.Join(parts, " and ")))
	}
}

// posterHighlights writes the month's standout movements in the poster's own
// voice and money format. Building them from the figures rather than reworking
// the terminal strings keeps the euro signs and avoids truncating a sentence
// mid-word.
func posterHighlights(h Highlights) []string {
	var out []string
	if h.BiggestIn > 0 {
		out = append(out, "Biggest payment in: "+eur(h.BiggestIn)+" — "+shortWhat(h.BiggestInWhat))
	}
	if h.BiggestOut > 0 {
		out = append(out, "Biggest single expense: "+eur(h.BiggestOut)+" — "+shortWhat(h.BiggestOutWhat))
	}
	if h.UncategorisedN > 0 {
		out = append(out, fmt.Sprintf("%s in %s isn't sorted yet",
			eur(h.UncategorisedIn), plural(h.UncategorisedN, "payment")))
	}
	return out
}

// shortWhat keeps a description to its first clause. Odoo line items string
// several products together, which is useful in a ledger and unreadable on a
// wall.
func shortWhat(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, ","); i > 0 {
		s = s[:i]
	}
	if len(s) > 46 {
		s = strings.TrimSpace(s[:43]) + "…"
	}
	if s == "" {
		return "no description recorded"
	}
	return s
}

func drawSectionLabel(pdf *fpdf.Fpdf, y float64, label string) float64 {
	face(pdf, fontBold, 23, colMuted)
	text(pdf, padX, y, label)
	return y + mm(46)
}

// fmtNumberInt renders a token count without decimals — CHT is whole-unit.
func fmtNumberInt(v float64) string { return trimCents(money.Number(math.Round(v))) }

func signedInt(v int) string {
	switch {
	case v > 0:
		return fmt.Sprintf("+%d", v)
	case v < 0:
		return fmt.Sprintf("−%d", -v)
	default:
		return ""
	}
}

// The money package is the shared definition of how the hub writes and groups
// figures, and of which currency codes count as euro. Importing it — rather
// than keeping a local copy — is what stops this page and the terminal report
// disagreeing about the same month.

// plural renders "1 payment" / "3 payments".
func plural(n int, singular string) string {
	if n == 1 || n == -1 {
		return fmt.Sprintf("%d %s", n, singular)
	}
	return fmt.Sprintf("%d %ss", n, singular)
}
