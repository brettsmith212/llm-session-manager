package hookpayload

import (
	"encoding/json"
	"testing"
)

func TestToolSummary(t *testing.T) {
	tests := []struct {
		name  string
		tool  string
		input string
		want  string
	}{
		{name: "claude bash", tool: "Bash", input: `{"command":"npm run db:migrate\nnpm test","description":"Apply migration"}`, want: "Bash · npm run db:migrate"},
		{name: "file edits show the file name", tool: "Edit", input: `{"file_path":"/repo/internal/picker/picker.go","old_string":"a"}`, want: "Edit · picker.go"},
		{name: "web fetches show the host", tool: "WebFetch", input: `{"url":"https://docs.github.com/en/rest","prompt":"x"}`, want: "WebFetch · docs.github.com"},
		{name: "codex argument vectors unwrap the shell", tool: "shell", input: `{"command":["bash","-lc","cargo test --workspace"]}`, want: "shell · cargo test --workspace"},
		{name: "plain argument vectors join", tool: "exec_command", input: `{"command":["git","status"]}`, want: "exec_command · git status"},
		{name: "patches name the first file", tool: "apply_patch", input: `{"input":"*** Begin Patch\n*** Update File: src/lib.rs\n@@"}`, want: "apply_patch · lib.rs"},
		{name: "mcp tools read as server and tool", tool: "mcp__github__create_issue", input: `{"title":"x"}`, want: "github · create_issue"},
		{name: "unknown input keeps the tool name", tool: "Task", input: `not json`, want: "Task"},
		{name: "nothing known", tool: "", input: `{}`, want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ToolSummary(tt.tool, json.RawMessage(tt.input)); got != tt.want {
				t.Fatalf("ToolSummary() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestQuestionPlanAndErrorText(t *testing.T) {
	question := json.RawMessage(`{"questions":[{"question":"Retry with jitter or fixed backoff?","options":[]}]}`)
	if got := Question(question); got != "Retry with jitter or fixed backoff?" {
		t.Fatalf("Question() = %q", got)
	}
	if got := Question(json.RawMessage(`{"prompt":"Which database?"}`)); got != "Which database?" {
		t.Fatalf("Question(prompt) = %q", got)
	}
	if got := PlanTitle(json.RawMessage(`{"plan":"\n## Migrate the session store\n\n1. Add redis"}`)); got != "Migrate the session store" {
		t.Fatalf("PlanTitle() = %q", got)
	}
	if got := ErrorText(json.RawMessage(`{"type":"overloaded_error","message":"Overloaded"}`)); got != "Overloaded" {
		t.Fatalf("ErrorText(object) = %q", got)
	}
	if got := ErrorText(json.RawMessage(`"rate limited"`)); got != "rate limited" {
		t.Fatalf("ErrorText(string) = %q", got)
	}
	if got := ErrorText(nil, "", "API Error: 500"); got != "API Error: 500" {
		t.Fatalf("ErrorText(fallback) = %q", got)
	}
}
