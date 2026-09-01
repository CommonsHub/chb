package etherscan

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// ErrProEndpoint marks a call that the API accepted but refused to answer
// because the endpoint is only served to API Pro keys. Callers use it to fall
// back to another source rather than reporting a hard failure.
var ErrProEndpoint = errors.New("etherscan: API Pro endpoint")

// BlockNumberBefore resolves a wall-clock timestamp to the last block mined at
// or before it. Free-tier endpoint.
func BlockNumberBefore(chainID int, timestamp int64, apiKey string) (string, error) {
	params := url.Values{}
	params.Set("module", "block")
	params.Set("action", "getblocknobytime")
	params.Set("timestamp", fmt.Sprintf("%d", timestamp))
	params.Set("closest", "before")

	raw, err := apiGet(chainID, params, apiKey)
	if err != nil {
		return "", err
	}
	block := resultString(raw)
	if block == "" {
		return "", fmt.Errorf("etherscan: empty block for timestamp %d on chain %d", timestamp, chainID)
	}
	return block, nil
}

// TokenBalance returns the wallet's current raw ERC20 balance, in the token's
// smallest unit. Free-tier endpoint.
func TokenBalance(chainID int, tokenAddress, walletAddress, apiKey string) (string, error) {
	params := url.Values{}
	params.Set("module", "account")
	params.Set("action", "tokenbalance")
	params.Set("contractaddress", tokenAddress)
	params.Set("address", walletAddress)
	params.Set("tag", "latest")

	raw, err := apiGet(chainID, params, apiKey)
	if err != nil {
		return "", err
	}
	return resultString(raw), nil
}

// TokenBalanceAtBlock returns the wallet's raw ERC20 balance as of a past
// block. This is an API Pro endpoint: a free key gets ErrProEndpoint back, and
// the caller is expected to fall back to an archive-node eth_call.
func TokenBalanceAtBlock(chainID int, tokenAddress, walletAddress, blockNumber, apiKey string) (string, error) {
	params := url.Values{}
	params.Set("module", "account")
	params.Set("action", "tokenbalancehistory")
	params.Set("contractaddress", tokenAddress)
	params.Set("address", walletAddress)
	params.Set("blockno", blockNumber)

	raw, err := apiGet(chainID, params, apiKey)
	if err != nil {
		return "", err
	}
	return resultString(raw), nil
}

// apiGet performs one Etherscan V2 call through the package's shared throttle
// and retry policy, returning the decoded `result` field. It exists so the
// balance calls get the same rate-limit handling, daily-quota short circuit and
// error classification as the transfer sync.
func apiGet(chainID int, params url.Values, apiKey string) (json.RawMessage, error) {
	if notice, blocked := QuotaExhausted(); blocked {
		return nil, fmt.Errorf("etherscan daily free-tier quota exhausted (chain=%d action=%s): %s",
			chainID, params.Get("action"), notice)
	}

	params.Set("chainid", fmt.Sprintf("%d", chainID))
	params.Set("apikey", apiKey)
	requestURL := APIBaseURL + "?" + params.Encode()
	ctx := fmt.Sprintf("chain=%d action=%s url=%s", chainID, params.Get("action"), redactAPIKey(requestURL, apiKey))

	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			time.Sleep(retryDelay(attempt))
		}

		throttle()
		resp, err := http.Get(requestURL)
		if err != nil {
			lastErr = fmt.Errorf("etherscan request failed (%s): %w", ctx, err)
			continue
		}

		body, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			lastErr = fmt.Errorf("etherscan: reading response body (HTTP %d, %s): %w", resp.StatusCode, ctx, readErr)
			continue
		}

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			lastErr = fmt.Errorf("etherscan HTTP %d (%s): %s", resp.StatusCode, ctx, bodySnippet(body))
			if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
				continue
			}
			return nil, lastErr
		}

		var result struct {
			Status  string          `json:"status"`
			Message string          `json:"message"`
			Result  json.RawMessage `json:"result"`
		}
		if err := json.Unmarshal(body, &result); err != nil {
			lastErr = fmt.Errorf("etherscan: decoding response (HTTP %d, %s): %v — body: %s",
				resp.StatusCode, ctx, err, bodySnippet(body))
			continue
		}

		if result.Status != "1" {
			detail := resultString(result.Result)
			apiErr := fmt.Errorf("etherscan API error: status=%s message=%q result=%q (%s)",
				result.Status, result.Message, detail, ctx)
			if isProEndpointNotice(detail) {
				return nil, fmt.Errorf("%w: %v", ErrProEndpoint, apiErr)
			}
			switch classifyError(result.Message, detail) {
			case errQuota:
				noteQuotaExhausted(detail)
				return nil, fmt.Errorf("etherscan daily free-tier quota exhausted — retrying will not help until it resets: %w", apiErr)
			case errTransient:
				lastErr = fmt.Errorf("transient etherscan error: %w", apiErr)
				continue
			default:
				return nil, apiErr
			}
		}

		return result.Result, nil
	}

	return nil, fmt.Errorf("etherscan: failed after %d attempts: %w", maxAttempts, lastErr)
}
