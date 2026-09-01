package cmd

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestDecimalBlockToHex(t *testing.T) {
	got, err := decimalBlockToHex("40216446")
	if err != nil {
		t.Fatalf("decimalBlockToHex: %v", err)
	}
	if got != "0x265a77e" {
		t.Fatalf("got %q, want 0x265a77e", got)
	}
	if _, err := decimalBlockToHex("  40216446  "); err != nil {
		t.Fatalf("padded block should parse: %v", err)
	}
	if _, err := decimalBlockToHex("Max rate limit reached"); err == nil {
		t.Fatal("expected an error for a non-numeric block")
	}
}

// An on-chain reading only means something for an account that is actually a
// token wallet — asking for one elsewhere must say so rather than silently
// reporting nothing.
func TestOnchainBalanceAtRejectsNonTokenAccounts(t *testing.T) {
	cutoff := time.Date(2026, 5, 23, 23, 59, 59, 0, BrusselsTZ())

	for _, acc := range []*AccountConfig{
		nil,
		{Slug: "stripe", Provider: "stripe"},
		{Slug: "no-token", Provider: "etherscan", Address: "0xabc"},
		{Slug: "no-address", Provider: "etherscan"},
	} {
		if _, err := onchainBalanceAt(acc, cutoff); err == nil {
			t.Fatalf("expected an error for %v", acc)
		}
	}
}

func TestFetchTokenBalanceFromRPCAtBlockSendsBlockTag(t *testing.T) {
	var params []interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload struct {
			Method string        `json:"method"`
			Params []interface{} `json:"params"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if payload.Method != "eth_call" {
			t.Errorf("method = %q, want eth_call", payload.Method)
		}
		params = payload.Params
		// 1234.5 tokens at 18 decimals.
		w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":"0x000000000000000000000000000000000000000000000042ec210956b3ba0000"}`))
	}))
	defer srv.Close()

	got, err := fetchTokenBalanceFromRPCAtBlock(srv.URL, "0xtoken", "0x1234567890123456789012345678901234567890", "0x265a77e", 18)
	if err != nil {
		t.Fatalf("fetchTokenBalanceFromRPCAtBlock: %v", err)
	}
	if got != 1234.5 {
		t.Fatalf("balance = %v, want 1234.5", got)
	}
	if len(params) != 2 {
		t.Fatalf("params = %v, want [call, blockTag]", params)
	}
	if params[1] != "0x265a77e" {
		t.Fatalf("block tag = %v, want 0x265a77e", params[1])
	}
	call, ok := params[0].(map[string]interface{})
	if !ok {
		t.Fatalf("first param is %T, want the call object", params[0])
	}
	data, _ := call["data"].(string)
	if !strings.HasPrefix(data, "0x70a08231") {
		t.Fatalf("calldata %q is not balanceOf", data)
	}
	if !strings.HasSuffix(strings.ToLower(data), "1234567890123456789012345678901234567890") {
		t.Fatalf("calldata %q does not carry the wallet address", data)
	}
}

// An empty result is a zero balance, not a failure — a drained wallet must not
// drop out of the comparison.
func TestFetchTokenBalanceFromRPCAtBlockTreatsEmptyResultAsZero(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":"0x"}`))
	}))
	defer srv.Close()

	got, err := fetchTokenBalanceFromRPCAtBlock(srv.URL, "0xtoken", "0x1234567890123456789012345678901234567890", "latest", 18)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 0 {
		t.Fatalf("balance = %v, want 0", got)
	}
}

// fetchTokenBalanceFromRPC keeps its old meaning: the current balance.
func TestFetchTokenBalanceFromRPCDefaultsToLatest(t *testing.T) {
	var blockTag interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload struct {
			Params []interface{} `json:"params"`
		}
		_ = json.Unmarshal(body, &payload)
		if len(payload.Params) == 2 {
			blockTag = payload.Params[1]
		}
		w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":"0x0"}`))
	}))
	defer srv.Close()

	if _, err := fetchTokenBalanceFromRPC(srv.URL, "0xtoken", "0x1234567890123456789012345678901234567890", 18); err != nil {
		t.Fatalf("fetchTokenBalanceFromRPC: %v", err)
	}
	if blockTag != "latest" {
		t.Fatalf("block tag = %v, want latest", blockTag)
	}
}
