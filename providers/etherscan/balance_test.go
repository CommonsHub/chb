package etherscan

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// stubAPI points the package at a test server for the duration of a test.
func stubAPI(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	ResetQuotaState()
	srv := httptest.NewServer(handler)
	old := APIBaseURL
	APIBaseURL = srv.URL
	t.Cleanup(func() {
		APIBaseURL = old
		srv.Close()
		ResetQuotaState()
	})
	return srv
}

func TestBlockNumberBeforeSendsClosestBefore(t *testing.T) {
	var gotQuery map[string]string
	stubAPI(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = map[string]string{}
		for k, v := range r.URL.Query() {
			gotQuery[k] = v[0]
		}
		w.Write([]byte(`{"status":"1","message":"OK","result":"40216446"}`))
	})

	block, err := BlockNumberBefore(100, 1748044799, "key")
	if err != nil {
		t.Fatalf("BlockNumberBefore: %v", err)
	}
	if block != "40216446" {
		t.Fatalf("block = %q, want 40216446", block)
	}
	for k, want := range map[string]string{
		"chainid":   "100",
		"module":    "block",
		"action":    "getblocknobytime",
		"timestamp": "1748044799",
		"closest":   "before",
		"apikey":    "key",
	} {
		if gotQuery[k] != want {
			t.Errorf("query %s = %q, want %q", k, gotQuery[k], want)
		}
	}
}

func TestTokenBalanceReadsLatest(t *testing.T) {
	var action, tag string
	stubAPI(t, func(w http.ResponseWriter, r *http.Request) {
		action = r.URL.Query().Get("action")
		tag = r.URL.Query().Get("tag")
		w.Write([]byte(`{"status":"1","message":"OK","result":"17817530000000000000000"}`))
	})

	raw, err := TokenBalance(100, "0xtoken", "0xwallet", "key")
	if err != nil {
		t.Fatalf("TokenBalance: %v", err)
	}
	if raw != "17817530000000000000000" {
		t.Fatalf("raw = %q", raw)
	}
	if action != "tokenbalance" || tag != "latest" {
		t.Fatalf("action=%q tag=%q", action, tag)
	}
}

// A free key gets a refusal, not a failure: the caller needs to tell it apart
// from a real error so it can fall back to an archive node.
func TestTokenBalanceAtBlockReportsProEndpoint(t *testing.T) {
	stubAPI(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"0","message":"NOTOK","result":"Sorry, it looks like you are trying to access an API Pro endpoint. Contact us to upgrade to API Pro."}`))
	})

	_, err := TokenBalanceAtBlock(100, "0xtoken", "0xwallet", "40216446", "key")
	if err == nil {
		t.Fatal("expected an error for a Pro-only endpoint")
	}
	if !errors.Is(err, ErrProEndpoint) {
		t.Fatalf("error %v does not wrap ErrProEndpoint", err)
	}
}

func TestTokenBalanceAtBlockSendsBlockNumber(t *testing.T) {
	var action, blockno string
	stubAPI(t, func(w http.ResponseWriter, r *http.Request) {
		action = r.URL.Query().Get("action")
		blockno = r.URL.Query().Get("blockno")
		w.Write([]byte(`{"status":"1","message":"OK","result":"42"}`))
	})

	raw, err := TokenBalanceAtBlock(100, "0xtoken", "0xwallet", "40216446", "key")
	if err != nil {
		t.Fatalf("TokenBalanceAtBlock: %v", err)
	}
	if raw != "42" {
		t.Fatalf("raw = %q, want 42", raw)
	}
	if action != "tokenbalancehistory" || blockno != "40216446" {
		t.Fatalf("action=%q blockno=%q", action, blockno)
	}
}

// The daily cap is process-wide, so a balance call must not burn its retry
// budget against a wall that cannot lift until the reset time.
func TestApiGetShortCircuitsOnExhaustedQuota(t *testing.T) {
	calls := 0
	stubAPI(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Write([]byte(`{"status":"0","message":"NOTOK","result":"Community Free API Limit reached. Resets 2026-09-02 00:00:00 UTC"}`))
	})

	if _, err := TokenBalance(100, "0xtoken", "0xwallet", "key"); err == nil {
		t.Fatal("expected a quota error")
	}
	if calls != 1 {
		t.Fatalf("made %d calls for the first quota error, want 1", calls)
	}
	if _, err := BlockNumberBefore(100, 1748044799, "key"); err == nil {
		t.Fatal("expected the second call to fail fast")
	}
	if calls != 1 {
		t.Fatalf("made %d calls total, want the second one short-circuited", calls)
	}
}

func TestIsProEndpointNotice(t *testing.T) {
	if !isProEndpointNotice("Sorry, it looks like you are trying to access an API Pro endpoint.") {
		t.Fatal("Pro notice not recognised")
	}
	if isProEndpointNotice("Max rate limit reached") {
		t.Fatal("rate limit misread as a Pro notice")
	}
}
