package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"

	odooinvoiceprocessor "github.com/CommonsHub/chb/processors/odooinvoice"
	odoosource "github.com/CommonsHub/chb/providers/odoo"
)

// odooInvoiceProcessor links a bank/on-chain transaction back to the Odoo
// invoice or bill it settles, using the reference the payer typed.
//
// Belgian payments carry a *structured communication* — "+++000/0044/22287+++"
// — which is 12 digits: a zero-padded document id followed by two check digits
// (id mod 97, with 0 meaning 97). Decoding it turns an opaque memo into the
// exact Odoo document, and the mod-97 check makes a false positive a 1-in-97
// coincidence rather than a guess.
//
// The processor does not decide categories. It writes what Odoo already knows
// — document title, journal, product lines — into the transaction's
// fullDescription and metadata, and the rules engine (which runs after
// processors, by design) categorises from there. That keeps the mapping
// declarative in rules.json instead of hardcoded here.
type odooInvoiceProcessor struct {
	docsByID    map[int]odooDocSummary
	docsByTitle map[string]odooDocSummary
	// titles is the title index sorted longest-first, so scanning a
	// description matches "VENE1/2026/00089" before a shorter prefix.
	titles []string
	loaded bool
}

// odooDocSummary is the slice of an Odoo invoice/bill worth attaching to a
// transaction. Deliberately small: names of people are PII and live in the
// private invoice archive, so nothing here can leak into public JSON.
type odooDocSummary struct {
	ID       int
	Title    string
	Kind     string // "invoice" | "bill"
	Journal  string
	Products []string
	Total    float64
	Category string
}

func newOdooInvoiceProcessor() *odooInvoiceProcessor {
	return &odooInvoiceProcessor{}
}

func (p *odooInvoiceProcessor) Name() string {
	return odooinvoiceprocessor.Name
}

func (p *odooInvoiceProcessor) EnvVars() []ProcessorEnvVar {
	return nil
}

// WarmUp indexes every archived invoice and bill, not just the month being
// generated: a payment in August routinely settles an invoice raised in March,
// so a month-scoped index would miss exactly the references worth resolving.
func (p *odooInvoiceProcessor) WarmUp(ctx *ProcessorContext) error {
	p.docsByID = map[int]odooDocSummary{}
	p.docsByTitle = map[string]odooDocSummary{}
	p.titles = nil
	p.loaded = false

	years, err := os.ReadDir(ctx.DataDir)
	if err != nil {
		return err
	}
	for _, y := range years {
		if !y.IsDir() || !isYearDir(y.Name()) {
			continue
		}
		months, err := os.ReadDir(fmt.Sprintf("%s/%s", ctx.DataDir, y.Name()))
		if err != nil {
			continue
		}
		for _, m := range months {
			if !m.IsDir() {
				continue
			}
			p.loadDocs(odoosource.Path(ctx.DataDir, y.Name(), m.Name(), odoosource.InvoicesFile), "invoice")
			p.loadDocs(odoosource.Path(ctx.DataDir, y.Name(), m.Name(), odoosource.BillsFile), "bill")
		}
	}

	for title := range p.docsByTitle {
		p.titles = append(p.titles, title)
	}
	sort.Slice(p.titles, func(i, j int) bool {
		if len(p.titles[i]) != len(p.titles[j]) {
			return len(p.titles[i]) > len(p.titles[j])
		}
		return p.titles[i] < p.titles[j]
	})
	p.loaded = len(p.docsByID) > 0 || len(p.docsByTitle) > 0
	return nil
}

// odooDocFile covers both invoices.json and bills.json, which share a shape
// but not a key.
type odooDocFile struct {
	Invoices []OdooOutgoingInvoicePublic `json:"invoices"`
	Bills    []OdooOutgoingInvoicePublic `json:"bills"`
}

func (p *odooInvoiceProcessor) loadDocs(path, kind string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var f odooDocFile
	if json.Unmarshal(data, &f) != nil {
		return
	}
	docs := f.Invoices
	if kind == "bill" {
		docs = f.Bills
	}
	for _, doc := range docs {
		summary := odooDocSummary{
			ID:       doc.ID,
			Title:    strings.TrimSpace(doc.Title),
			Kind:     kind,
			Journal:  doc.Journal.Name,
			Total:    doc.TotalAmount,
			Category: firstNonEmptyString(doc.Categories),
		}
		for _, li := range doc.LineItems {
			// line_note rows carry free text, not a product; they are the
			// operator's prose and often repeat the product name.
			if li.DisplayType != "product" {
				continue
			}
			if name := strings.TrimSpace(firstNonEmptyString([]string{li.ProductName, li.Title})); name != "" {
				summary.Products = append(summary.Products, name)
			}
		}
		if doc.ID > 0 {
			p.docsByID[doc.ID] = summary
		}
		if isMatchableDocTitle(summary.Title) {
			p.docsByTitle[summary.Title] = summary
		}
	}
}

func (p *odooInvoiceProcessor) ProcessTransaction(ctx *ProcessorContext, tx *TransactionEntry) error {
	if !p.loaded {
		return nil
	}
	haystack := strings.TrimSpace(strings.Join([]string{
		stringMetadata(tx.Metadata, "description"),
		stringMetadata(tx.Metadata, "memo"),
		stringMetadata(tx.Metadata, "fullDescription"),
	}, " "))
	if haystack == "" {
		return nil
	}

	doc, ok := p.resolve(haystack)
	if !ok {
		return nil
	}

	if tx.Metadata == nil {
		tx.Metadata = map[string]interface{}{}
	}
	tx.Metadata["odooDocId"] = doc.ID
	tx.Metadata["odooDocType"] = doc.Kind
	if doc.Title != "" {
		tx.Metadata["odooDocTitle"] = doc.Title
	}
	if doc.Journal != "" {
		tx.Metadata["odooJournal"] = doc.Journal
	}
	if len(doc.Products) > 0 {
		tx.Metadata["odooProducts"] = strings.Join(doc.Products, ", ")
		// productName is the slot the rules engine's `product:` matcher
		// reads (see RuleMatch.Product). Stripe fills it from checkout line
		// items; an Odoo document's first line is the same kind of evidence,
		// and matching on it is far tighter than globbing a free-text memo —
		// `product: "*room*"` cannot be tripped by a bank narration that
		// happens to contain the word.
		if tx.Metadata["productName"] == nil || tx.Metadata["productName"] == "" {
			tx.Metadata["productName"] = doc.Products[0]
		}
	}

	// Fold the document's own words into fullDescription. The rule matcher
	// globs over description/memo/fullDescription, so "Coworking Day Pass"
	// arriving from Odoo lets an existing *cowork* rule fire on a payment
	// whose memo was nothing but a 12-digit reference.
	//
	// Only the FIRST product line joins the match text, mirroring how
	// matchMoveRule treats invoices (see cmd/rules.go): an invoice whose
	// first line is "Mush Room" and whose second is "Coffee, tea, water" is a
	// room rental, and folding in every line would let a *coffee* rule claim
	// it. The full list stays in odooProducts for humans.
	//
	// The journal name is excluded for the same reason. Odoo journals are
	// named after the bucket they collect ("Factures clients
	// (RENTAL-COWORKING-CATERING)"), so feeding one to a glob matcher makes
	// every document under it match all three categories at once — whichever
	// rule is ordered first wins, and the answer is arbitrary.
	parts := []string{haystack, doc.Title}
	if len(doc.Products) > 0 {
		parts = append(parts, doc.Products[0])
	}
	tx.Metadata["fullDescription"] = strings.Join(nonEmptyStrings(parts), " · ")

	addTransactionTag(&tx.Tags, "source", odooinvoiceprocessor.Name)
	addTransactionTag(&tx.Tags, "odoo", strconv.Itoa(doc.ID))
	tx.Tags = normalizeTransactionTags(tx.Tags)
	return nil
}

// resolve finds the Odoo document a description refers to. Structured
// communications win over title scanning: they are checksummed, so a hit is
// near-certain, while a title can appear inside unrelated prose.
func (p *odooInvoiceProcessor) resolve(desc string) (odooDocSummary, bool) {
	for _, id := range structuredCommunicationIDs(desc) {
		if doc, ok := p.docsByID[id]; ok {
			return doc, true
		}
	}
	upper := strings.ToUpper(desc)
	for _, title := range p.titles {
		if strings.Contains(upper, strings.ToUpper(title)) {
			return p.docsByTitle[title], true
		}
	}
	return odooDocSummary{}, false
}

var (
	// "+++000/0044/22287+++" and the "***…***" variant.
	structuredCommRe = regexp.MustCompile(`[+*]{3}(\d{3})/(\d{4})/(\d{5})[+*]{3}`)
	// The same 12 digits pasted without separators, as some banks render them.
	bareTwelveDigitRe = regexp.MustCompile(`\b(\d{12})\b`)
)

// structuredCommunicationIDs extracts every Belgian structured communication in
// s and returns the document ids they encode. The trailing two digits are a
// mod-97 checksum over the leading ten; entries that fail it are dropped, which
// is what keeps a bare 12-digit invoice number or card token from being read as
// a reference.
func structuredCommunicationIDs(s string) []int {
	var out []int
	seen := map[int]bool{}
	add := func(digits string) {
		id, ok := decodeStructuredCommunication(digits)
		if ok && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	for _, m := range structuredCommRe.FindAllStringSubmatch(s, -1) {
		add(m[1] + m[2] + m[3])
	}
	for _, m := range bareTwelveDigitRe.FindAllStringSubmatch(s, -1) {
		add(m[1])
	}
	return out
}

// decodeStructuredCommunication validates the 12-digit form and returns the
// document id it carries. Belgian rule: check = base mod 97, and a remainder of
// 0 is written as 97.
func decodeStructuredCommunication(digits string) (int, bool) {
	if len(digits) != 12 {
		return 0, false
	}
	base, err := strconv.ParseInt(digits[:10], 10, 64)
	if err != nil {
		return 0, false
	}
	check, err := strconv.Atoi(digits[10:])
	if err != nil {
		return 0, false
	}
	want := int(base % 97)
	if want == 0 {
		want = 97
	}
	if check != want || base <= 0 {
		return 0, false
	}
	return int(base), true
}

// isMatchableDocTitle rejects titles too short or too generic to scan for
// inside free text. A structured communication rendered as a title ("+++…+++")
// is already covered by the checksum path.
func isMatchableDocTitle(title string) bool {
	t := strings.TrimSpace(title)
	if len(t) < 5 || strings.HasPrefix(t, "+++") || strings.HasPrefix(t, "***") {
		return false
	}
	// Require at least one digit and one letter: "Reversal of: 2026_RCC_52"
	// style prose and bare numbers are both unreliable anchors.
	var hasDigit, hasAlpha bool
	for _, r := range t {
		switch {
		case r >= '0' && r <= '9':
			hasDigit = true
		case (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z'):
			hasAlpha = true
		}
	}
	return hasDigit && hasAlpha && !strings.Contains(strings.ToLower(t), "reversal of")
}

func (p *odooInvoiceProcessor) ProcessEvent(ctx *ProcessorContext, ev *FullEvent) error {
	return nil
}

func (p *odooInvoiceProcessor) Flush(ctx *ProcessorContext) error {
	return nil
}

func isYearDir(name string) bool {
	if len(name) != 4 {
		return false
	}
	for _, r := range name {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func firstNonEmptyString(values []string) string {
	for _, v := range values {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

func nonEmptyStrings(values []string) []string {
	out := make([]string, 0, len(values))
	for _, v := range values {
		if s := strings.TrimSpace(v); s != "" {
			out = append(out, s)
		}
	}
	return out
}
