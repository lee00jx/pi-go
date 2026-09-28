package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/lee00jx/pi-go/core"
	"github.com/lee00jx/pi-go/provider"
	"github.com/lee00jx/pi-go/session"
)

func getJSON(t *testing.T, url string) (int, map[string]any) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// waitFor polls url until pred(body) is true, failing after the deadline.
func waitFor(t *testing.T, url string, pred func(map[string]any) bool) map[string]any {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var last map[string]any
	for time.Now().Before(deadline) {
		if code, body := getJSON(t, url); code == http.StatusOK {
			if pred(body) {
				return body
			}
			last = body
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting at %s; last=%v", url, last)
	return nil
}

// TestUsageEndpoint: per-run usage + cost is folded into the session header
// at run end and served by GET /usage (DESIGN §7.6).
func TestUsageEndpoint(t *testing.T) {
	gw, srv, _ := newTestGateway(t, 0,
		provider.FauxTurn{Text: "hi", Usage: &core.Usage{InputTokens: 1000, OutputTokens: 500, TotalTokens: 1500}},
	)
	gw.runner.CostPerMillion = func(core.Model) (in, out float64) { return 3.0, 1.5 }
	base := srv.URL + "/api/agent/sessions/s1"

	if code, _ := postJSON(t, base+"/prompts", `{"text":"hello"}`); code != http.StatusAccepted {
		t.Fatalf("prompt: %d", code)
	}

	body := waitFor(t, base+"/usage", func(b map[string]any) bool {
		v, _ := b["tokensIn"].(float64)
		return v == 1000
	})
	if v, _ := body["tokensOut"].(float64); v != 500 {
		t.Fatalf("tokensOut = %v, want 500", body["tokensOut"])
	}
	want := 1000.0/1e6*3.0 + 500.0/1e6*1.5
	if c, _ := body["cost"].(float64); c < want-1e-9 || c > want+1e-9 {
		t.Fatalf("cost = %v, want ~%v", body["cost"], want)
	}
	if body["model"] != "m" {
		t.Fatalf("model = %v, want m", body["model"])
	}
}

// TestUsageEndpointNotFound: unknown session → 404.
func TestUsageEndpointNotFound(t *testing.T) {
	_, srv, _ := newTestGateway(t, 0, provider.FauxTurn{Text: "x"})
	if code, _ := getJSON(t, srv.URL+"/api/agent/sessions/nope/usage"); code != http.StatusNotFound {
		t.Fatalf("unknown session usage: %d, want 404", code)
	}
}

// TestContextStatsEndpoint: occupancy reflects the derived context, the model
// window, and a consistent percentage (DESIGN §8).
func TestContextStatsEndpoint(t *testing.T) {
	gw, srv, _ := newTestGateway(t, 0, provider.FauxTurn{Text: "hi"})
	gw.runner.Model.ContextWindow = 1000
	base := srv.URL + "/api/agent/sessions/s1"

	if code, _ := postJSON(t, base+"/prompts", `{"text":"hello"}`); code != http.StatusAccepted {
		t.Fatalf("prompt: %d", code)
	}

	body := waitFor(t, base+"/context-stats", func(b map[string]any) bool {
		v, _ := b["currentTokens"].(float64)
		return v > 0
	})
	if w, _ := body["contextWindow"].(float64); w != 1000 {
		t.Fatalf("contextWindow = %v, want 1000", body["contextWindow"])
	}
	if c, _ := body["compactions"].(float64); c != 0 {
		t.Fatalf("compactions = %v, want 0", body["compactions"])
	}
	ct, _ := body["currentTokens"].(float64)
	uc, _ := body["usedPercent"].(float64)
	want := ct / 1000 * 100
	if uc < want-0.01 || uc > want+0.01 {
		t.Fatalf("usedPercent = %v, want ~%v (currentTokens=%v)", uc, want, ct)
	}
}

// TestContextStatsCompactionCount: each L2 compaction entry is counted in the
// occupancy bar's "已压缩" number.
func TestContextStatsCompactionCount(t *testing.T) {
	gw, srv, _ := newTestGateway(t, 0, provider.FauxTurn{Text: "hi"})
	gw.runner.Model.ContextWindow = 1000
	base := srv.URL + "/api/agent/sessions/s1"

	// Seed one compaction entry directly (no need to drive an L2 trigger).
	payload, _ := json.Marshal(core.CompactionEntry{Summary: "S", FirstKeptSeq: 1, TokensBefore: 10, Model: "m"})
	if _, err := gw.store.AppendEntry(context.Background(), "s1", session.EntryCompaction, payload, nil); err != nil {
		t.Fatal(err)
	}

	code, body := getJSON(t, base+"/context-stats")
	if code != http.StatusOK {
		t.Fatalf("context-stats: %d", code)
	}
	if c, _ := body["compactions"].(float64); c != 1 {
		t.Fatalf("compactions = %v, want 1", body["compactions"])
	}
}

// TestContextStatsEndpointNotFound: unknown session → 404.
func TestContextStatsEndpointNotFound(t *testing.T) {
	_, srv, _ := newTestGateway(t, 0, provider.FauxTurn{Text: "x"})
	if code, _ := getJSON(t, srv.URL+"/api/agent/sessions/nope/context-stats"); code != http.StatusNotFound {
		t.Fatalf("unknown session context-stats: %d, want 404", code)
	}
}
