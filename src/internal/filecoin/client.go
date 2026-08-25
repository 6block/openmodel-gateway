package filecoin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"time"
)

// Client is a minimal Lotus JSON-RPC client over plain HTTP with ordered-endpoint
// failover. It only implements the read-only State methods SP admission needs.
//
// Numeric reads (power, balance) additionally cross-check endpoints on a zero
// result: some public gateways intermittently return well-formed "empty" answers
// (observed ~30% on glif mainnet), and a fake zero must not reject a legitimate
// miner when another endpoint knows the real value. A zero is only accepted when
// every reachable endpoint agrees.
type Client struct {
	endpoints []string
	hc        *http.Client
	logger    *slog.Logger
}

func NewClient(endpoints []string, logger *slog.Logger) *Client {
	if logger == nil {
		logger = slog.Default()
	}
	return &Client{
		endpoints: endpoints,
		hc:        &http.Client{Timeout: 15 * time.Second},
		logger:    logger,
	}
}

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id"`
	Method  string `json:"method"`
	Params  []any  `json:"params"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
}

// callOne performs one JSON-RPC call against one endpoint.
func (c *Client) callOne(ctx context.Context, endpoint, method string, params []any, out any) error {
	body, err := json.Marshal(rpcRequest{JSONRPC: "2.0", ID: 1, Method: method, Params: params})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("http %d: %.200s", resp.StatusCode, data)
	}
	var rr rpcResponse
	if err := json.Unmarshal(data, &rr); err != nil {
		return fmt.Errorf("bad json-rpc response: %w", err)
	}
	if rr.Error != nil {
		return fmt.Errorf("rpc error %d: %s", rr.Error.Code, rr.Error.Message)
	}
	if len(rr.Result) == 0 || string(rr.Result) == "null" {
		return fmt.Errorf("empty result")
	}
	return json.Unmarshal(rr.Result, out)
}

// call tries each endpoint in order and returns the first successful decode.
func (c *Client) call(ctx context.Context, method string, params []any, out any) error {
	var lastErr error
	for _, ep := range c.endpoints {
		if err := c.callOne(ctx, ep, method, params, out); err != nil {
			lastErr = err
			c.logger.Warn("filecoin rpc call failed; trying next endpoint",
				"endpoint", ep, "method", method, "error", err)
			continue
		}
		return nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no endpoints configured")
	}
	return fmt.Errorf("all filecoin rpc endpoints failed for %s: %w", method, lastErr)
}

// MinerInfo returns the miner's owner and worker as ID addresses (f0/t0 form).
func (c *Client) MinerInfo(ctx context.Context, miner string) (ownerID, workerID string, err error) {
	var out struct {
		Owner  string `json:"Owner"`
		Worker string `json:"Worker"`
	}
	if err := c.call(ctx, "Filecoin.StateMinerInfo", []any{miner, nil}, &out); err != nil {
		return "", "", err
	}
	if out.Owner == "" || out.Worker == "" {
		return "", "", fmt.Errorf("miner %s: empty owner/worker in StateMinerInfo", miner)
	}
	return out.Owner, out.Worker, nil
}

// AccountKey resolves an ID address (f0…) to its public-key address (f1/f3).
func (c *Client) AccountKey(ctx context.Context, idAddr string) (string, error) {
	var out string
	if err := c.call(ctx, "Filecoin.StateAccountKey", []any{idAddr, nil}, &out); err != nil {
		return "", err
	}
	if out == "" {
		return "", fmt.Errorf("empty key address for %s", idAddr)
	}
	return out, nil
}

// MinerRawPower returns the miner's raw byte power. Zero is only reported when
// every reachable endpoint agrees (fake-empty cross-check).
func (c *Client) MinerRawPower(ctx context.Context, miner string) (*big.Int, error) {
	read := func(ctx context.Context, ep string) (*big.Int, error) {
		var out struct {
			MinerPower struct {
				RawBytePower string `json:"RawBytePower"`
			} `json:"MinerPower"`
		}
		if err := c.callOne(ctx, ep, "Filecoin.StateMinerPower", []any{miner, nil}, &out); err != nil {
			return nil, err
		}
		v, ok := new(big.Int).SetString(out.MinerPower.RawBytePower, 10)
		if !ok {
			return nil, fmt.Errorf("bad RawBytePower %q", out.MinerPower.RawBytePower)
		}
		return v, nil
	}
	return c.crossCheckedRead(ctx, "Filecoin.StateMinerPower", read)
}

// ActorBalance returns an actor's balance in attoFIL. Zero is only reported when
// every reachable endpoint agrees (fake-empty cross-check).
func (c *Client) ActorBalance(ctx context.Context, addr string) (*big.Int, error) {
	read := func(ctx context.Context, ep string) (*big.Int, error) {
		var out struct {
			Balance string `json:"Balance"`
		}
		if err := c.callOne(ctx, ep, "Filecoin.StateGetActor", []any{addr, nil}, &out); err != nil {
			return nil, err
		}
		v, ok := new(big.Int).SetString(out.Balance, 10)
		if !ok {
			return nil, fmt.Errorf("bad Balance %q", out.Balance)
		}
		return v, nil
	}
	return c.crossCheckedRead(ctx, "Filecoin.StateGetActor", read)
}

// crossCheckedRead walks the endpoints, returning the first POSITIVE value. A zero
// answer is remembered but the next endpoint is still consulted, so a single
// endpoint's fake-empty response cannot masquerade as "miner has nothing". Zero is
// returned only when all reachable endpoints said zero.
func (c *Client) crossCheckedRead(ctx context.Context, method string, read func(context.Context, string) (*big.Int, error)) (*big.Int, error) {
	var sawZero bool
	var lastErr error
	for _, ep := range c.endpoints {
		v, err := read(ctx, ep)
		if err != nil {
			lastErr = err
			c.logger.Warn("filecoin rpc read failed; trying next endpoint",
				"endpoint", ep, "method", method, "error", err)
			continue
		}
		if v.Sign() > 0 {
			return v, nil
		}
		sawZero = true
		c.logger.Warn("filecoin rpc returned zero; cross-checking next endpoint",
			"endpoint", ep, "method", method)
	}
	if sawZero {
		return new(big.Int), nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no endpoints configured")
	}
	return nil, fmt.Errorf("all filecoin rpc endpoints failed for %s: %w", method, lastErr)
}
