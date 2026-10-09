package agent

import "testing"

func TestLaunchCommand(t *testing.T) {
	tests := []struct {
		command string
		want    string
	}{
		{command: "codex", want: "codex --no-daemon"},
		{command: "/opt/bin/codex --model gpt-6", want: "/opt/bin/codex --model gpt-6 --no-daemon"},
		{command: "codex resume --last", want: "codex resume --last --no-daemon"},
		{command: "codex --no-daemon", want: "codex --no-daemon"},
		{command: "claude", want: "claude"},
		{command: "opencode --port 0", want: "opencode --port 0"},
	}
	for _, tt := range tests {
		if got := LaunchCommand(tt.command); got != tt.want {
			t.Errorf("LaunchCommand(%q) = %q, want %q", tt.command, got, tt.want)
		}
	}
}
