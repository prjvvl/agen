// Order desk: an embedded Agen agent with a custom Go tool.
//
//	go run .          scripted model, no API key needed
//	go run . --live   real model via OpenRouter ($OPENROUTER_API_KEY)
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"

	"github.com/prjvvl/agen/sdks/go/agen"
)

var orders = map[string]map[string]string{
	"A-1001": {"status": "shipped", "carrier": "UPS", "date": "2026-09-25"},
}

func main() {
	live := len(os.Args) > 1 && os.Args[1] == "--live"
	provider := agen.OpenRouter("")
	if !live {
		provider = agen.Fake(
			map[string]any{"toolCalls": []any{map[string]any{"name": "lookup_order", "arguments": map[string]any{"order_id": "A-1001"}}}},
			map[string]any{"expect": "shipped", "text": "Order A-1001 shipped on 2026-09-25 via UPS."},
		)
	}
	lookup := agen.Tool{
		Name:        "lookup_order",
		Description: "Look up an order by id. Returns its status, carrier and ship date.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"order_id":{"type":"string"}},"required":["order_id"]}`),
		ReadOnly:    true,
		Func: func(ctx context.Context, args json.RawMessage) (string, error) {
			var in struct {
				OrderID string `json:"order_id"`
			}
			if err := json.Unmarshal(args, &in); err != nil {
				return "", err
			}
			o, ok := orders[in.OrderID]
			if !ok {
				return fmt.Sprintf(`{"error":"no order %s"}`, in.OrderID), nil
			}
			b, err := json.Marshal(o)
			return string(b), err
		},
	}
	agent, err := agen.New(agen.Spec{
		Name:         "order-desk",
		Instructions: "You answer order questions. Always use lookup_order, then answer in one sentence.",
		Model:        "deepseek/deepseek-v4-flash",
		Provider:     provider,
		MaxOutput:    400,
	}, agen.WithTool(lookup))
	if err != nil {
		log.Fatal(err)
	}
	defer agent.Close()

	res, err := agent.Run(context.Background(), "Where is my order A-1001?", agen.OnDelta(func(s string) { fmt.Print(s) }))
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("\n\nstatus=%s tokens=%d+%d\ntrace:\n", res.Status, res.Usage.InputTokens, res.Usage.OutputTokens)
	spans, err := agent.Trace(res.TraceID)
	if err != nil {
		log.Fatal(err)
	}
	for _, s := range spans {
		fmt.Printf("  %-16s %5d ms  %s\n", s.Name, s.EndMs-s.StartMs, s.Status)
	}
	if res.Status != "succeeded" {
		os.Exit(1)
	}
}
