package ansi

import (
	"fmt"
	"strings"
)

// ThemeOption selects the Control Room palette.
const ThemeOption = "@llm_theme"

// DefaultTheme is used when @llm_theme is unset or unknown.
const DefaultTheme = "catppuccin-mocha"

// Theme maps the Control Room's semantic roles to colors, so rendering code
// says what an element means rather than which palette entry it happens to use.
type Theme struct {
	Name string

	Text    RGB // primary text: task names, selected rows
	Subtext RGB // secondary text: unselected task names, checkout names
	Muted   RGB // metadata: ages, hints, git summaries
	Faint   RGB // structure: column headings, rules, separators

	HeaderBg    RGB // header band
	SelectionBg RGB // selected row

	Accent    RGB // the room's identity and focused borders
	Attention RGB // needs you
	Late      RGB // a wait that has gone on for a while
	Working   RGB
	Done      RGB // finished but not yet viewed
	Error     RGB // failed turns and exited agents
	Pinned    RGB
	Branch    RGB
	Added     RGB
	Removed   RGB
	Ahead     RGB // commits a worktree has made since it branched

	agents       map[string]RGB
	unknownAgent RGB
}

// Agent returns the stable badge color for an agent name. Unknown agents get a
// muted neutral so the badge still reads without competing for attention.
func (t Theme) Agent(name string) RGB {
	if color, ok := t.agents[name]; ok {
		return color
	}
	return t.unknownAgent
}

var themes = map[string]Theme{
	"catppuccin-mocha": {
		Name:    "catppuccin-mocha",
		Text:    Text,
		Subtext: Subtext0,
		Muted:   Overlay0,
		Faint:   Surface2,

		HeaderBg:    Surface0,
		SelectionBg: Surface1,

		Accent:    Blue,
		Attention: Yellow,
		Late:      Peach,
		Working:   Blue,
		Done:      Teal,
		Error:     Red,
		Pinned:    Mauve,
		Branch:    Mauve,
		Added:     Green,
		Removed:   Red,
		Ahead:     RGB{116, 199, 236},

		agents:       map[string]RGB{"opencode": Blue, "claude": Peach, "amp": Red, "codex": Green},
		unknownAgent: Surface2,
	},
	"tokyonight": {
		Name:    "tokyonight",
		Text:    RGB{192, 202, 245},
		Subtext: RGB{169, 177, 214},
		Muted:   RGB{115, 122, 162},
		Faint:   RGB{84, 92, 126},

		HeaderBg:    RGB{41, 46, 66},
		SelectionBg: RGB{40, 52, 87},

		Accent:    RGB{122, 162, 247},
		Attention: RGB{224, 175, 104},
		Late:      RGB{255, 158, 100},
		Working:   RGB{122, 162, 247},
		Done:      RGB{115, 218, 202},
		Error:     RGB{247, 118, 142},
		Pinned:    RGB{187, 154, 247},
		Branch:    RGB{187, 154, 247},
		Added:     RGB{158, 206, 106},
		Removed:   RGB{247, 118, 142},
		Ahead:     RGB{125, 207, 255},

		agents: map[string]RGB{
			"opencode": {122, 162, 247},
			"claude":   {255, 158, 100},
			"amp":      {247, 118, 142},
			"codex":    {158, 206, 106},
		},
		unknownAgent: RGB{84, 92, 126},
	},
}

// ThemeNamed returns the named theme, falling back to DefaultTheme.
func ThemeNamed(name string) Theme {
	if theme, ok := themes[strings.ToLower(strings.TrimSpace(name))]; ok {
		return theme
	}
	return themes[DefaultTheme]
}

// Hex renders a color as #rrggbb for tmux style strings.
func Hex(c RGB) string {
	return fmt.Sprintf("#%02x%02x%02x", c[0], c[1], c[2])
}

// Italic marks text the user did not write, such as automatic task names.
const Italic = "\x1b[3m"
