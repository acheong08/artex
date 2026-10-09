package server

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/Autumn-27/norma/llm"
)

// Recognition and continuation of no-op turns (reasoning only, no text or tools);
// see steerHooks.Stop.

func assistantThinking(text string) llm.Message {
	return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
		{Type: llm.BlockThinking, Thinking: text, Signature: "sig"},
	}}
}

func TestIsThinkingOnlyTurn(t *testing.T) {
	toolUse := llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
		{Type: llm.BlockThinking, Thinking: "Scan ports first"},
		{Type: llm.BlockToolUse, ID: "t1", Name: "run_nuclei"},
	}}
	cases := []struct {
		name string
		msgs []llm.Message
		want bool
	}{
		{"reasoning only", []llm.Message{llm.UserText("Start"), assistantThinking("Think")}, true},
		{"reasoning + tool", []llm.Message{llm.UserText("Start"), toolUse}, false},
		{"reasoning + text", []llm.Message{assistantThinking("Think"), {
			Role:    llm.RoleAssistant,
			Content: []llm.ContentBlock{{Type: llm.BlockThinking, Thinking: "x"}, llm.TextBlock("Conclusion")},
		}}, false},
		{"text contains only whitespace", []llm.Message{{
			Role:    llm.RoleAssistant,
			Content: []llm.ContentBlock{{Type: llm.BlockThinking, Thinking: "x"}, llm.TextBlock("  \n ")},
		}}, true},
		{"completely empty assistant turn", []llm.Message{{Role: llm.RoleAssistant}}, true},
		// Tool results use the user role, so detection must check the preceding
		// assistant message instead of misclassifying the result itself.
		{"last message is a tool result", []llm.Message{toolUse, {
			Role:    llm.RoleUser,
			Content: []llm.ContentBlock{{Type: llm.BlockToolResult, ToolUseID: "t1"}},
		}}, false},
		{"no assistant message", []llm.Message{llm.UserText("Start")}, false},
		{"empty history", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isThinkingOnlyTurn(c.msgs); got != c.want {
				t.Fatalf("isThinkingOnlyTurn = %v, want %v", got, c.want)
			}
		})
	}
}

// fakeHooks is a programmable inner HookRunner used to verify that steerHooks
// respects decisions made by the inner hook.
type fakeHooks struct {
	prevent  bool
	blocking []string
	msg      string
}

func (f fakeHooks) PreToolUse(context.Context, string, []byte) (bool, string, []byte) {
	return false, "", nil
}
func (f fakeHooks) PostToolUse(context.Context, string, []byte, []byte, bool) {}
func (f fakeHooks) Stop(context.Context, []llm.Message) (bool, []string, string) {
	return f.prevent, f.blocking, f.msg
}

func TestSteerHooksStopNudgesEmptyTurn(t *testing.T) {
	empty := []llm.Message{assistantThinking("I should enumerate subdomains first")}
	t.Run("inject continuation after no-op turn", func(t *testing.T) {
		h := steerHooks{nudges: &atomic.Int64{}, limit: defaultEmptyTurnNudges, label: "worker-1 · #1"}
		prevent, blocking, _ := h.Stop(context.Background(), empty)
		if prevent {
			t.Fatal("no-op turn should not hard-stop")
		}
		if len(blocking) != 1 || blocking[0] != emptyTurnNudge {
			t.Fatalf("blocking = %v, want [emptyTurnNudge]", blocking)
		}
	})

	t.Run("do not intervene when text or tools are present", func(t *testing.T) {
		h := steerHooks{nudges: &atomic.Int64{}, limit: defaultEmptyTurnNudges}
		normal := []llm.Message{{
			Role:    llm.RoleAssistant,
			Content: []llm.ContentBlock{llm.TextBlock("Scan complete; no open ports found")},
		}}
		if _, blocking, _ := h.Stop(context.Background(), normal); blocking != nil {
			t.Fatalf("normal completion was misclassified as a no-op: %v", blocking)
		}
		if n := h.nudges.Load(); n != 0 {
			t.Fatalf("counter should not change when not intervening; got %d", n)
		}
	})

	t.Run("allow completion after reaching the limit", func(t *testing.T) {
		const limit = 5 // User configured "empty response retries" to 5.
		h := steerHooks{nudges: &atomic.Int64{}, limit: limit}
		for i := 1; i <= limit; i++ {
			if _, blocking, _ := h.Stop(context.Background(), empty); len(blocking) != 1 {
				t.Fatalf("attempt %d should still be within the quota; blocking = %v", i, blocking)
			}
		}
		if _, blocking, _ := h.Stop(context.Background(), empty); blocking != nil {
			t.Fatalf("continuation still injected after reaching the limit: %v", blocking)
		}
	})

	// Setting "empty response retries" to -1 disables this layer; emptyTurnNudgeLimit resolves to 0.
	t.Run("disabled by configuration", func(t *testing.T) {
		h := steerHooks{nudges: &atomic.Int64{}, limit: 0}
		if _, blocking, _ := h.Stop(context.Background(), empty); blocking != nil {
			t.Fatalf("continuation injected despite being disabled: %v", blocking)
		}
	})

	t.Run("do not override inner hard stop", func(t *testing.T) {
		h := steerHooks{inner: fakeHooks{prevent: true, msg: "guard rejected completion"}, nudges: &atomic.Int64{}, limit: defaultEmptyTurnNudges}
		prevent, blocking, msg := h.Stop(context.Background(), empty)
		if !prevent || msg != "guard rejected completion" || blocking != nil {
			t.Fatalf("inner hard stop was overridden: prevent=%v blocking=%v msg=%q", prevent, blocking, msg)
		}
		if n := h.nudges.Load(); n != 0 {
			t.Fatalf("quota should not be consumed when deferring to inner hook; got %d", n)
		}
	})

	t.Run("do not stack on inner continuation", func(t *testing.T) {
		h := steerHooks{inner: fakeHooks{blocking: []string{"guard continuation reason"}}, nudges: &atomic.Int64{}, limit: defaultEmptyTurnNudges}
		_, blocking, _ := h.Stop(context.Background(), empty)
		if len(blocking) != 1 || blocking[0] != "guard continuation reason" {
			t.Fatalf("inner continuation message was overridden: %v", blocking)
		}
	})

	t.Run("preserve behavior without a counter", func(t *testing.T) {
		h := steerHooks{limit: defaultEmptyTurnNudges} // For example, a future caller might omit nudges.
		if _, blocking, _ := h.Stop(context.Background(), empty); blocking != nil {
			t.Fatalf("continuation should not be injected without a counter: %v", blocking)
		}
	})
}
