package gateway

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/lee00jx/pi-go/core"
	"github.com/lee00jx/pi-go/session"
)

// A turn is one user submission (prompt_id), projected from entries at
// request time. Activity and results come only from ui_event rows; the
// user message supplies the original text. Status is not "this prompt's
// agent_end": a run emits a single agent_end stamped with the last
// prompt_id, so a mid-run steer would leave the first turn running
// forever. A turn closes when a later user message with a different
// prompt_id appears, or when its run ends (agent_end / session
// interrupted or ended).
func (g *Gateway) handleTurns(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ctx := r.Context()
	meta, err := g.sessionFor(ctx, id)
	if err != nil {
		writeSessionErr(w, err)
		return
	}
	entries, err := g.store.ListEntries(ctx, id, 0, 0)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, projectTurns(meta, entries))
}

type turnUserView struct {
	Text      string    `json:"text"`
	Seq       int64     `json:"seq"`
	CreatedAt time.Time `json:"createdAt,omitempty"`
}

type seqEvent struct {
	Seq       int64     `json:"seq"`
	CreatedAt time.Time `json:"createdAt,omitempty"`
	core.AgentEvent
}

type turnView struct {
	PromptID  string        `json:"promptId"`
	Status    string        `json:"status"`
	CreatedAt time.Time     `json:"createdAt,omitempty"` // first result, else last activity, else user
	User      *turnUserView `json:"user,omitempty"`
	Activity  []seqEvent    `json:"activity"`
	Results   []seqEvent    `json:"results"`
}

type turnsResponse struct {
	SessionID string                `json:"sessionId"`
	Agent     string                `json:"agent,omitempty"`
	Status    session.SessionStatus `json:"status"`
	Hints     []seqEvent            `json:"hints"`
	Turns     []turnView            `json:"turns"`
}

type turnAcc struct {
	promptID     string
	runIDs       map[string]struct{}
	user         *turnUserView
	activity     []seqEvent
	results      []seqEvent
	failed       bool
	closedByNext bool
}

func projectTurns(meta session.SessionMeta, entries []session.Entry) turnsResponse {
	out := turnsResponse{
		SessionID: meta.ID,
		Agent:     meta.Agent,
		Status:    meta.Status,
		Hints:     []seqEvent{},
		Turns:     []turnView{},
	}
	order := make([]string, 0)
	acc := make(map[string]*turnAcc)
	ensure := func(pid string) *turnAcc {
		if pid == "" {
			return nil
		}
		t, ok := acc[pid]
		if !ok {
			t = &turnAcc{
				promptID: pid,
				runIDs:   make(map[string]struct{}),
				activity: []seqEvent{},
				results:  []seqEvent{},
			}
			acc[pid] = t
			order = append(order, pid)
		}
		return t
	}
	endedRuns := make(map[string]struct{})
	lastUserPID := ""

	for _, e := range entries {
		switch e.Type {
		case session.EntryUser:
			var m core.Message
			if json.Unmarshal(e.Payload, &m) != nil {
				continue
			}
			pid := m.PromptID
			if pid == "" {
				pid = "seq:" + strconv.FormatInt(e.Seq, 10)
			}
			if lastUserPID != "" && lastUserPID != pid {
				if prev := acc[lastUserPID]; prev != nil {
					prev.closedByNext = true
				}
			}
			lastUserPID = pid
			t := ensure(pid)
			if t.user == nil {
				t.user = &turnUserView{Text: m.Text(), Seq: e.Seq, CreatedAt: e.CreatedAt}
			}
		case session.EntryUIEvent:
			var ev core.AgentEvent
			if json.Unmarshal(e.Payload, &ev) != nil {
				continue
			}
			item := seqEvent{Seq: e.Seq, CreatedAt: e.CreatedAt, AgentEvent: ev}
			if ev.Type == core.AgentEnd && ev.RunID != "" {
				endedRuns[ev.RunID] = struct{}{}
			}
			t := ensure(ev.PromptID)
			if t != nil && ev.RunID != "" {
				t.runIDs[ev.RunID] = struct{}{}
			}
			if isSessionHint(ev.Type) {
				out.Hints = append(out.Hints, item)
			}
			if t == nil {
				continue
			}
			if ev.Type == core.OutputError || ev.Type == core.BudgetExhausted {
				t.failed = true
			}
			if isTurnActivity(ev.Type) {
				t.activity = append(t.activity, item)
			}
			if isTurnResult(ev.Type) {
				t.results = append(t.results, item)
			}
		}
	}

	sessionDead := meta.Status == session.StatusInterrupted || meta.Status == session.StatusEnded
	unclosed := make([]int, 0)
	views := make([]turnView, len(order))
	for i, pid := range order {
		t := acc[pid]
		runEnded := false
		for rid := range t.runIDs {
			if _, ok := endedRuns[rid]; ok {
				runEnded = true
				break
			}
		}
		closed := t.closedByNext || runEnded
		st := "running"
		if closed {
			if t.failed {
				st = "failed"
			} else {
				st = "complete"
			}
		} else if sessionDead {
			unclosed = append(unclosed, i)
		} else if meta.Status != session.StatusRunning {
			// idle without a recorded agent_end (legacy / missing stamp):
			// the run is not in flight, so do not leave the turn hanging.
			if t.failed {
				st = "failed"
			} else {
				st = "complete"
			}
		}
		views[i] = turnView{
			PromptID:  t.promptID,
			Status:    st,
			CreatedAt: turnCreatedAt(t),
			User:      t.user,
			Activity:  t.activity,
			Results:   t.results,
		}
	}
	if sessionDead && len(unclosed) > 0 {
		last := unclosed[len(unclosed)-1]
		views[last].Status = "cancelled"
		for _, i := range unclosed[:len(unclosed)-1] {
			t := acc[order[i]]
			if t.failed {
				views[i].Status = "failed"
			} else {
				views[i].Status = "complete"
			}
		}
	}
	out.Turns = views
	return out
}

func turnCreatedAt(t *turnAcc) time.Time {
	if t == nil {
		return time.Time{}
	}
	if len(t.results) > 0 && !t.results[0].CreatedAt.IsZero() {
		return t.results[0].CreatedAt
	}
	if n := len(t.activity); n > 0 && !t.activity[n-1].CreatedAt.IsZero() {
		return t.activity[n-1].CreatedAt
	}
	if t.user != nil {
		return t.user.CreatedAt
	}
	return time.Time{}
}

func isTurnActivity(typ string) bool {
	switch typ {
	case core.ThinkingDelta, core.ToolCallDelta,
		core.ToolExecStart, core.ToolExecUpdate, core.ToolExecEnd,
		core.ToolConfirmRequest, core.ToolConfirmResponse:
		return true
	default:
		return false
	}
}

func isTurnResult(typ string) bool {
	return typ == core.TextDelta || typ == core.Output
}

func isSessionHint(typ string) bool {
	switch typ {
	case core.CompactionStart, core.CompactionEnd, core.ModelChanged,
		core.ContextFull, core.BudgetExhausted:
		return true
	default:
		return false
	}
}
