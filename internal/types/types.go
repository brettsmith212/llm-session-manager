package types

// State represents the activity state of an LLM session.
type State string

const (
	Working State = "working"
	Waiting State = "waiting"
	Idle    State = "idle"
)

// IsState reports whether value is a known session state.
func IsState(value string) bool {
	switch State(value) {
	case Working, Waiting, Idle:
		return true
	}
	return false
}

// ReasonKind classifies why a waiting agent needs the user.
type ReasonKind string

const (
	ReasonPermission ReasonKind = "permission"
	ReasonQuestion   ReasonKind = "question"
	ReasonPlan       ReasonKind = "plan"
	ReasonInput      ReasonKind = "input"
	ReasonError      ReasonKind = "error"
)

// Title is the short human name shown for a reason kind.
func (k ReasonKind) Title() string {
	switch k {
	case ReasonPermission:
		return "Permission"
	case ReasonQuestion:
		return "Question"
	case ReasonPlan:
		return "Plan ready"
	case ReasonInput:
		return "Input"
	case ReasonError:
		return "Error"
	}
	return ""
}

// Session describes a single managed tmux window running an LLM agent.
type Session struct {
	Name         string // parent tmux session name
	WindowID     string // tmux window id, e.g. "@1"
	WindowIndex  int    // tmux window index, e.g. 0
	WindowName   string // tmux window name (the agent binary basename, e.g. opencode)
	Label        string // optional human-readable task/purpose
	AutoLabel    string // task name taken from the first prompt when Label is empty (@llm_label_auto)
	State        State
	DisplayState State // effective state frozen at the latest tmux refresh
	StateAt      int64 // when the agent last changed state (@llm_state_at)
	SeenAt       int64 // when the agent last reported any state (@llm_seen_at)
	ActivityAt   int64 // when the agent's pane last printed output (#{window_activity})
	Path         string
	Origin       string
	Pinned       bool       // pinned to the top of the control room via the `p` key (@llm_pinned window option)
	Unseen       bool       // finished a turn the user has not looked at yet (@llm_unseen)
	ReasonKind   ReasonKind // why a waiting agent needs the user (@llm_reason_kind)
	Reason       string     // one-line detail for ReasonKind, e.g. a command (@llm_reason)
	Exited       bool       // the agent process exited with an error and its pane was kept (#{pane_dead})
	ExitStatus   int        // exit status of an exited agent (#{pane_dead_status})
	AgentSession string     // the agent's own conversation id, used to resume it (@llm_agent_session)
	StartCommand string     // the command the agent's pane was started with (#{pane_start_command})
}

// Title returns the task name to display: the user's label, else the
// automatic one.
func (s Session) Title() string {
	if s.Label != "" {
		return s.Label
	}
	return s.AutoLabel
}
