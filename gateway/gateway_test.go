package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lee00jx/pi-go/core"
	"github.com/lee00jx/pi-go/provider"
	"github.com/lee00jx/pi-go/session"
)

// newTestGateway builds a gateway over an in-memory store with a faux
// runner whose first LLM call is slowed down so tests can hit the running
// window (for steering).
func newTestGateway(t *testing.T, firstCallDelay time.Duration, turns ...provider.FauxTurn) (*Gateway, *httptest.Server, *[]*core.Request) {
	t.Helper()
	store := session.NewMemoryStore()
	meta := session.SessionMeta{ID: "s1", UserID: "u", Provider: "faux", Model: "m", Status: session.StatusIdle, CreatedAt: time.Now()}
	if err := store.CreateSession(context.Background(), meta); err != nil {
		t.Fatal(err)
	}
	faux := provider.NewFaux(core.Model{Provider: "faux", ID: "m"}, turns...)
	var mu sync.Mutex
	var reqs []*core.Request
	fn := func(ctx context.Context, req core.Request) (<-chan core.StreamEvent, error) {
		mu.Lock()
		first := len(reqs) == 0
		r := req
		reqs = append(reqs, &r)
		mu.Unlock()
		if first && firstCallDelay > 0 {
			select {
			case <-time.After(firstCallDelay):
			case <-ctx.Done():
			}
		}
		return faux.Stream(ctx, req)
	}
	runner := &core.Runner{
		Store:    store,
		StreamFn: fn,
		Model:    core.Model{Provider: "faux", ID: "m"},
		MaxTurns: 8,
	}
	gw := New(store, runner, WithAnonymousUser("u"))
	srv := httptest.NewServer(gw)
	t.Cleanup(srv.Close)
	return gw, srv, &reqs
}

func postJSON(t *testing.T, url string, body string) (int, map[string]any) {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// waitMessages polls the messages endpoint until it returns n messages.
func waitMessages(t *testing.T, url string, n int) []map[string]any {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil {
			var msgs []map[string]any
			_ = json.NewDecoder(resp.Body).Decode(&msgs)
			resp.Body.Close()
			if len(msgs) == n {
				return msgs
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d messages at %s", n, url)
	return nil
}

func textOf(msg map[string]any) string {
	var b strings.Builder
	blocks, _ := msg["content"].([]any)
	for _, blk := range blocks {
		m, _ := blk.(map[string]any)
		if m != nil && m["type"] == "text" {
			if s, ok := m["text"].(string); ok {
				b.WriteString(s)
			}
		}
	}
	return b.String()
}

// TestSteeringMidRun: a prompt sent while the session is running must be
// accepted (202 queued) and injected at the next turn boundary — visible
// both in the persisted history and in the working context of the second
// LLM call (DESIGN §2.5).
func TestSteeringMidRun(t *testing.T) {
	_, srv, reqs := newTestGateway(t, 300*time.Millisecond,
		provider.FauxTurn{Text: "turn1"},
		provider.FauxTurn{Text: "turn2"},
	)
	base := srv.URL + "/api/agent/sessions/s1"

	code, body := postJSON(t, base+"/prompts", `{"text":"first"}`)
	if code != http.StatusAccepted || body["status"] != "started" {
		t.Fatalf("first prompt: %d %+v, want 202 started", code, body)
	}

	// Wait inside the running window (first LLM call is slowed to 300ms).
	time.Sleep(60 * time.Millisecond)
	code, body = postJSON(t, base+"/prompts", `{"text":"steer"}`)
	if code != http.StatusAccepted || body["status"] != "queued" {
		t.Fatalf("steering prompt: %d %+v, want 202 queued", code, body)
	}

	// user first / assistant turn1 / user steer / assistant turn2
	msgs := waitMessages(t, base+"/messages", 4)
	want := []struct {
		role string
		text string
	}{{"user", "first"}, {"assistant", "turn1"}, {"user", "steer"}, {"assistant", "turn2"}}
	for i, w := range want {
		if msgs[i]["role"] != w.role || textOf(msgs[i]) != w.text {
			t.Fatalf("msg[%d] = %+v, want %s %q", i, msgs[i], w.role, w.text)
		}
	}

	// The second LLM call must have seen the steering message in context.
	var got bool
	for _, r := range *reqs {
		for _, m := range r.Messages {
			if m.Role == core.RoleUser && m.Text() == "steer" {
				got = true
			}
		}
	}
	if !got {
		t.Fatal("steering message was never fed to the LLM")
	}
}

// TestMessagesEndpoint: the first-screen endpoint returns only message
// entries (never ui_event), seq-tagged, in append order.
func TestMessagesEndpoint(t *testing.T) {
	_, srv, _ := newTestGateway(t, 0,
		provider.FauxTurn{
			ToolCalls: []provider.FauxToolCall{{ID: "c1", Name: "nope", Args: json.RawMessage(`{}`)}},
		},
		provider.FauxTurn{Text: "final"},
	)
	base := srv.URL + "/api/agent/sessions/s1"

	code, _ := postJSON(t, base+"/prompts", `{"text":"hi"}`)
	if code != http.StatusAccepted {
		t.Fatalf("prompt: %d", code)
	}
	// user / assistant(toolcall) / tool_result / assistant = 4 messages;
	// the run also appends many ui_event entries that must be filtered out.
	msgs := waitMessages(t, base+"/messages", 4)
	roles := make([]string, 0, 4)
	var lastSeq int64
	for i, m := range msgs {
		roles = append(roles, m["role"].(string))
		seq, _ := m["seq"].(float64)
		if int64(seq) <= lastSeq {
			t.Fatalf("msg[%d] seq %v not increasing (last %d)", i, m["seq"], lastSeq)
		}
		lastSeq = int64(seq)
	}
	if strings.Join(roles, ",") != "user,assistant,tool,assistant" {
		t.Fatalf("roles = %v", roles)
	}
}

// TestPromptValidation: unknown session → 404, empty text → 400.
func TestPromptValidation(t *testing.T) {
	_, srv, _ := newTestGateway(t, 0, provider.FauxTurn{Text: "x"})
	code, _ := postJSON(t, srv.URL+"/api/agent/sessions/nope/prompts", `{"text":"hi"}`)
	if code != http.StatusNotFound {
		t.Fatalf("unknown session: %d, want 404", code)
	}
	code, _ = postJSON(t, srv.URL+"/api/agent/sessions/s1/prompts", `{"text":"  "}`)
	if code != http.StatusBadRequest {
		t.Fatalf("empty text: %d, want 400", code)
	}
}

func newBareGateway(t *testing.T, opts ...Option) *httptest.Server {
	t.Helper()
	store := session.NewMemoryStore()
	runner := &core.Runner{
		Store: store,
		StreamFn: func(context.Context, core.Request) (<-chan core.StreamEvent, error) {
			ch := make(chan core.StreamEvent)
			close(ch)
			return ch, nil
		},
		Model:    core.Model{Provider: "faux", ID: "m"},
		MaxTurns: 1,
	}
	srv := httptest.NewServer(New(store, runner, append([]Option{WithAnonymousUser("local")}, opts...)...))
	t.Cleanup(srv.Close)
	return srv
}

func TestWithBasePathRegistersRoutes(t *testing.T) {
	srv := newBareGateway(t, WithBasePath("/api/v1/agent/"))
	code, body := postJSON(t, srv.URL+"/api/v1/agent/sessions", `{}`)
	if code != http.StatusCreated {
		t.Fatalf("custom prefix create: %d %#v, want 201", code, body)
	}
	if id, _ := body["id"].(string); id == "" {
		t.Fatalf("create body = %#v, want id", body)
	}
	if code, _ := postJSON(t, srv.URL+"/api/agent/sessions", `{}`); code != http.StatusNotFound {
		t.Fatalf("default prefix: %d, want 404 after WithBasePath", code)
	}
}

func TestWithBasePathAndAuthOptionOrder(t *testing.T) {
	const token = "secret"
	auth := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("X-Token") != token {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
	// Auth is listed first on purpose: it used to wrap a nil/default mux
	// before WithBasePath could take effect.
	srv := newBareGateway(t, WithAuthMiddleware(auth), WithBasePath("/api/v1/agent"))

	code, _ := postJSON(t, srv.URL+"/api/v1/agent/sessions", `{}`)
	if code != http.StatusUnauthorized {
		t.Fatalf("missing token: %d, want 401", code)
	}

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/agent/sessions", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Token", token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("authed custom prefix: %d, want 201", resp.StatusCode)
	}
}
