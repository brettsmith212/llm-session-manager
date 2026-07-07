package picker

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"

	"llm-session-manager/internal/ansi"
	"llm-session-manager/internal/sessions"
	"llm-session-manager/internal/tmux"
	"llm-session-manager/internal/types"
)

const windowName = "llm-picker"

// Run starts the ANSI session picker.
func Run() error {
	prefix := tmux.GetGlobalOption("@llm_session_prefix", "llm-")
	parent := tmux.GetGlobalOption("@llm_parent", "")

	sess := sessions.GetAllSessions(prefix)
	p := &picker{
		prefix:        prefix,
		parent:        parent,
		sessions:      sess,
		query:         "",
		selectedIndex: 0,
		isSearching:   false,
		pickerActive:  true,
	}

	oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		return fmt.Errorf("failed to set raw mode: %w", err)
	}
	defer term.Restore(int(os.Stdin.Fd()), oldState)

	fmt.Print(ansi.HideCursor)
	defer fmt.Print(ansi.ShowCursor)

	resize := make(chan os.Signal, 1)
	signal.Notify(resize, syscall.SIGWINCH)
	defer signal.Stop(resize)

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	input := make(chan string, 16)
	go readInput(input)

	// ── B: poll which pane the user has focused (picker left vs preview
	// right). The picker pane (.0) is the only one reading keys, but tmux
	// flips pane_active when the user clicks / Ctrl-arrows into the preview.
	// We sample pane_active every ~150ms and re-render on change so the
	// in-canvas header reflects reality.
	activeChange := make(chan bool, 4)
	go pollPaneActive(activeChange)

	if first := p.filtered(); len(first) > 0 {
		p.updatePreview(first[0])
	}
	p.render()

	lastSnapshot := p.snapshot()

	for {
		select {
		case <-ticker.C:
			next := sessions.GetAllSessions(prefix)
			hadSessions := len(p.sessions) > 0
			p.sessions = next
			snap := p.snapshot()
			if snap != lastSnapshot {
				lastSnapshot = snap
				p.render()
			}
			if !hadSessions && len(next) > 0 && p.selectedIndex < len(next) {
				p.updatePreview(next[p.selectedIndex])
			}
		case <-resize:
			p.render()
		case active := <-activeChange:
			if active != p.pickerActive {
				p.pickerActive = active
				p.render()
			}
		case keys := <-input:
			if done := p.handleKeys(keys); done {
				return nil
			}
		}
	}
}

type picker struct {
	prefix        string
	parent        string
	sessions      []types.Session
	query         string
	selectedIndex int
	isSearching   bool
	pickerActive  bool // true when left pane (.0) is the active tmux pane
}

func (p *picker) filtered() []types.Session {
	q := strings.ToLower(p.query)
	if q == "" {
		return p.sessions
	}
	out := make([]types.Session, 0, len(p.sessions))
	for _, s := range p.sessions {
		if strings.Contains(strings.ToLower(s.Path), q) || strings.Contains(strings.ToLower(s.Name), q) {
			out = append(out, s)
		}
	}
	return out
}

func (p *picker) snapshot() string {
	parts := make([]string, len(p.sessions))
	for i, s := range p.sessions {
		parts[i] = fmt.Sprintf("%s:%s:%d:%s:%d:%s", s.Name, s.WindowID, s.WindowIndex, s.State, s.StateAt, s.Path)
	}
	return strings.Join(parts, "|")
}

func (p *picker) render() {
	list := p.filtered()
	cols, rows, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil {
		cols, rows = 80, 24
	}

	const itemHeight = 1
	const headerRows = 6
	// Leave extra room because each new session group adds a header row.
	visibleCount := max(1, (rows-headerRows-2)/(itemHeight+1))

	if p.selectedIndex >= len(list) {
		p.selectedIndex = max(0, len(list)-1)
	}
	startIndex := max(0, min(p.selectedIndex, max(0, len(list)-visibleCount)))
	visible := list[startIndex:min(len(list), startIndex+visibleCount)]

	fmt.Print(ansi.ClearScreen)

	row := 1
	// Title — bright blue bar when picker is focused; muted hint bar when
	// the user has moved focus into the preview pane on the right.
	if p.pickerActive {
		writeLineBg(row, cols, fmt.Sprintf(" %s%s◆ Sessions%s", ansi.Foreground(ansi.Base), ansi.Bold, ansi.Reset), ansi.Blue)
	} else {
		writeLineBg(row, cols,
			fmt.Sprintf(" %s%s◀ focus picker%s   %spreview active — ← to return%s",
				ansi.Foreground(ansi.Overlay2), ansi.Bold, ansi.Reset,
				ansi.Foreground(ansi.Overlay0), ansi.Reset),
			ansi.Surface0)
	}
	row++

	// Counter / search
	if p.isSearching {
		writeLine(row, cols, fmt.Sprintf("  %s/%s%s%s%s_%s",
			ansi.Foreground(ansi.Overlay0),
			ansi.Reset,
			ansi.Foreground(ansi.Text),
			p.query,
			ansi.Foreground(ansi.Blue),
			ansi.Reset))
	} else {
		counter := fmt.Sprintf("  %s%d%s/%s%d%s",
			ansi.Foreground(ansi.Overlay2), p.selectedIndex+1,
			ansi.Foreground(ansi.Overlay0), ansi.Foreground(ansi.Overlay2), len(list), ansi.Reset)
		if p.query != "" {
			counter += fmt.Sprintf("  %sfilter: %s%s%s", ansi.Foreground(ansi.Overlay0), ansi.Foreground(ansi.Text), p.query, ansi.Reset)
		}
		writeLine(row, cols, counter)
	}
	row++

	// Help — dimmed to overlay0 (with cancelled accent glyphs) when the
	// picker is blurred so the eye is drawn away to the preview pane.
	helpAccent := ansi.Foreground(ansi.Surface2)
	helpMuted := ansi.Foreground(ansi.Overlay0)
	if !p.pickerActive {
		helpAccent = ansi.Foreground(ansi.Overlay0)
		helpMuted = ansi.Foreground(ansi.Crust)
	}
	help1 := fmt.Sprintf("  %s↑↓%s %snav%s  %s/%s %sfind%s   %s⏎%s %sopen%s",
		helpAccent, ansi.Reset, helpMuted, ansi.Reset,
		helpAccent, ansi.Reset, helpMuted, ansi.Reset,
		helpAccent, ansi.Reset, helpMuted, ansi.Reset)
	help2 := fmt.Sprintf("  %sa%s %sadd%s   %s^x%s %skill%s  %sq%s %squit%s",
		helpAccent, ansi.Reset, helpMuted, ansi.Reset,
		helpAccent, ansi.Reset, helpMuted, ansi.Reset,
		helpAccent, ansi.Reset, helpMuted, ansi.Reset)
	writeLine(row, cols, help1)
	row++
	writeLine(row, cols, help2)
	row++

	// Divider
	div := fmt.Sprintf("  %s%s%s", ansi.Foreground(ansi.Surface0), strings.Repeat("─", min(listWidth(cols)-4, cols-4)), ansi.Reset)
	writeLine(row, cols, div)
	row++

	if len(visible) == 0 {
		writeLine(row, cols, fmt.Sprintf("  %sno sessions — press %sa%s to create%s", ansi.Foreground(ansi.Overlay0), ansi.Foreground(ansi.Blue), ansi.Foreground(ansi.Overlay0), ansi.Reset))
		return
	}

	var prevSession string
	for i, session := range visible {
		if session.Name != prevSession {
			path := sessions.FormatPath(session.Path)
			if path == "" {
				path = session.Name
			}
			pathColor := ansi.Blue
			if !p.pickerActive {
				pathColor = ansi.Overlay0
			}
			header := fmt.Sprintf("  %s%s%s", ansi.Foreground(pathColor), path, ansi.Reset)
			writeLine(row, cols, header)
			row++
			prevSession = session.Name
		}
		selected := startIndex+i == p.selectedIndex
		isLastInGroup := i == len(visible)-1 || visible[i+1].Name != session.Name
		row = drawItem(session, cols, selected, row, isLastInGroup, p.pickerActive)
		if i < len(visible)-1 && visible[i+1].Name != session.Name {
			writeLine(row, cols, "")
			row++
		}
	}
}

func drawItem(session types.Session, cols int, selected bool, row int, isLastInGroup bool, active bool) int {
	inner := cols - 24
	stateStr := string(sessions.EffectiveState(session))
	sc := stateColor(stateStr)
	ago := sessions.FormatAgo(session.StateAt)
	nameStr := truncate(fmt.Sprintf("%s · #%d", session.Name, session.WindowIndex), inner)

	connector := "├─"
	if isLastInGroup {
		connector = "└─"
	}

	statePadded := fmt.Sprintf("%-7s", stateStr)

	// ── C: when the picker pane is blurred, dim the entire list to a single
	// overlay0 tone so the left side reads as "asleep" and the eye is drawn
	// to the now-active preview pane on the right. Selection highlight is
	// also dropped (no bg fill) so an inactive selection doesn't scream.
	if !active {
		dim := ansi.Foreground(ansi.Overlay0)
		line1 := fmt.Sprintf("  %s%s%s %s● %s%s%s  %s%s  %s%s%s",
			dim, connector, ansi.Reset,
			dim, dim, statePadded, ansi.Reset,
			dim, ago,
			dim, nameStr, ansi.Reset)
		writeLine(row, cols, line1)
		return row + 1
	}

	if selected {
		accent := ansi.Foreground(ansi.Blue)
		dot := ansi.Foreground(sc)
		txt := ansi.Foreground(ansi.Text)
		muted := ansi.Foreground(ansi.Overlay2)

		line1 := fmt.Sprintf("  %s%s %s● %s%s  %s%s  %s%s", accent, connector, dot, txt, statePadded, muted, ago, muted, nameStr)

		writeLineBg(row, cols, line1, ansi.Surface0)
	} else {
		tree := ansi.Foreground(ansi.Blue)
		dot := ansi.Foreground(sc)
		txt := ansi.Foreground(ansi.Subtext0)
		muted := ansi.Foreground(ansi.Overlay0)

		line1 := fmt.Sprintf("  %s%s%s %s● %s%s%s  %s%s  %s%s%s", tree, connector, ansi.Reset, dot, txt, statePadded, ansi.Reset, muted, ago, muted, nameStr, ansi.Reset)

		writeLine(row, cols, line1)
	}

	return row + 1
}

func stateColor(state string) ansi.RGB {
	switch state {
	case "working":
		return ansi.Red
	case "idle":
		return ansi.Green
	case "waiting":
		return ansi.Yellow
	default:
		return ansi.Overlay0
	}
}

func writeLine(row, cols int, content string) {
	fmt.Printf("%s%s%s%s", ansi.CursorPos(row, 1), ansi.ClearLine(), content, ansi.Reset)
}

func writeLineBg(row, cols int, content string, bg ansi.RGB) {
	bgSeq := ansi.Background(bg)
	fmt.Printf("%s%s%s%s%s%s%s%s",
		ansi.CursorPos(row, 1), bgSeq, strings.Repeat(" ", cols), ansi.Reset,
		ansi.CursorPos(row, 1), bgSeq, content, ansi.Reset)
}

func listWidth(cols int) int {
	return cols
}

func truncate(str string, width int) string {
	if len(str) <= width {
		return str
	}
	if width <= 1 {
		return "…"
	}
	return str[:width-1] + "…"
}

func (p *picker) updatePreview(session types.Session) {
	cmd := tmux.AttachCommand(session.Name, true) + " \\; select-window -t " + tmux.ShellQuote(session.WindowID)
	result := tmux.RunRaw([]string{"list-panes", "-t", windowName, "-F", "#{pane_index}"})
	if result.ExitCode != 0 {
		return
	}
	panes := strings.Fields(result.Stdout)
	if len(panes) > 1 {
		_ = tmux.RunRaw([]string{"respawn-pane", "-k", "-t", ":" + windowName + ".1", cmd})
	} else {
		_, _ = tmux.Run([]string{"split-window", "-h", "-l", "67%", "-t", ":" + windowName + ".0", cmd})
	}
	// Refresh the preview pane title with the live name + window index so
	// the "▶ Preview · …" border label always tracks what is shown there.
	_ = tmux.RunRaw([]string{"select-pane", "-T",
		fmt.Sprintf("▶ Preview · %s #%d", session.Name, session.WindowIndex),
		"-t", ":" + windowName + ".1"})
	_, _ = tmux.Run([]string{"select-pane", "-t", ":" + windowName + ".0"})
}

func (p *picker) changeSelection(delta int) {
	list := p.filtered()
	p.selectedIndex = max(0, min(len(list)-1, p.selectedIndex+delta))
	if p.selectedIndex < len(list) {
		p.updatePreview(list[p.selectedIndex])
	}
	p.render()
}

func (p *picker) activateSession() {
	list := p.filtered()
	if p.selectedIndex >= len(list) {
		return
	}
	session := list[p.selectedIndex]

	width := tmux.GetGlobalOption("@llm_popup_width", "90%")
	height := tmux.GetGlobalOption("@llm_popup_height", "90%")

	origin := session.Origin
	if origin == "" {
		origin = tmux.GetSessionOption(session.Name, "@llm_origin")
	}
	if origin == "" {
		origin = tmux.GetParentSession()
	}
	if origin != "" && origin != session.Name {
		_ = tmux.EnsureOriginWindow(origin, session.Path, p.parent)
	}

	if p.parent != "" {
		attachCmd := tmux.AttachCommand(session.Name, false) + " \\; select-window -t " + tmux.ShellQuote(session.WindowID)
		cmd := exec.Command("tmux", "display-popup",
			"-c", p.parent,
			"-w", width,
			"-h", height,
			"-E",
			attachCmd)
		cmd.Stdin = nil
		cmd.Stdout = nil
		cmd.Stderr = nil
		_ = cmd.Start()
	}
	_ = tmux.RunRaw([]string{"kill-window", "-t", windowName})
}

func (p *picker) killSelected() {
	list := p.filtered()
	if p.selectedIndex >= len(list) {
		return
	}
	session := list[p.selectedIndex]

	_ = tmux.KillWindow(session.Name + ":" + session.WindowID)

	// If the parent session has no opencode windows left, clean it up too.
	remaining := tmux.RunRaw([]string{"list-windows", "-t", session.Name, "-F", "#{@llm_opencode}"})
	hasOpencode := false
	if remaining.ExitCode == 0 && remaining.Stdout != "" {
		for _, line := range strings.Split(remaining.Stdout, "\n") {
			if strings.TrimSpace(line) != "" {
				hasOpencode = true
				break
			}
		}
	}
	if !hasOpencode {
		_ = tmux.KillSession(session.Name)
	}

	p.sessions = sessions.GetAllSessions(p.prefix)
	if len(p.sessions) == 0 {
		_ = tmux.RunRaw([]string{"kill-window", "-t", windowName})
		return
	}
	if p.selectedIndex >= len(p.sessions) {
		p.selectedIndex = max(0, len(p.sessions)-1)
	}
	if next := p.filtered(); len(next) > 0 && p.selectedIndex < len(next) {
		p.updatePreview(next[p.selectedIndex])
	}
	p.render()
}

func (p *picker) handleKeys(data string) (done bool) {
	keys := parseKeys(data)
	for _, key := range keys {
		if p.isSearching {
			done := p.handleSearchKey(key)
			if done {
				return true
			}
			continue
		}

		switch key {
		case "\x1b[A", "k":
			p.changeSelection(-1)
		case "\x1b[B", "j":
			p.changeSelection(1)
		case "\r":
			p.activateSession()
			return true
		case "/":
			p.isSearching = true
			p.query = ""
			p.selectedIndex = 0
			p.render()
		case "a":
			p.openCreatePopup()
		case "\x18": // ^x
			p.killSelected()
			if len(p.sessions) == 0 {
				return true
			}
		case "\x03", "\x1b", "q": // ^c, esc, q
			_ = tmux.RunRaw([]string{"kill-window", "-t", windowName})
			return true
		}
	}
	return false
}

func (p *picker) handleSearchKey(key string) (done bool) {
	if len(key) == 0 {
		return false
	}
	code := key[0]
	switch {
	case code == 27 || code == 13: // esc or enter
		p.isSearching = false
		p.render()
	case code == 127 || code == 8: // backspace
		if len(p.query) > 0 {
			p.query = p.query[:len(p.query)-1]
		}
		p.selectedIndex = 0
		if list := p.filtered(); len(list) > 0 {
			p.updatePreview(list[0])
		}
		p.render()
	case code >= 32 && code <= 126:
		p.query += key
		p.selectedIndex = 0
		if list := p.filtered(); len(list) > 0 {
			p.updatePreview(list[0])
		}
		p.render()
	}
	return false
}

func (p *picker) openCreatePopup() {
	list := p.filtered()
	defaultPath := ""
	if p.selectedIndex < len(list) {
		defaultPath = list[p.selectedIndex].Path
	}
	if defaultPath == "" {
		if cwd, err := os.Getwd(); err == nil {
			defaultPath = cwd
		}
	}

	width := tmux.GetGlobalOption("@llm_popup_width", "90%")
	height := "30%"

	cmd := exec.Command("tmux", "display-popup",
		"-w", width,
		"-h", height,
		"-E",
		binaryPath()+" prompt "+tmux.ShellQuote(defaultPath))
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	_ = cmd.Start()
}

func binaryPath() string {
	if exe, err := os.Executable(); err == nil && exe != "" {
		return exe
	}
	if path, err := exec.LookPath("llmux"); err == nil {
		return path
	}
	return os.Args[0]
}

func readInput(out chan<- string) {
	reader := bufio.NewReader(os.Stdin)
	for {
		b, err := reader.ReadByte()
		if err != nil {
			return
		}

		var buf strings.Builder
		buf.WriteByte(b)

		// If this looks like the start of an escape sequence, try to read the rest.
		if b == '\x1b' {
			_ = os.Stdin.SetReadDeadline(time.Now().Add(20 * time.Millisecond))
			for {
				next, err := reader.ReadByte()
				_ = os.Stdin.SetReadDeadline(time.Time{})
				if err != nil {
					break
				}
				buf.WriteByte(next)
				// CSI sequences end at 0x40-0x7e after params/intermediates.
				if next >= 0x40 && next <= 0x7e && buf.Len() > 2 {
					break
				}
				_ = os.Stdin.SetReadDeadline(time.Now().Add(20 * time.Millisecond))
			}
		}

		out <- buf.String()
	}
}

func parseKeys(data string) []string {
	var tokens []string
	i := 0
	for i < len(data) {
		if data[i] == '\x1b' && i+1 < len(data) && data[i+1] == '[' {
			j := i + 2
			for j < len(data) && data[j] >= 0x30 && data[j] <= 0x3f {
				j++
			}
			for j < len(data) && data[j] >= 0x20 && data[j] <= 0x2f {
				j++
			}
			if j < len(data) && data[j] >= 0x40 && data[j] <= 0x7e {
				j++
			}
			tokens = append(tokens, data[i:j])
			i = j
		} else {
			tokens = append(tokens, string(data[i]))
			i++
		}
	}
	return tokens
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// pollPaneActive reports whether the picker pane (.0) is the active tmux
// pane. It sends the current value once on startup, then pushes only on
// change so the render loop sleeps idle otherwise.
func pollPaneActive(out chan<- bool) {
	last := true
	out <- last
	t := time.NewTicker(150 * time.Millisecond)
	defer t.Stop()
	for range t.C {
		res := tmux.RunRaw([]string{"display-message", "-p", "-t", windowName + ".0", "#{pane_active}"})
		active := strings.TrimSpace(res.Stdout) == "1"
		if active != last {
			last = active
			out <- active
		}
	}
}
