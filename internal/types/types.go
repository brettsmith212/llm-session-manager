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

// Session describes a single managed tmux session.
type Session struct {
	Name    string
	State   State
	StateAt int64
	Path    string
	Origin  string
}
