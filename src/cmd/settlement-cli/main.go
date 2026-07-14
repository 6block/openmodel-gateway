package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

const defaultAdminURL = "http://localhost:9091"

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	adminURL := os.Getenv("ADMIN_URL")
	if adminURL == "" {
		adminURL = defaultAdminURL
	}
	adminToken := os.Getenv("AGENT_ADMIN_TOKEN")

	cmd := os.Args[1]
	args := os.Args[2:]

	var err error
	switch cmd {
	case "balance":
		err = cmdBalance(adminURL, adminToken, args)
	case "balances":
		err = cmdBalances(adminURL, adminToken)
	case "earnings":
		err = cmdEarnings(adminURL, adminToken, args)
	case "revenue":
		err = cmdRevenue(adminURL, adminToken)
	case "settlements":
		err = cmdSettlements(adminURL, adminToken)
	case "verify":
		err = cmdVerify(adminURL, adminToken, args)
	case "settle-now":
		err = cmdSettleNow(adminURL, adminToken)
	case "operator-balance":
		err = cmdOperatorBalance(adminURL, adminToken)
	case "fil-price":
		err = cmdFILPrice(adminURL, adminToken, args)
	case "revenue-report":
		err = cmdRevenueReport(adminURL, adminToken)
	case "sp-detail":
		err = cmdSPDetail(adminURL, adminToken, args)
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", cmd)
		printUsage()
		os.Exit(1)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Println(`settlement-cli — OpenModel settlement management tool

Usage: settlement-cli <command> [args]

Commands:
  balance <address>         Query user balance (on-chain + pending spend)
  balances                  List all user balances
  earnings <sp_address>     Query SP earnings
  revenue                   All SP revenue summary
  settlements               List settlement batches
  verify <batch_id>         Reconcile an on-chain batch vs the local audit log
  settle-now                Trigger immediate settlement
  operator-balance          Operator wallet gas balance
  fil-price [set <price>]   Query or set FIL/USD price
  revenue-report            Formatted revenue report
  sp-detail <sp> [since_unix] [limit]
                            Per-request earnings for one SP (each inference request:
                            earning + settled/pending + on-chain tx)

Environment:
  ADMIN_URL          Admin API URL (default: http://localhost:9091)
  AGENT_ADMIN_TOKEN  Admin API bearer token`)
}

func cmdBalance(adminURL, token string, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: settlement-cli balance <address>")
	}
	resp, err := apiGet(adminURL, "/api/v1/balances/"+args[0], token)
	if err != nil {
		return err
	}
	printJSON(resp)
	return nil
}

func cmdBalances(adminURL, token string) error {
	resp, err := apiGet(adminURL, "/api/v1/balances", token)
	if err != nil {
		return err
	}
	printJSON(resp)
	return nil
}

func cmdEarnings(adminURL, token string, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: settlement-cli earnings <sp_address>")
	}
	resp, err := apiGet(adminURL, "/api/v1/revenue/"+args[0], token)
	if err != nil {
		return err
	}
	printJSON(resp)
	return nil
}

func cmdRevenue(adminURL, token string) error {
	resp, err := apiGet(adminURL, "/api/v1/revenue", token)
	if err != nil {
		return err
	}
	printJSON(resp)
	return nil
}

// cmdSPDetail queries per-request earnings for one SP. Optional positional args:
// args[1]=since_unix, args[2]=limit. Mapped to ?since=&limit= query params.
func cmdSPDetail(adminURL, token string, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: settlement-cli sp-detail <sp> [since_unix] [limit]")
	}
	path := "/api/v1/sp-earnings-detail/" + args[0]
	q := ""
	if len(args) >= 2 && args[1] != "" {
		q = "since=" + args[1]
	}
	if len(args) >= 3 && args[2] != "" {
		if q != "" {
			q += "&"
		}
		q += "limit=" + args[2]
	}
	if q != "" {
		path += "?" + q
	}
	resp, err := apiGet(adminURL, path, token)
	if err != nil {
		return err
	}
	printJSON(resp)
	return nil
}

func cmdSettlements(adminURL, token string) error {
	resp, err := apiGet(adminURL, "/api/v1/settlements", token)
	if err != nil {
		return err
	}
	printJSON(resp)
	return nil
}

func cmdVerify(adminURL, token string, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: settlement-cli verify <batch_id>")
	}
	resp, err := apiGet(adminURL, "/api/v1/settlements/"+args[0], token)
	if err != nil {
		return err
	}
	printJSON(resp)
	fmt.Println()
	fmt.Println(settlementVerdict(resp))
	return nil
}

// settlementVerdict turns a /settlements/:id response into a human reconciliation
// verdict comparing on-chain state with the local audit log.
func settlementVerdict(resp map[string]interface{}) string {
	processed := false
	if onChain, ok := resp["on_chain"].(map[string]interface{}); ok {
		processed, _ = onChain["processed"].(bool)
	}
	localFound := false
	hasLocal := false
	if la, ok := resp["local_audit"].(map[string]interface{}); ok {
		hasLocal = true
		localFound, _ = la["found"].(bool)
	}

	switch {
	case processed && localFound:
		return "VERDICT: OK — on-chain record matches the local audit log"
	case processed && hasLocal && !localFound:
		return "VERDICT: WARN — on-chain shows processed, but NO local audit record (audit-log gap)"
	case processed && !hasLocal:
		return "VERDICT: OK (on-chain) — batch processed on-chain (no local audit log available to cross-check)"
	case !processed && localFound:
		return "VERDICT: WARN — local audit record exists, but batch is NOT marked processed on-chain"
	default:
		return "VERDICT: WARN — batch not processed on-chain and no local audit record"
	}
}

func cmdSettleNow(adminURL, token string) error {
	resp, err := apiPost(adminURL, "/api/v1/settle-now", token, nil)
	if err != nil {
		return err
	}
	printJSON(resp)
	return nil
}

func cmdOperatorBalance(adminURL, token string) error {
	resp, err := apiGet(adminURL, "/api/v1/operator-balance", token)
	if err != nil {
		return err
	}
	printJSON(resp)
	return nil
}

func cmdFILPrice(adminURL, token string, args []string) error {
	if len(args) >= 2 && args[0] == "set" {
		body := fmt.Sprintf(`{"fil_price_usd":"%s"}`, args[1])
		resp, err := apiPut(adminURL, "/api/v1/fil-price", token, []byte(body))
		if err != nil {
			return err
		}
		printJSON(resp)
		return nil
	}
	resp, err := apiGet(adminURL, "/api/v1/fil-price", token)
	if err != nil {
		return err
	}
	printJSON(resp)
	return nil
}

func cmdRevenueReport(adminURL, token string) error {
	fmt.Println("=== OpenModel Revenue Report ===")
	fmt.Printf("Generated: %s\n\n", time.Now().Format(time.RFC3339))

	// FIL Price
	priceResp, err := apiGet(adminURL, "/api/v1/fil-price", token)
	if err == nil {
		if p, ok := priceResp["fil_price_usd"].(string); ok {
			fmt.Printf("FIL/USD: $%s\n", p)
		}
	}

	// Operator Balance
	opResp, err := apiGet(adminURL, "/api/v1/operator-balance", token)
	if err == nil {
		if b, ok := opResp["balance"].(string); ok {
			fmt.Printf("Operator Gas Balance: %s\n", b)
		}
	}
	fmt.Println()

	// SP Revenue
	fmt.Println("--- SP Revenue ---")
	revResp, err := apiGet(adminURL, "/api/v1/revenue", token)
	if err == nil {
		if providers, ok := revResp["providers"].([]interface{}); ok {
			for _, p := range providers {
				sp, ok := p.(map[string]interface{})
				if !ok {
					continue
				}
				fmt.Printf("  SP: %s (%s)\n", sp["miner_address"], sp["evm_address"])
				if earnings, ok := sp["earnings"].(map[string]interface{}); ok {
					for token, amount := range earnings {
						fmt.Printf("    %s: %s\n", token, amount)
					}
				}
			}
		}
	}
	fmt.Println()

	// User Balances
	fmt.Println("--- User Balances ---")
	balResp, err := apiGet(adminURL, "/api/v1/balances", token)
	if err == nil {
		if users, ok := balResp["users"].([]interface{}); ok {
			for _, u := range users {
				user, ok := u.(map[string]interface{})
				if !ok {
					continue
				}
				fmt.Printf("  %s (pending: $%s USD)\n", user["wallet"], user["pending_spend_usd"])
				if bals, ok := user["balances"].(map[string]interface{}); ok {
					for token, amount := range bals {
						fmt.Printf("    %s: %s\n", token, amount)
					}
				}
			}
		}
	}
	fmt.Println()

	// Settlement stats
	fmt.Println("--- Settlements ---")
	settResp, err := apiGet(adminURL, "/api/v1/settlements", token)
	if err == nil {
		if total, ok := settResp["total_batches"].(float64); ok {
			fmt.Printf("  Total batches: %d\n", int(total))
		}
	}

	return nil
}

// --- HTTP helpers ---

func apiGet(baseURL, path, token string) (map[string]interface{}, error) {
	return apiRequest("GET", baseURL+path, token, nil)
}

func apiPost(baseURL, path, token string, body []byte) (map[string]interface{}, error) {
	return apiRequest("POST", baseURL+path, token, body)
}

func apiPut(baseURL, path, token string, body []byte) (map[string]interface{}, error) {
	return apiRequest("PUT", baseURL+path, token, body)
}

func apiRequest(method, url, token string, body []byte) (map[string]interface{}, error) {
	var bodyReader io.Reader
	if body != nil {
		bodyReader = strings.NewReader(string(body))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, method, url, bodyReader)
	if err != nil {
		return nil, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	var result map[string]interface{}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}
	return result, nil
}

func printJSON(data map[string]interface{}) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	enc.Encode(data)
}
