package cmd

import (
	"errors"
	"fmt"
	"math/big"
	"os"
	"strings"
	"time"

	etherscansource "github.com/CommonsHub/chb/providers/etherscan"
)

// onchainReading is one balance read straight from the chain, for comparison
// against the balance chb computed from its local archive.
type onchainReading struct {
	Balance float64
	// Block is the block the reading is taken at: a decimal block number, or
	// "latest" when the cutoff is not in the past.
	Block string
	// Source names where the figure came from — "etherscan" or the RPC URL —
	// so a mismatch can be traced to the endpoint that produced it.
	Source string
	// Historical is true when the reading needed past state, i.e. when a
	// pruned node could answer with a zero it should have refused.
	Historical bool
}

// onchainBalanceAt reads an account's token balance from the chain as of the
// cutoff. A cutoff at or after now reads the current balance; an earlier one is
// resolved to the last block before the cutoff and read at that block.
//
// PriorTokens are deliberately not summed. Monerium's V2 contracts report the
// same balanceOf for a wallet as their V1 predecessors, so adding them would
// double-count — the same reasoning as refreshAccountBalance.
func onchainBalanceAt(acc *AccountConfig, cutoff time.Time) (onchainReading, error) {
	if acc == nil || acc.Provider != "etherscan" || acc.Address == "" || acc.Token == nil {
		return onchainReading{}, fmt.Errorf("--onchain needs an etherscan account with a token contract; %q is not one", accSlugOrUnknown(acc))
	}

	if !cutoff.Before(time.Now()) {
		v, err := fetchTokenBalance(acc.ChainID, acc.Token.Address, acc.Address, acc.Token.Decimals)
		if err != nil {
			return onchainReading{}, err
		}
		return onchainReading{Balance: v, Block: "latest", Source: "etherscan/rpc"}, nil
	}

	apiKey := os.Getenv("ETHERSCAN_API_KEY")
	if apiKey == "" {
		return onchainReading{}, fmt.Errorf("ETHERSCAN_API_KEY not set — needed to resolve %s to a block number",
			cutoff.In(BrusselsTZ()).Format("2006-01-02"))
	}
	block, err := etherscansource.BlockNumberBefore(acc.ChainID, cutoff.Unix(), apiKey)
	if err != nil {
		return onchainReading{}, fmt.Errorf("resolving %s to a block: %w", cutoff.In(BrusselsTZ()).Format("2006-01-02"), err)
	}

	// Etherscan's historical token balance is an API Pro endpoint. Try it
	// first — a Pro key gets the authoritative answer — and fall back to an
	// archive-node eth_call, which the public RPCs serve for recent history.
	raw, esErr := etherscansource.TokenBalanceAtBlock(acc.ChainID, acc.Token.Address, acc.Address, block, apiKey)
	if esErr == nil {
		v, err := rawTokenBalanceToFloat(raw, acc.Token.Decimals)
		if err != nil {
			return onchainReading{}, err
		}
		return onchainReading{Balance: v, Block: block, Source: "etherscan", Historical: true}, nil
	}

	rpcURL := defaultRPCForChainID(acc.ChainID)
	if rpcURL == "" {
		return onchainReading{}, fmt.Errorf("no default RPC for chain ID %d to fall back to: %w", acc.ChainID, esErr)
	}
	blockTag, err := decimalBlockToHex(block)
	if err != nil {
		return onchainReading{}, err
	}
	v, rpcErr := fetchTokenBalanceFromRPCAtBlock(rpcURL, acc.Token.Address, acc.Address, blockTag, acc.Token.Decimals)
	if rpcErr != nil {
		if errors.Is(esErr, etherscansource.ErrProEndpoint) {
			return onchainReading{}, fmt.Errorf("historical balance needs an Etherscan API Pro key or an archive node; %s refused it: %w", rpcURL, rpcErr)
		}
		return onchainReading{}, fmt.Errorf("%v; RPC fallback also failed: %w", esErr, rpcErr)
	}
	return onchainReading{Balance: v, Block: block, Source: rpcURL, Historical: true}, nil
}

// decimalBlockToHex converts Etherscan's decimal block number to the
// 0x-prefixed form JSON-RPC expects.
func decimalBlockToHex(block string) (string, error) {
	n, ok := new(big.Int).SetString(strings.TrimSpace(block), 10)
	if !ok {
		return "", fmt.Errorf("unexpected block number %q from etherscan", block)
	}
	return "0x" + n.Text(16), nil
}

func accSlugOrUnknown(acc *AccountConfig) string {
	if acc == nil || acc.Slug == "" {
		return "unknown account"
	}
	return acc.Slug
}
