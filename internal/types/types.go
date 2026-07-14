package types

// State represents the activity state of an LLM session.
type State string

const (
	Working State = "working"
	Waiting State = "waiting"
	Idle    State = "idle"
)

var states = []State{Working, Waiting, Idle}

// IsState reports whether value is a known session state.
func IsState(value string) bool {
	switch State(value) {
	case Working, Waiting, Idle:
		return true
	}
	return false
}

// Session describes a single managed tmux window running an LLM agent.
type Session struct {
	Name         string // parent tmux session name
	WindowID     string // tmux window id, e.g. "@1"
	WindowIndex  int    // tmux window index, e.g. 0
	WindowName   string // tmux window name (the agent binary basename, e.g. opencode)
	Label        string // optional human-readable task/purpose
	State        State
	DisplayState State // effective state frozen at the latest tmux refresh
	StateAt      int64
	Path         string
	Origin       string
}
