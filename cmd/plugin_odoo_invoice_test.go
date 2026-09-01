package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	odoosource "github.com/CommonsHub/chb/providers/odoo"
)

func TestDecodeStructuredCommunication(t *testing.T) {
	// Real references from the August 2026 books; the trailing pair is
	// base mod 97 (0 written as 97).
	for _, tc := range []struct {
		digits string
		want   int
		ok     bool
	}{
		{"000004430472", 44304, true}, // +++000/0044/30472+++
		{"000004422287", 44222, true}, // +++000/0044/22287+++
		{"000004346610", 43466, true}, // +++000/0043/46610+++
		{"000004422186", 44221, true}, // +++000/0044/22186+++
		{"000004430473", 0, false},    // check digits off by one
		{"00000443047", 0, false},     // too short
		{"0000044304721", 0, false},   // too long
		{"000000000000", 0, false},    // no document
		{"12345678901x", 0, false},    // not digits
	} {
		got, ok := decodeStructuredCommunication(tc.digits)
		if ok != tc.ok || got != tc.want {
			t.Errorf("decodeStructuredCommunication(%q) = (%d, %v), want (%d, %v)",
				tc.digits, got, ok, tc.want, tc.ok)
		}
	}
}

func TestStructuredCommunicationIDsExtractsBothForms(t *testing.T) {
	for _, tc := range []struct {
		desc string
		want []int
	}{
		{"+++000/0044/22287+++", []int{44222}},
		{"***000/0044/22287***", []int{44222}},
		{"000004430472", []int{44304}},
		{"payment ref 000004430472 received", []int{44304}},
		// A card token that happens to be 12 digits must not resolve.
		{"CP-06A5E796 400000000000", nil},
		{"no reference here", nil},
	} {
		got := structuredCommunicationIDs(tc.desc)
		if len(got) != len(tc.want) {
			t.Errorf("structuredCommunicationIDs(%q) = %v, want %v", tc.desc, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("structuredCommunicationIDs(%q) = %v, want %v", tc.desc, got, tc.want)
				break
			}
		}
	}
}

func TestIsMatchableDocTitle(t *testing.T) {
	for _, tc := range []struct {
		title string
		want  bool
	}{
		{"S00193", true},
		{"VENE1/2026/00089", true},
		{"MEM/2026/00070", true},
		{"+++000/0044/22287+++", false}, // covered by the checksum path
		{"S001", false},                 // too short to scan for
		{"Invoice", false},              // no digits
		{"12345678", false},             // no letters
		{"Reversal of: 2026_RCC_52, do", false},
	} {
		if got := isMatchableDocTitle(tc.title); got != tc.want {
			t.Errorf("isMatchableDocTitle(%q) = %v, want %v", tc.title, got, tc.want)
		}
	}
}

// TestOdooInvoiceProcessorEnrichesFromReference is the end-to-end contract: a
// transaction whose memo is nothing but a structured communication comes out
// carrying the invoice's own words, so the rules engine — which runs after
// processors — can categorise it without any per-transaction rule.
func TestOdooInvoiceProcessorEnrichesFromReference(t *testing.T) {
	dataDir := t.TempDir()
	writeTestInvoices(t, dataDir, "2026", "08", "invoices", []OdooOutgoingInvoicePublic{
		{
			ID:          44304,
			Title:       "+++000/0044/30472+++",
			TotalAmount: 242.00,
			Journal:     OdooInvoiceJournal{ID: 11, Name: "Factures clients (RENTAL-COWORKING-CATERING)"},
			LineItems: []OdooInvoiceLineItem{
				{ProductName: "Coworking Day Pass", DisplayType: "product"},
				{Title: "a free-text note", DisplayType: "line_note"},
			},
		},
	})
	writeTestInvoices(t, dataDir, "2026", "08", "bills", []OdooOutgoingInvoicePublic{
		{
			ID:          44092,
			Title:       "VENE1/2026/00089",
			TotalAmount: 504.57,
			Journal:     OdooInvoiceJournal{ID: 37, Name: "CHB Suppliers"},
			LineItems: []OdooInvoiceLineItem{
				{ProductName: "Relieve furniture rental May 2026", DisplayType: "product"},
			},
		},
	})

	p := newOdooInvoiceProcessor()
	ctx := newProcessorContext(dataDir, "2026", "08")
	if err := p.WarmUp(ctx); err != nil {
		t.Fatalf("WarmUp: %v", err)
	}

	t.Run("structured communication", func(t *testing.T) {
		tx := &TransactionEntry{Metadata: map[string]interface{}{"description": "000004430472"}}
		if err := p.ProcessTransaction(ctx, tx); err != nil {
			t.Fatal(err)
		}
		if got := stringMetadata(tx.Metadata, "odooDocTitle"); got != "+++000/0044/30472+++" {
			t.Errorf("odooDocTitle = %q", got)
		}
		full := stringMetadata(tx.Metadata, "fullDescription")
		if !contains(full, "Coworking Day Pass") {
			t.Errorf("fullDescription = %q, want it to carry the product name", full)
		}
		if contains(full, "free-text note") {
			t.Errorf("fullDescription = %q, want line_note rows excluded", full)
		}
		if contains(full, "RENTAL-COWORKING-CATERING") {
			t.Errorf("fullDescription = %q, want the journal name excluded", full)
		}
	})

	t.Run("document title in prose", func(t *testing.T) {
		tx := &TransactionEntry{Metadata: map[string]interface{}{
			"description": "CHB-S/2026/08/0004 - VENE1/2026/00089",
		}}
		if err := p.ProcessTransaction(ctx, tx); err != nil {
			t.Fatal(err)
		}
		if got := stringMetadata(tx.Metadata, "odooDocType"); got != "bill" {
			t.Errorf("odooDocType = %q, want bill", got)
		}
		if !contains(stringMetadata(tx.Metadata, "fullDescription"), "furniture rental") {
			t.Errorf("fullDescription missing the bill's product line")
		}
	})

	// Mirrors matchMoveRule: only the first line item is evidence, so a room
	// booking that also bills coffee cannot be claimed by a *coffee* rule.
	t.Run("only the first product line reaches the matcher", func(t *testing.T) {
		writeTestInvoices(t, dataDir, "2026", "07", "invoices", []OdooOutgoingInvoicePublic{{
			ID:          43466,
			Title:       "S00193",
			TotalAmount: 269.23,
			LineItems: []OdooInvoiceLineItem{
				{ProductName: "Mush Room", DisplayType: "product"},
				{ProductName: "Coffee, tea, water", DisplayType: "product"},
			},
		}})
		p2 := newOdooInvoiceProcessor()
		if err := p2.WarmUp(ctx); err != nil {
			t.Fatal(err)
		}
		tx := &TransactionEntry{Metadata: map[string]interface{}{"description": "S00193 - Devis ECOFIRST"}}
		if err := p2.ProcessTransaction(ctx, tx); err != nil {
			t.Fatal(err)
		}
		full := stringMetadata(tx.Metadata, "fullDescription")
		if !contains(full, "Mush Room") {
			t.Errorf("fullDescription = %q, want the first product line", full)
		}
		if contains(full, "Coffee") {
			t.Errorf("fullDescription = %q, want later product lines excluded", full)
		}
		if got := stringMetadata(tx.Metadata, "odooProducts"); !contains(got, "Coffee") {
			t.Errorf("odooProducts = %q, want the full list kept for humans", got)
		}
	})

	t.Run("unmatched transaction is left alone", func(t *testing.T) {
		tx := &TransactionEntry{Metadata: map[string]interface{}{
			"description": "CP-06A5E796-996D-4A89-90FF-099D806003DE",
		}}
		if err := p.ProcessTransaction(ctx, tx); err != nil {
			t.Fatal(err)
		}
		if _, ok := tx.Metadata["odooDocId"]; ok {
			t.Errorf("unmatched tx picked up odooDocId: %+v", tx.Metadata)
		}
	})
}

func writeTestInvoices(t *testing.T, dataDir, year, month, kind string, docs []OdooOutgoingInvoicePublic) {
	t.Helper()
	name := odoosource.InvoicesFile
	payload := map[string]interface{}{"invoices": docs}
	if kind == "bills" {
		name = odoosource.BillsFile
		payload = map[string]interface{}{"bills": docs}
	}
	path := odoosource.Path(dataDir, year, month, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (haystack == needle ||
		len(needle) == 0 || indexOf(haystack, needle) >= 0)
}

func indexOf(h, n string) int {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return i
		}
	}
	return -1
}
