package cmd

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// odoo_web.go — `chb serve`: a read-only, mobile-first web UI to review the
// latest transactions across all Odoo journals, with partner / GL-account
// drill-downs and invoice links.
//
// Design constraints:
//   - STRICTLY read-only: every handler only ever issues search_read /
//     read_group RPCs against a whitelisted set of models. There is no generic
//     proxy endpoint, so no client input can trigger a write.
//   - Multi-database: the client picks a DB per request (?db=…) from a
//     server-side allowlist (ODOO_SERVE_DBS, default commonshub +
//     commonshub-test). Credentials are the operator's ODOO_LOGIN/PASSWORD
//     from config.env; the URL is derived per Odoo SaaS convention.
//   - Network exposure is delegated to Tailscale: the server binds to
//     localhost by default; `tailscale serve` (or a DNS record pointing at the
//     tailnet IP) makes it reachable as e.g. odoo.xavierdamman.com.

//go:embed odoo_web_app.html
var odooWebAppHTML []byte

// odooWebServer holds per-database auth state for the read-only web UI.
type odooWebServer struct {
	dbs      []string // allowlist
	login    string
	password string

	mu   sync.Mutex
	uids map[string]int // db → cached uid
}

// odooWebAllowedDBs resolves the database allowlist: ODOO_SERVE_DBS
// (comma-separated) when set, else the two known Commons Hub databases.
func odooWebAllowedDBs() []string {
	if v := strings.TrimSpace(os.Getenv("ODOO_SERVE_DBS")); v != "" {
		var dbs []string
		for _, d := range strings.Split(v, ",") {
			if d = strings.TrimSpace(d); d != "" {
				dbs = append(dbs, d)
			}
		}
		return dbs
	}
	return []string{"commonshub", "commonshub-test"}
}

// OdooWebServe is `chb serve` — start the read-only web UI.
func OdooWebServe(args []string) error {
	if HasFlag(args, "--help", "-h", "help") {
		printOdooWebServeHelp()
		return nil
	}
	addr := GetOption(args, "--addr")
	if addr == "" {
		addr = "127.0.0.1:8787"
	}

	login := os.Getenv("ODOO_LOGIN")
	password := os.Getenv("ODOO_PASSWORD")
	if login == "" || password == "" {
		return fmt.Errorf("ODOO_LOGIN/ODOO_PASSWORD not set (check APP_DATA_DIR/settings/config.env)")
	}

	srv := &odooWebServer{
		dbs:      odooWebAllowedDBs(),
		login:    login,
		password: password,
		uids:     map[string]int{},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(odooWebAppHTML)
	})
	mux.HandleFunc("/api/config", srv.handleConfig)
	mux.HandleFunc("/api/journals", srv.handleJournals)
	mux.HandleFunc("/api/transactions", srv.handleTransactions)
	mux.HandleFunc("/api/balances", srv.handleBalances)
	mux.HandleFunc("/api/transaction", srv.handleTransaction)
	mux.HandleFunc("/api/partner", srv.handlePartner)
	mux.HandleFunc("/api/account", srv.handleAccount)

	fmt.Printf("\n%schb serve%s — read-only Odoo transaction review\n", Fmt.Bold, Fmt.Reset)
	fmt.Printf("  %sDatabases: %s%s\n", Fmt.Dim, strings.Join(srv.dbs, ", "), Fmt.Reset)
	fmt.Printf("  %sListening on http://%s%s\n\n", Fmt.Green, addr, Fmt.Reset)
	fmt.Printf("  %sExpose over Tailscale (pick one):%s\n", Fmt.Dim, Fmt.Reset)
	fmt.Printf("    %stailscale serve --bg %s%s          %s→ https://<machine>.<tailnet>.ts.net%s\n", Fmt.Cyan, addr, Fmt.Reset, Fmt.Dim, Fmt.Reset)
	fmt.Printf("    %schb serve --addr <tailscale-ip>:80%s   %s→ point odoo.xavierdamman.com at the tailnet IP%s\n\n", Fmt.Cyan, Fmt.Reset, Fmt.Dim, Fmt.Reset)

	server := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	return server.ListenAndServe()
}

// authFor validates the db against the allowlist and returns cached
// credentials + uid, authenticating on first use.
func (s *odooWebServer) authFor(db string) (*OdooCredentials, int, error) {
	allowed := false
	for _, d := range s.dbs {
		if d == db {
			allowed = true
			break
		}
	}
	if !allowed {
		return nil, 0, fmt.Errorf("database %q is not in the allowlist (%s)", db, strings.Join(s.dbs, ", "))
	}
	creds := &OdooCredentials{
		URL:      odooURLFromDB(db),
		DB:       db,
		Login:    s.login,
		Password: s.password,
	}
	s.mu.Lock()
	uid, ok := s.uids[db]
	s.mu.Unlock()
	if ok && uid > 0 {
		return creds, uid, nil
	}
	uid, err := odooAuth(creds.URL, creds.DB, creds.Login, creds.Password)
	if err != nil || uid == 0 {
		return nil, 0, fmt.Errorf("odoo auth failed for %s: %v", db, err)
	}
	s.mu.Lock()
	s.uids[db] = uid
	s.mu.Unlock()
	return creds, uid, nil
}

func webJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(v)
}

func webError(w http.ResponseWriter, code int, err error) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

// webSearchRead is a single windowed read-only search_read (no pagination
// loop, no progress line — this runs inside HTTP handlers).
func webSearchRead(creds *OdooCredentials, uid int, model string, domain []interface{}, fields []string, order string, limit, offset int) ([]map[string]interface{}, error) {
	kwargs := map[string]interface{}{"fields": fields}
	if order != "" {
		kwargs["order"] = order
	}
	if limit > 0 {
		kwargs["limit"] = limit
	}
	if offset > 0 {
		kwargs["offset"] = offset
	}
	result, err := odooExec(creds.URL, creds.DB, uid, creds.Password,
		model, "search_read", []interface{}{domain}, kwargs)
	if err != nil {
		return nil, err
	}
	var rows []map[string]interface{}
	if err := json.Unmarshal(result, &rows); err != nil {
		return nil, err
	}
	return rows, nil
}

// webReadGroupSums returns (sum of `field`, record count) for a domain via a
// single read_group RPC.
func webReadGroupSums(creds *OdooCredentials, uid int, model string, domain []interface{}, field string) (float64, int, error) {
	result, err := odooExec(creds.URL, creds.DB, uid, creds.Password,
		model, "read_group",
		[]interface{}{domain, []string{field}, []string{}},
		map[string]interface{}{"lazy": false})
	if err != nil {
		return 0, 0, err
	}
	var groups []map[string]interface{}
	if err := json.Unmarshal(result, &groups); err != nil {
		return 0, 0, err
	}
	if len(groups) == 0 {
		return 0, 0, nil
	}
	sum := odooFloat(groups[0][field])
	count := odooInt(groups[0]["__count"])
	return sum, count, nil
}

// ---- API payload types ----

type webTx struct {
	ID          int     `json:"id"`
	Date        string  `json:"date"`
	Amount      float64 `json:"amount"`
	Ref         string  `json:"ref"`
	PartnerID   int     `json:"partnerId,omitempty"`
	PartnerName string  `json:"partnerName,omitempty"`
	AccountCode string  `json:"accountCode,omitempty"`
	AccountName string  `json:"accountName,omitempty"`
	JournalID   int     `json:"journalId"`
	JournalName string  `json:"journalName,omitempty"`
	Reconciled  bool    `json:"reconciled"`
	MatchedName string  `json:"matchedName,omitempty"`
	MatchedURL  string  `json:"matchedUrl,omitempty"`
	OdooURL     string  `json:"odooUrl,omitempty"`
	SourceURL   string  `json:"sourceUrl,omitempty"`
	SourceLabel string  `json:"sourceLabel,omitempty"`
}

// txSourceLink derives the upstream-source link from a unique_import_id:
// on-chain transfers (chain:address:txhash:logIndex) → the block explorer,
// Stripe lines (stripe:acct:id[:fee]) → the Stripe dashboard.
func txSourceLink(importID string) (url, label string) {
	parts := strings.Split(importID, ":")
	if len(parts) < 3 {
		return "", ""
	}
	id := parts[2]
	switch parts[0] {
	case "stripe":
		switch {
		case strings.HasPrefix(id, "ch_"), strings.HasPrefix(id, "py_"), strings.HasPrefix(id, "pi_"):
			return "https://dashboard.stripe.com/payments/" + id, "Stripe payment"
		case strings.HasPrefix(id, "po_"):
			return "https://dashboard.stripe.com/payouts/" + id, "Stripe payout"
		case id != "":
			// txn_… balance transactions have no page of their own —
			// dashboard search resolves them to their source object.
			return "https://dashboard.stripe.com/search?query=" + id, "Stripe dashboard"
		}
	case "gnosis":
		if strings.HasPrefix(id, "0x") {
			return "https://gnosisscan.io/tx/" + id, "Gnosisscan"
		}
	case "polygon":
		if strings.HasPrefix(id, "0x") {
			return "https://polygonscan.com/tx/" + id, "Polygonscan"
		}
	case "ethereum", "mainnet":
		if strings.HasPrefix(id, "0x") {
			return "https://etherscan.io/tx/" + id, "Etherscan"
		}
	}
	return "", ""
}

func (s *odooWebServer) handleConfig(w http.ResponseWriter, r *http.Request) {
	webJSON(w, map[string]interface{}{"databases": s.dbs})
}

func (s *odooWebServer) handleJournals(w http.ResponseWriter, r *http.Request) {
	creds, uid, err := s.authFor(r.URL.Query().Get("db"))
	if err != nil {
		webError(w, 400, err)
		return
	}
	rows, err := webSearchRead(creds, uid, "account.journal",
		[]interface{}{[]interface{}{"type", "in", []interface{}{"bank", "cash"}}},
		[]string{"id", "name"}, "id asc", 0, 0)
	if err != nil {
		webError(w, 502, err)
		return
	}
	type journal struct {
		ID   int    `json:"id"`
		Name string `json:"name"`
	}
	journals := make([]journal, 0, len(rows))
	for _, row := range rows {
		journals = append(journals, journal{ID: odooInt(row["id"]), Name: odooString(row["name"])})
	}
	webJSON(w, map[string]interface{}{"journals": journals})
}

// parseIDList parses "44,48,56" → []int.
func parseIDList(s string) []int {
	var out []int
	for _, part := range strings.Split(s, ",") {
		if n, err := strconv.Atoi(strings.TrimSpace(part)); err == nil && n > 0 {
			out = append(out, n)
		}
	}
	return out
}

func (s *odooWebServer) handleTransactions(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	creds, uid, err := s.authFor(q.Get("db"))
	if err != nil {
		webError(w, 400, err)
		return
	}
	limit := 30
	if n, err := strconv.Atoi(q.Get("limit")); err == nil && n > 0 && n <= 200 {
		limit = n
	}
	offset := 0
	if n, err := strconv.Atoi(q.Get("offset")); err == nil && n > 0 {
		offset = n
	}
	domain := []interface{}{}
	if ids := parseIDList(q.Get("journals")); len(ids) > 0 {
		domain = append(domain, []interface{}{"journal_id", "in", intsToInterfaces(ids)})
	}
	txs, err := s.fetchStatementLines(creds, uid, domain, limit, offset)
	if err != nil {
		webError(w, 502, err)
		return
	}
	webJSON(w, map[string]interface{}{"transactions": txs})
}

// fetchStatementLines reads a page of bank statement lines and enriches each
// with its counterpart GL account and, when reconciled, the matched
// invoice/bill — all via batched read-only RPCs (4 per page).
func (s *odooWebServer) fetchStatementLines(creds *OdooCredentials, uid int, domain []interface{}, limit, offset int) ([]webTx, error) {
	rows, err := webSearchRead(creds, uid, "account.bank.statement.line", domain,
		[]string{"id", "date", "amount", "payment_ref", "partner_id", "move_id", "journal_id", "is_reconciled", "unique_import_id", "narration"},
		"date desc, id desc", limit, offset)
	if err != nil {
		return nil, err
	}

	txs := make([]webTx, 0, len(rows))
	moveIDs := make([]int, 0, len(rows))
	txByMove := map[int]int{} // move id → index into txs
	for _, row := range rows {
		tx := webTx{
			ID:          odooInt(row["id"]),
			Date:        odooString(row["date"]),
			Amount:      odooFloat(row["amount"]),
			Ref:         odooString(row["payment_ref"]),
			PartnerID:   odooFieldID(row["partner_id"]),
			PartnerName: odooFieldName(row["partner_id"]),
			JournalID:   odooFieldID(row["journal_id"]),
			JournalName: odooFieldName(row["journal_id"]),
			Reconciled:  odooBool(row["is_reconciled"]),
		}
		tx.SourceURL, tx.SourceLabel = txSourceLink(odooString(row["unique_import_id"]))
		// The import id lowercases Stripe ids, but Stripe ids are
		// case-sensitive — the narration metadata carries the original-case
		// charge / balance-transaction id, so prefer that for the link.
		if strings.HasPrefix(odooString(row["unique_import_id"]), "stripe:") {
			if meta := parseOdooLineNarration(odooString(row["narration"])); meta != nil {
				ch := metaString(meta, "charge")
				if ch == "" {
					ch = metaString(meta, "chargeId") // fee-line narration key
				}
				if strings.HasPrefix(ch, "ch_") || strings.HasPrefix(ch, "py_") || strings.HasPrefix(ch, "pi_") {
					tx.SourceURL, tx.SourceLabel = "https://dashboard.stripe.com/payments/"+ch, "Stripe payment"
				} else if bt := metaString(meta, "balanceTransaction"); strings.HasPrefix(bt, "txn_") {
					tx.SourceURL, tx.SourceLabel = "https://dashboard.stripe.com/search?query="+bt, "Stripe dashboard"
				} else if strings.HasPrefix(ch, "txn_") {
					tx.SourceURL, tx.SourceLabel = "https://dashboard.stripe.com/search?query="+ch, "Stripe dashboard"
				}
			}
		}
		if mid := odooFieldID(row["move_id"]); mid > 0 {
			tx.OdooURL = OdooWebURL(creds.URL, "account.move", mid)
			txByMove[mid] = len(txs)
			moveIDs = append(moveIDs, mid)
		}
		txs = append(txs, tx)
	}
	if len(moveIDs) == 0 {
		return txs, nil
	}

	// Counterpart GL account per move (skip the bank/cash leg), plus the A/R /
	// A/P legs' partial-reconcile ids for the invoice-link resolution.
	mlines, err := webSearchRead(creds, uid, "account.move.line",
		[]interface{}{[]interface{}{"move_id", "in", intsToInterfaces(moveIDs)}},
		[]string{"move_id", "account_id", "account_type", "matched_debit_ids", "matched_credit_ids"}, "", 0, 0)
	if err != nil {
		return txs, nil // enrichment is best-effort
	}
	armLineByPartial := map[int]int{} // partial id → stmt move id
	selfMoves := map[int]bool{}
	for _, ml := range mlines {
		mid := odooFieldID(ml["move_id"])
		idx, ok := txByMove[mid]
		if !ok {
			continue
		}
		selfMoves[mid] = true
		accountType := odooString(ml["account_type"])
		switch accountType {
		case "asset_cash", "liability_credit_card":
			continue
		case "asset_receivable", "liability_payable":
			for _, key := range []string{"matched_debit_ids", "matched_credit_ids"} {
				if arr, ok := ml[key].([]interface{}); ok {
					for _, v := range arr {
						if pid := odooInt(v); pid > 0 {
							armLineByPartial[pid] = mid
						}
					}
				}
			}
		}
		if txs[idx].AccountName == "" {
			name := odooFieldName(ml["account_id"])
			txs[idx].AccountName = name
			if fields := strings.Fields(name); len(fields) > 0 {
				if _, err := strconv.Atoi(fields[0]); err == nil {
					txs[idx].AccountCode = fields[0]
					txs[idx].AccountName = strings.TrimSpace(strings.TrimPrefix(name, fields[0]))
				}
			}
		}
	}

	// Resolve matched invoices/bills: partial reconcile → counterpart move.
	if len(armLineByPartial) > 0 {
		partialIDs := make([]int, 0, len(armLineByPartial))
		for pid := range armLineByPartial {
			partialIDs = append(partialIDs, pid)
		}
		partials, err := webSearchRead(creds, uid, "account.partial.reconcile",
			[]interface{}{[]interface{}{"id", "in", intsToInterfaces(partialIDs)}},
			[]string{"id", "debit_move_id", "credit_move_id"}, "", 0, 0)
		if err == nil {
			cpLineIDs := make([]int, 0, len(partials))
			cpLineToStmtMove := map[int]int{}
			for _, p := range partials {
				stmtMove := armLineByPartial[odooInt(p["id"])]
				for _, k := range []string{"debit_move_id", "credit_move_id"} {
					if lid := odooFieldID(p[k]); lid > 0 {
						cpLineIDs = append(cpLineIDs, lid)
						if _, dup := cpLineToStmtMove[lid]; !dup {
							cpLineToStmtMove[lid] = stmtMove
						}
					}
				}
			}
			cpLines, err := webSearchRead(creds, uid, "account.move.line",
				[]interface{}{[]interface{}{"id", "in", intsToInterfaces(cpLineIDs)}},
				[]string{"id", "move_id"}, "", 0, 0)
			if err == nil {
				for _, cl := range cpLines {
					mid := odooFieldID(cl["move_id"])
					stmtMove := cpLineToStmtMove[odooInt(cl["id"])]
					if mid == 0 || selfMoves[mid] || stmtMove == 0 {
						continue
					}
					if idx, ok := txByMove[stmtMove]; ok && txs[idx].MatchedName == "" {
						txs[idx].MatchedName = odooFieldName(cl["move_id"])
						txs[idx].MatchedURL = OdooWebURL(creds.URL, "account.move", mid)
					}
				}
			}
		}
	}
	return txs, nil
}

// handleBalances returns the current balance per selected journal (sum of its
// statement-line amounts — the same figure `chb odoo journals` reports) plus
// the aggregate, in a single read_group RPC.
func (s *odooWebServer) handleBalances(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	creds, uid, err := s.authFor(q.Get("db"))
	if err != nil {
		webError(w, 400, err)
		return
	}
	ids := parseIDList(q.Get("journals"))
	if len(ids) == 0 {
		webJSON(w, map[string]interface{}{"total": 0, "journals": []interface{}{}})
		return
	}
	result, err := odooExec(creds.URL, creds.DB, uid, creds.Password,
		"account.bank.statement.line", "read_group",
		[]interface{}{
			[]interface{}{[]interface{}{"journal_id", "in", intsToInterfaces(ids)}},
			[]string{"amount"},
			[]string{"journal_id"},
		},
		map[string]interface{}{"lazy": false})
	if err != nil {
		webError(w, 502, err)
		return
	}
	var groups []map[string]interface{}
	if err := json.Unmarshal(result, &groups); err != nil {
		webError(w, 502, err)
		return
	}
	type journalBalance struct {
		JournalID   int     `json:"journalId"`
		JournalName string  `json:"journalName"`
		Balance     float64 `json:"balance"`
	}
	total := 0.0
	balances := make([]journalBalance, 0, len(groups))
	for _, g := range groups {
		b := journalBalance{
			JournalID:   odooFieldID(g["journal_id"]),
			JournalName: odooFieldName(g["journal_id"]),
			Balance:     odooFloat(g["amount"]),
		}
		total += b.Balance
		balances = append(balances, b)
	}
	sort.Slice(balances, func(i, j int) bool { return balances[i].JournalID < balances[j].JournalID })
	resp := map[string]interface{}{"total": total, "journals": balances}
	if ts := lastJournalSyncTime(creds.DB, ids); !ts.IsZero() {
		resp["lastSync"] = ts.UTC().Format(time.RFC3339)
	}
	webJSON(w, resp)
}

// lastJournalSyncTime returns the most recent moment chb synced any of the
// given journals of a database, read from the per-journal push cursors. The
// key is built explicitly from db (rather than SyncCursorKeyForOdooJournal,
// which reads the process-wide namespace) because the server serves several
// databases from one process.
func lastJournalSyncTime(db string, journalIDs []int) time.Time {
	var newest time.Time
	for _, id := range journalIDs {
		key := "odoo." + safeCursorKeyPart(db) + ".journal." + strconv.Itoa(id)
		if cur := LoadSyncCursor(key); cur.UpdatedAt.After(newest) {
			newest = cur.UpdatedAt
		}
	}
	return newest
}

// handleTransaction returns one enriched statement line — the payload behind
// the shareable transaction page (#/tx/<id>).
func (s *odooWebServer) handleTransaction(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	creds, uid, err := s.authFor(q.Get("db"))
	if err != nil {
		webError(w, 400, err)
		return
	}
	id, _ := strconv.Atoi(q.Get("id"))
	if id <= 0 {
		webError(w, 400, fmt.Errorf("missing ?id="))
		return
	}
	txs, err := s.fetchStatementLines(creds, uid,
		[]interface{}{[]interface{}{"id", "=", id}}, 1, 0)
	if err != nil {
		webError(w, 502, err)
		return
	}
	if len(txs) == 0 {
		webError(w, 404, fmt.Errorf("transaction #%d not found", id))
		return
	}
	tx := txs[0]
	resp := map[string]interface{}{"transaction": tx}
	// A line with no linked document that isn't settled yet gets reconcile
	// suggestions: open invoices/bills ranked by amount / partner / ref match.
	if tx.MatchedName == "" && !tx.Reconciled {
		resp["candidates"] = s.reconcileCandidates(creds, uid, tx)
		resp["reconcileUrl"] = OdooBankReconciliationURL(creds.URL, tx.JournalID)
	}
	webJSON(w, resp)
}

// webCandidate is one reconcile suggestion on the transaction page.
type webCandidate struct {
	ID       int      `json:"id"`
	Name     string   `json:"name"`
	Ref      string   `json:"ref,omitempty"`
	Type     string   `json:"type"` // "invoice" | "bill"
	Partner  string   `json:"partner,omitempty"`
	Date     string   `json:"date"`
	Total    float64  `json:"total"`
	Residual float64  `json:"residual"`
	Reasons  []string `json:"reasons"`
	OdooURL  string   `json:"odooUrl"`
	score    int
}

// reconcileCandidates suggests open (posted, not fully paid) invoices/bills
// for an unmatched statement line, scored by exact amount, partner and
// ref-substring signals — the same heuristics the CLI reconcile matcher uses,
// but live against the selected database and read-only. Money in → customer
// invoices; money out → vendor bills.
func (s *odooWebServer) reconcileCandidates(creds *OdooCredentials, uid int, tx webTx) []webCandidate {
	moveTypes := []interface{}{"out_invoice", "out_receipt"}
	kind := "invoice"
	if tx.Amount < 0 {
		moveTypes = []interface{}{"in_invoice", "in_receipt"}
		kind = "bill"
	}
	rows, err := webSearchRead(creds, uid, "account.move",
		[]interface{}{
			[]interface{}{"state", "=", "posted"},
			[]interface{}{"payment_state", "in", []interface{}{"not_paid", "partial"}},
			[]interface{}{"move_type", "in", moveTypes},
		},
		[]string{"id", "name", "ref", "partner_id", "invoice_date", "amount_total", "amount_residual"},
		"invoice_date desc, id desc", 300, 0)
	if err != nil {
		return nil
	}
	want := tx.Amount
	if want < 0 {
		want = -want
	}
	refLower := strings.ToLower(tx.Ref)
	var out []webCandidate
	for _, row := range rows {
		c := webCandidate{
			ID:       odooInt(row["id"]),
			Name:     odooString(row["name"]),
			Ref:      odooString(row["ref"]),
			Type:     kind,
			Partner:  odooFieldName(row["partner_id"]),
			Date:     odooString(row["invoice_date"]),
			Total:    odooFloat(row["amount_total"]),
			Residual: odooFloat(row["amount_residual"]),
			OdooURL:  OdooWebURL(creds.URL, "account.move", odooInt(row["id"])),
		}
		if diff := c.Residual - want; diff < 0.01 && diff > -0.01 {
			c.score += 100
			c.Reasons = append(c.Reasons, "same amount")
		} else if diff := c.Total - want; diff < 0.01 && diff > -0.01 {
			c.score += 80
			c.Reasons = append(c.Reasons, "same total")
		}
		if tx.PartnerID > 0 && c.Partner != "" && odooFieldID(row["partner_id"]) == tx.PartnerID {
			c.score += 50
			c.Reasons = append(c.Reasons, "same partner")
		}
		// Ref substring both ways: the bank memo often carries the invoice
		// number (or the invoice ref carries the payment's structured ref).
		for _, needle := range []string{strings.ToLower(c.Name), strings.ToLower(c.Ref)} {
			if len(needle) >= 5 && refLower != "" && (strings.Contains(refLower, needle) || strings.Contains(needle, refLower)) {
				c.score += 70
				c.Reasons = append(c.Reasons, "ref match")
				break
			}
		}
		if c.score > 0 {
			out = append(out, c)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].score > out[j].score })
	if len(out) > 10 {
		out = out[:10]
	}
	// No signal at all: fall back to the few open documents closest in amount,
	// so the page still gives the operator something to compare against.
	if len(out) == 0 {
		sort.SliceStable(rows, func(i, j int) bool {
			di := odooFloat(rows[i]["amount_residual"]) - want
			dj := odooFloat(rows[j]["amount_residual"]) - want
			if di < 0 {
				di = -di
			}
			if dj < 0 {
				dj = -dj
			}
			return di < dj
		})
		for _, row := range rows {
			out = append(out, webCandidate{
				ID:       odooInt(row["id"]),
				Name:     odooString(row["name"]),
				Ref:      odooString(row["ref"]),
				Type:     kind,
				Partner:  odooFieldName(row["partner_id"]),
				Date:     odooString(row["invoice_date"]),
				Total:    odooFloat(row["amount_total"]),
				Residual: odooFloat(row["amount_residual"]),
				Reasons:  []string{"closest amount"},
				OdooURL:  OdooWebURL(creds.URL, "account.move", odooInt(row["id"])),
			})
			if len(out) == 5 {
				break
			}
		}
	}
	return out
}

func (s *odooWebServer) handlePartner(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	creds, uid, err := s.authFor(q.Get("db"))
	if err != nil {
		webError(w, 400, err)
		return
	}
	partnerID, _ := strconv.Atoi(q.Get("id"))
	if partnerID <= 0 {
		webError(w, 400, fmt.Errorf("missing ?id="))
		return
	}
	prows, err := webSearchRead(creds, uid, "res.partner",
		[]interface{}{[]interface{}{"id", "=", partnerID}},
		[]string{"id", "name", "email"}, "", 1, 0)
	if err != nil || len(prows) == 0 {
		webError(w, 404, fmt.Errorf("partner #%d not found", partnerID))
		return
	}

	base := []interface{}{[]interface{}{"partner_id", "=", partnerID}}
	if ids := parseIDList(q.Get("journals")); len(ids) > 0 {
		base = append(base, []interface{}{"journal_id", "in", intsToInterfaces(ids)})
	}
	inSum, inCount, _ := webReadGroupSums(creds, uid, "account.bank.statement.line",
		append(append([]interface{}{}, base...), []interface{}{"amount", ">", 0}), "amount")
	outSum, outCount, _ := webReadGroupSums(creds, uid, "account.bank.statement.line",
		append(append([]interface{}{}, base...), []interface{}{"amount", "<", 0}), "amount")

	txs, err := s.fetchStatementLines(creds, uid, base, 30, 0)
	if err != nil {
		webError(w, 502, err)
		return
	}
	webJSON(w, map[string]interface{}{
		"partner": map[string]interface{}{
			"id":      partnerID,
			"name":    odooString(prows[0]["name"]),
			"email":   odooString(prows[0]["email"]),
			"odooUrl": OdooWebURL(creds.URL, "res.partner", partnerID),
		},
		"totals": map[string]interface{}{
			"count": inCount + outCount,
			"in":    inSum,
			"out":   outSum,
		},
		"transactions": txs,
	})
}

func (s *odooWebServer) handleAccount(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	creds, uid, err := s.authFor(q.Get("db"))
	if err != nil {
		webError(w, 400, err)
		return
	}
	code := strings.TrimSpace(q.Get("code"))
	if code == "" {
		webError(w, 400, fmt.Errorf("missing ?code="))
		return
	}
	arows, err := webSearchRead(creds, uid, "account.account",
		[]interface{}{[]interface{}{"code", "=", code}},
		[]string{"id", "code", "name"}, "", 1, 0)
	if err != nil || len(arows) == 0 {
		webError(w, 404, fmt.Errorf("account %s not found", code))
		return
	}
	accountID := odooInt(arows[0]["id"])

	domain := []interface{}{
		[]interface{}{"account_id", "=", accountID},
		[]interface{}{"parent_state", "=", "posted"},
	}
	balance, count, _ := webReadGroupSums(creds, uid, "account.move.line", domain, "balance")
	debit, _, _ := webReadGroupSums(creds, uid, "account.move.line", domain, "debit")
	credit, _, _ := webReadGroupSums(creds, uid, "account.move.line", domain, "credit")

	lines, err := webSearchRead(creds, uid, "account.move.line", domain,
		[]string{"id", "date", "name", "move_id", "partner_id", "balance", "journal_id"},
		"date desc, id desc", 30, 0)
	if err != nil {
		webError(w, 502, err)
		return
	}
	type accountLine struct {
		ID          int     `json:"id"`
		Date        string  `json:"date"`
		Name        string  `json:"name"`
		MoveName    string  `json:"moveName"`
		MoveURL     string  `json:"moveUrl"`
		PartnerID   int     `json:"partnerId,omitempty"`
		PartnerName string  `json:"partnerName,omitempty"`
		Amount      float64 `json:"amount"`
		JournalName string  `json:"journalName,omitempty"`
	}
	out := make([]accountLine, 0, len(lines))
	for _, l := range lines {
		out = append(out, accountLine{
			ID:          odooInt(l["id"]),
			Date:        odooString(l["date"]),
			Name:        odooString(l["name"]),
			MoveName:    odooFieldName(l["move_id"]),
			MoveURL:     OdooWebURL(creds.URL, "account.move", odooFieldID(l["move_id"])),
			PartnerID:   odooFieldID(l["partner_id"]),
			PartnerName: odooFieldName(l["partner_id"]),
			Amount:      odooFloat(l["balance"]),
			JournalName: odooFieldName(l["journal_id"]),
		})
	}
	webJSON(w, map[string]interface{}{
		"account": map[string]interface{}{
			"id":      accountID,
			"code":    odooString(arows[0]["code"]),
			"name":    odooString(arows[0]["name"]),
			"odooUrl": OdooWebURL(creds.URL, "account.account", accountID),
		},
		"totals": map[string]interface{}{
			"count":   count,
			"balance": balance,
			"debit":   debit,
			"credit":  credit,
		},
		"lines": out,
	})
}

func printOdooWebServeHelp() {
	f := Fmt
	fmt.Printf(`
%schb serve%s — Read-only web UI to review the latest transactions across all
Odoo journals from your phone. Strictly read-only: the server only issues
search_read / read_group calls, so nothing in the UI can modify Odoo.

Features: latest transactions across selected journals, per-account filter
with shortcuts, partner pages (total txs, volume in/out, latest txs), GL
account pages (summary + latest entries), links to the matched invoice/bill
and to every record in Odoo. Settings (database + journal selection) persist
in the browser's localStorage; a banner warns when the selected database has
"test" in its name.

%sUSAGE%s
  %schb serve%s                       Listen on 127.0.0.1:8787
  %schb serve --addr 0.0.0.0:8080%s   Custom bind address

%sEXPOSING VIA TAILSCALE%s
  %stailscale serve --bg 127.0.0.1:8787%s
      → https://<machine>.<tailnet>.ts.net (Tailscale-only, TLS included)
  Or bind to the machine's Tailscale IP and point a DNS record
  (odoo.xavierdamman.com → 100.x.y.z) at it — only tailnet members can reach it.

%sOPTIONS%s
  %s--addr host:port%s     Bind address (default 127.0.0.1:8787)

%sENVIRONMENT%s
  %sODOO_LOGIN / ODOO_PASSWORD%s   Odoo credentials (from settings/config.env)
  %sODOO_SERVE_DBS%s               Comma-separated DB allowlist
                               (default: commonshub,commonshub-test)
`,
		f.Bold, f.Reset,
		f.Bold, f.Reset,
		f.Cyan, f.Reset,
		f.Cyan, f.Reset,
		f.Bold, f.Reset,
		f.Cyan, f.Reset,
		f.Bold, f.Reset,
		f.Yellow, f.Reset,
		f.Bold, f.Reset,
		f.Yellow, f.Reset,
		f.Yellow, f.Reset,
	)
}
