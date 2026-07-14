package picker

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"golang.org/x/term"

	"llm-session-manager/internal/agent"
	"llm-session-manager/internal/ansi"
	"llm-session-manager/internal/sessions"
	"llm-session-manager/internal/tmux"
	"llm-session-manager/internal/types"
)

const (
	windowName            = "llm-picker"
	pickerSelectionOption = "@llm_picker_selection"
	previewDelay          = 75 * time.Millisecond
)

// Run starts the ANSI session picker.
func Run() error {
	prefix := tmux.GetGlobalOption("@llm_session_prefix", "llm-")
	parent := tmux.GetGlobalOption("@llm_parent", "")

	sess := sessions.GetAllSessions(prefix)
	_ = sessions.PublishWaitingStatus(sess)
	p := &picker{
		prefix:        prefix,
		parent:        parent,
		sessions:      sess,
		query:         "",
		selectedIndex: 0,
		isSearching:   false,
		popupClosed:   make(chan struct{}, 1),
	}
	p.previewTimer = time.NewTimer(time.Hour)
	if !p.previewTimer.Stop() {
		<-p.previewTimer.C
	}
	defer p.previewTimer.Stop()

	oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		return fmt.Errorf("failed to set raw mode: %w", err)
	}
	defer term.Restore(int(os.Stdin.Fd()), oldState)

	fmt.Print(ansi.HideCursor + ansi.BracketedPasteOn)
	defer fmt.Print(ansi.BracketedPasteOff + ansi.ShowCursor)

	resize := make(chan os.Signal, 1)
	signal.Notify(resize, syscall.SIGWINCH)
	defer signal.Stop(resize)

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	input := make(chan string, 16)
	go readInput(input)

	// Paint the interactive pane before doing any tmux work for the preview.
	p.render()
	if first := p.filtered(); len(first) > 0 {
		p.updatePreview(first[0])
	}

	lastSnapshot := p.snapshot()

	for {
		select {
		case <-ticker.C:
			next := sessions.GetAllSessions(prefix)
			_ = sessions.PublishWaitingStatus(next)
			hadSessions := len(p.sessions) > 0
			p.sessions = next
			selectedCreated := p.selectCreatedSession()
			snap := p.snapshot()
			if snap != lastSnapshot || selectedCreated {
				lastSnapshot = snap
				p.render()
				if len(p.filtered()) == 0 {
					p.cancelPreview()
				}
			}
			if !selectedCreated && !hadSessions && len(next) > 0 && p.selectedIndex < len(next) {
				p.queuePreview(next[p.selectedIndex])
			}
		case <-p.popupClosed:
			p.sessions = sessions.GetAllSessions(prefix)
			p.selectCreatedSession()
			lastSnapshot = p.snapshot()
			p.render()
		case <-resize:
			p.render()
		case <-p.previewTimer.C:
			p.flushPreview()
		case keys := <-input:
			if done := p.handleKeys(keys); done {
				return nil
			}
		}
	}
}

type picker struct {
	prefix            string
	parent            string
	sessions          []types.Session
	query             string
	selectedIndex     int
	isSearching       bool
	searchStart       string
	searchIndex       int
	isPasting         bool
	activateErr       string // set when activateSession fails to reach the origin window
	confirmStopID     string
	confirmLabel      string
	notice            string
	noticeError       bool
	popupClosed       chan struct{}
	previewTimer      *time.Timer
	pendingPreview    types.Session
	hasPendingPreview bool
	previewWindowID   string
}

func (p *picker) filtered() []types.Session {
	terms := strings.Fields(strings.ToLower(p.query))
	if len(terms) == 0 {
		return p.sessions
	}
	out := make([]types.Session, 0, len(p.sessions))
	for _, s := range p.sessions {
		_, stateLabel, _ := statePresentation(sessions.EffectiveState(s))
		haystack := strings.ToLower(fmt.Sprintf("%s %s %s %s #%d",
			s.Path, s.Name, s.WindowName, stateLabel, s.WindowIndex))
		matches := true
		for _, term := range terms {
			if !strings.Contains(haystack, term) {
				matches = false
				break
			}
		}
		if matches {
			out = append(out, s)
		}
	}
	return out
}

// formatBadge renders an agent name as colored text (no brackets, no
// background) — just the agent name in its assigned color. Used for the
// per-row agent indicator.
func formatBadge(agentName string, color ansi.RGB) string {
	if agentName == "" {
		agentName = "?"
	}
	return ansi.Foreground(color) + agentName + ansi.Reset
}

// truncateVisible truncates a plain string (no ansi) so the total visible
// width of prefix+str+suffix fits maxVisible cells. Truncation appends "…"
// to str to signal the cut.
func truncateVisible(str, prefix, suffix string, maxVisible int) string {
	avail := maxVisible - utf8.RuneCountInString(prefix) - utf8.RuneCountInString(suffix)
	runes := []rune(str)
	if avail >= len(runes) {
		return str
	}
	if avail <= 0 {
		return ""
	}
	if avail == 1 {
		return "…"
	}
	return string(runes[:avail-1]) + "…"
}

func (p *picker) snapshot() string {
	parts := make([]string, len(p.sessions))
	for i, s := range p.sessions {
		parts[i] = fmt.Sprintf("%s:%s:%d:%s:%d:%s:%s", s.Name, s.WindowID, s.WindowIndex, s.State, s.StateAt, s.Path, s.WindowName)
	}
	return strings.Join(parts, "|")
}

func (p *picker) render() {
	list := p.filtered()
	cols, rows, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil {
		cols, rows = 80, 24
	}
	cols = max(1, cols)
	rows = max(1, rows)

	if p.selectedIndex >= len(list) {
		p.selectedIndex = max(0, len(list)-1)
	}

	frame := newScreenFrame(cols, rows)
	position := 0
	if len(list) > 0 {
		position = p.selectedIndex + 1
	}
	waiting := 0
	for _, session := range p.sessions {
		if sessions.EffectiveState(session) == types.Waiting {
			waiting++
		}
	}

	header := fmt.Sprintf(" %s%s◆ Agents%s  %s%d/%d%s",
		ansi.Foreground(ansi.Blue), ansi.Bold, ansi.Reset,
		ansi.Foreground(ansi.Overlay2), position, len(list), ansi.Reset)
	if waiting > 0 {
		header += fmt.Sprintf("  %s◆ %d needs you%s", ansi.Foreground(ansi.Yellow), waiting, ansi.Reset)
	}
	active := agent.Active()
	if active == "" {
		header += fmt.Sprintf("  %sagent %snot configured%s", ansi.Foreground(ansi.Overlay0), ansi.Foreground(ansi.Red), ansi.Reset)
	} else {
		header += fmt.Sprintf("  %sagent%s %s%s%s %s▾%s",
			ansi.Foreground(ansi.Overlay0), ansi.Reset,
			ansi.Foreground(agent.BadgeColor(active)), active, ansi.Reset,
			ansi.Foreground(ansi.Surface2), ansi.Reset)
	}
	frame.lineBg(1, header, ansi.Surface0)

	if p.isSearching {
		frame.line(2, fmt.Sprintf("  %s/%s%s%s%s_%s  %sesc cancel · enter keep%s",
			ansi.Foreground(ansi.Overlay0), ansi.Reset,
			ansi.Foreground(ansi.Text), p.query,
			ansi.Foreground(ansi.Blue), ansi.Reset,
			ansi.Foreground(ansi.Overlay0), ansi.Reset))
	} else if p.query != "" {
		frame.line(2, fmt.Sprintf("  %sfilter:%s %s%s%s  %sesc clear · / replace%s",
			ansi.Foreground(ansi.Overlay0), ansi.Reset,
			ansi.Foreground(ansi.Text), p.query, ansi.Reset,
			ansi.Foreground(ansi.Overlay0), ansi.Reset))
	} else {
		frame.line(2, fmt.Sprintf("  %s↑↓%s %snav · %s⏎%s %slive · %so/⇧⏎%s %spopup · %s/%s %sfind%s",
			ansi.Foreground(ansi.Surface2), ansi.Reset, ansi.Foreground(ansi.Overlay0),
			ansi.Foreground(ansi.Surface2), ansi.Reset, ansi.Foreground(ansi.Overlay0),
			ansi.Foreground(ansi.Surface2), ansi.Reset, ansi.Foreground(ansi.Overlay0),
			ansi.Foreground(ansi.Surface2), ansi.Reset, ansi.Foreground(ansi.Overlay0), ansi.Reset))
	}
	frame.line(3, fmt.Sprintf("  %s%s%s", ansi.Foreground(ansi.Surface0), strings.Repeat("─", max(0, cols-4)), ansi.Reset))

	row := 4
	if p.confirmStopID != "" {
		const confirmationSuffix = "?  ^x confirm · esc cancel"
		labelWidth := max(1, cols-utf8.RuneCountInString("  stop "+confirmationSuffix))
		label := truncateVisible(p.confirmLabel, "", "", labelWidth)
		frame.line(row, fmt.Sprintf("  %sstop %s%s%s",
			ansi.Foreground(ansi.Yellow), label, confirmationSuffix, ansi.Reset))
		row++
	} else if p.activateErr != "" {
		frame.line(row, fmt.Sprintf("  %scouldn't switch to parent: %s%s",
			ansi.Foreground(ansi.Red), p.activateErr, ansi.Reset))
		row++
	} else if p.notice != "" {
		color := ansi.Green
		if p.noticeError {
			color = ansi.Red
		}
		frame.line(row, fmt.Sprintf("  %s%s%s", ansi.Foreground(color), p.notice, ansi.Reset))
		row++
	}

	footerRow := rows
	if rows >= 6 {
		footer := "  a add · n needs you · s agent · ^x stop · q close"
		if cols < 60 {
			footer = "  a add · n next · s agent · ^x stop · q"
		}
		frame.line(footerRow, fmt.Sprintf("%s%s%s", ansi.Foreground(ansi.Overlay0), footer, ansi.Reset))
	} else {
		footerRow = rows + 1
	}

	availableRows := max(0, footerRow-row)
	startIndex, endIndex := visibleRange(list, p.selectedIndex, availableRows)
	if startIndex == endIndex {
		if len(list) == 0 {
			if p.query != "" {
				frame.line(row, fmt.Sprintf("  %sno matches · %sesc%s clears the filter%s",
					ansi.Foreground(ansi.Overlay0), ansi.Foreground(ansi.Blue), ansi.Foreground(ansi.Overlay0), ansi.Reset))
			} else {
				frame.line(row, fmt.Sprintf("  %sno sessions · press %sa%s to create one%s",
					ansi.Foreground(ansi.Overlay0), ansi.Foreground(ansi.Blue), ansi.Foreground(ansi.Overlay0), ansi.Reset))
			}
		}
		frame.flush()
		return
	}

	var prevSession string
	for index := startIndex; index < endIndex && row < footerRow; index++ {
		session := list[index]
		if availableRows > 1 && session.Name != prevSession {
			path := sessions.FormatPath(session.Path)
			if path == "" {
				path = session.Name
			}
			frame.line(row, fmt.Sprintf("  %s%s%s", ansi.Foreground(ansi.Blue), path, ansi.Reset))
			row++
			prevSession = session.Name
		}
		if row >= footerRow {
			break
		}
		isLastInGroup := index == len(list)-1 || list[index+1].Name != session.Name
		row = drawItem(frame, session, cols, index == p.selectedIndex, row, isLastInGroup)
	}
	frame.flush()
}

func visibleRange(list []types.Session, selected, maxRows int) (start, end int) {
	if len(list) == 0 || maxRows < 1 {
		return 0, 0
	}
	selected = max(0, min(selected, len(list)-1))
	if maxRows == 1 {
		return selected, selected + 1
	}
	start, end = selected, selected+1
	for start > 0 && renderedListRows(list[start-1:end]) <= maxRows {
		start--
	}
	for end < len(list) && renderedListRows(list[start:end+1]) <= maxRows {
		end++
	}
	return start, end
}

func renderedListRows(list []types.Session) int {
	rows := len(list)
	previous := ""
	for _, session := range list {
		if session.Name != previous {
			rows++
			previous = session.Name
		}
	}
	return rows
}

func drawItem(frame *screenFrame, session types.Session, cols int, selected bool, row int, isLastInGroup bool) int {
	state := sessions.EffectiveState(session)
	stateSymbol, stateLabel, stateColor := statePresentation(state)
	ago := sessions.FormatAgo(session.StateAt)
	agentName := session.WindowName
	if agentName == "" {
		agentName = "?"
	}
	suffixStr := fmt.Sprintf(" #%d", session.WindowIndex)

	connector := "├─"
	if isLastInGroup {
		connector = "└─"
	}

	statePadded := fmt.Sprintf("%-9s", stateLabel)
	baseWidth := 16 // indent + connector + symbol + padded state
	available := max(0, cols-baseWidth-utf8.RuneCountInString(suffixStr))
	detailPrefix := ""
	if available >= utf8.RuneCountInString(ago)+7 {
		detailPrefix = "  " + ago + "  "
	} else if available >= 4 {
		detailPrefix = "  "
	} else if available >= 2 {
		detailPrefix = " "
	}
	agentWidth := max(0, available-utf8.RuneCountInString(detailPrefix))
	agentName = truncateVisible(agentName, "", "", agentWidth)
	if agentName == "" {
		detailPrefix = ""
	}

	badge := ""
	if agentName != "" {
		badge = formatBadge(agentName, agent.BadgeColor(session.WindowName))
	}

	if selected {
		accent := ansi.Foreground(ansi.Blue)
		dot := ansi.Foreground(stateColor)
		txt := ansi.Foreground(ansi.Text)
		muted := ansi.Foreground(ansi.Overlay2)

		line1 := "  " +
			accent + connector + " " +
			dot + stateSymbol + " " + txt + statePadded +
			muted + detailPrefix +
			badge + ansi.Background(ansi.Surface0) + muted + suffixStr

		frame.lineBg(row, line1, ansi.Surface0)
	} else {
		tree := ansi.Foreground(ansi.Blue)
		dot := ansi.Foreground(stateColor)
		txt := ansi.Foreground(ansi.Subtext0)
		muted := ansi.Foreground(ansi.Overlay0)

		line1 := "  " +
			tree + connector + ansi.Reset + " " +
			dot + stateSymbol + " " + txt + statePadded + ansi.Reset +
			muted + detailPrefix + ansi.Reset +
			badge + muted + suffixStr + ansi.Reset

		frame.line(row, line1)
	}

	return row + 1
}

func statePresentation(state types.State) (symbol, label string, color ansi.RGB) {
	switch state {
	case types.Waiting:
		return "◆", "needs you", ansi.Yellow
	case types.Working:
		return "●", "working", ansi.Blue
	case types.Idle:
		return "·", "idle", ansi.Overlay0
	default:
		return "·", "starting", ansi.Overlay0
	}
}

type screenLine struct {
	content       string
	background    ansi.RGB
	hasBackground bool
}

type screenFrame struct {
	cols  int
	rows  int
	lines []screenLine
}

func newScreenFrame(cols, rows int) *screenFrame {
	return &screenFrame{cols: cols, rows: rows, lines: make([]screenLine, rows)}
}

func (f *screenFrame) line(row int, content string) {
	if row < 1 || row > f.rows {
		return
	}
	f.lines[row-1] = screenLine{content: content}
}

func (f *screenFrame) lineBg(row int, content string, background ansi.RGB) {
	if row < 1 || row > f.rows {
		return
	}
	f.lines[row-1] = screenLine{content: content, background: background, hasBackground: true}
}

func (f *screenFrame) flush() {
	var out strings.Builder
	out.Grow(f.rows * (f.cols + 24))
	for row, line := range f.lines {
		out.WriteString(ansi.CursorPos(row+1, 1))
		out.WriteString(ansi.ClearLine())
		content := line.content
		if line.hasBackground {
			background := ansi.Background(line.background)
			out.WriteString(background)
			out.WriteString(strings.Repeat(" ", f.cols))
			out.WriteString(ansi.Reset)
			out.WriteString(ansi.CursorPos(row+1, 1))
			out.WriteString(background)
			content = strings.ReplaceAll(content, ansi.Reset, ansi.Reset+background)
		}
		out.WriteString(fitANSI(content, f.cols))
		out.WriteString(ansi.Reset)
	}
	_, _ = os.Stdout.WriteString(out.String())
}

func fitANSI(value string, maxWidth int) string {
	if maxWidth <= 0 || value == "" {
		return ""
	}
	if ansiWidth(value) <= maxWidth {
		return value
	}

	var out strings.Builder
	width := 0
	target := maxWidth - 1
	for i := 0; i < len(value) && width < target; {
		if value[i] == '\x1b' && i+1 < len(value) && value[i+1] == '[' {
			end := i + 2
			for end < len(value) {
				end++
				if value[end-1] >= 0x40 && value[end-1] <= 0x7e {
					break
				}
			}
			out.WriteString(value[i:end])
			i = end
			continue
		}
		_, size := utf8.DecodeRuneInString(value[i:])
		out.WriteString(value[i : i+size])
		i += size
		width++
	}
	out.WriteString("…")
	return out.String()
}

func ansiWidth(value string) int {
	width := 0
	for i := 0; i < len(value); {
		if value[i] == '\x1b' && i+1 < len(value) && value[i+1] == '[' {
			i += 2
			for i < len(value) {
				final := value[i] >= 0x40 && value[i] <= 0x7e
				i++
				if final {
					break
				}
			}
			continue
		}
		_, size := utf8.DecodeRuneInString(value[i:])
		i += size
		width++
	}
	return width
}

func (p *picker) queuePreview(session types.Session) {
	if session.WindowID == p.previewWindowID {
		p.cancelPreview()
		return
	}
	p.pendingPreview = session
	p.hasPendingPreview = true
	if !p.previewTimer.Stop() {
		select {
		case <-p.previewTimer.C:
		default:
		}
	}
	p.previewTimer.Reset(previewDelay)
}

func (p *picker) cancelPreview() {
	p.hasPendingPreview = false
	if !p.previewTimer.Stop() {
		select {
		case <-p.previewTimer.C:
		default:
		}
	}
}

func (p *picker) flushPreview() {
	if !p.hasPendingPreview {
		return
	}
	session := p.pendingPreview
	p.hasPendingPreview = false
	p.updatePreview(session)
}

func (p *picker) renderSelection() {
	p.render()
	list := p.filtered()
	if p.selectedIndex >= len(list) {
		p.cancelPreview()
		return
	}
	p.queuePreview(list[p.selectedIndex])
}

func (p *picker) updatePreview(session types.Session) {
	cmd := tmux.AttachCommand(session.Name, true) + " \\; select-window -t " + tmux.ShellQuote(session.WindowID)
	result := tmux.RunRaw([]string{"list-panes", "-t", windowName, "-F", "#{pane_index}"})
	if result.ExitCode != 0 {
		return
	}
	panes := strings.Fields(result.Stdout)
	updated := false
	if len(panes) > 1 {
		updated = tmux.RunRaw([]string{"respawn-pane", "-k", "-t", ":" + windowName + ".1", cmd}).ExitCode == 0
	} else {
		_, err := tmux.Run([]string{"split-window", "-d", "-h", "-l", "67%", "-t", ":" + windowName + ".0", cmd})
		updated = err == nil
	}
	if !updated {
		return
	}
	// Refresh the live pane title with the name + window index so its border
	// always tracks what is shown there and advertises the outer-tmux escape.
	_ = tmux.RunRaw([]string{"select-pane", "-T",
		fmt.Sprintf("▶ Live · %s · %s #%d · prefix u returns", projectName(session), session.WindowName, session.WindowIndex),
		"-t", ":" + windowName + ".1"})
	p.previewWindowID = session.WindowID
}

func (p *picker) focusPreview() {
	list := p.filtered()
	if p.selectedIndex >= len(list) {
		return
	}
	// Navigation updates are debounced, so flush the final selection before
	// handing input to the live pane. Otherwise a quick j+Enter could focus the
	// previously selected agent for a fraction of a second.
	p.flushPreview()
	_ = tmux.RunRaw([]string{"select-pane", "-t", ":" + windowName + ".1"})
}

func (p *picker) changeSelection(delta int) {
	p.activateErr = ""
	p.confirmStopID = ""
	p.confirmLabel = ""
	p.notice = ""
	list := p.filtered()
	p.selectedIndex = max(0, min(len(list)-1, p.selectedIndex+delta))
	p.renderSelection()
}

func (p *picker) selectNextWaiting() {
	p.confirmStopID = ""
	p.confirmLabel = ""
	currentID := ""
	filtered := p.filtered()
	if p.selectedIndex < len(filtered) {
		currentID = filtered[p.selectedIndex].WindowID
	}

	// Attention navigation is intentionally global. A committed project or
	// agent filter should never hide a session that needs the user.
	p.query = ""
	list := p.sessions
	if len(list) == 0 {
		p.notice = "no sessions need attention"
		p.noticeError = false
		p.render()
		return
	}
	start := -1
	for i, session := range list {
		if session.WindowID == currentID {
			start = i
			break
		}
	}
	p.selectedIndex = max(0, start)
	for offset := 1; offset <= len(list); offset++ {
		index := (start + offset) % len(list)
		if sessions.EffectiveState(list[index]) != types.Waiting {
			continue
		}
		p.selectedIndex = index
		p.notice = ""
		p.activateErr = ""
		p.renderSelection()
		return
	}
	p.notice = "no sessions need attention"
	p.noticeError = false
	p.render()
}

func projectName(session types.Session) string {
	if session.Path != "" {
		name := filepath.Base(filepath.Clean(session.Path))
		if name != "" && name != "." {
			return name
		}
	}
	return session.Name
}

func sessionLabel(session types.Session) string {
	agentName := session.WindowName
	if agentName == "" {
		agentName = "agent"
	}
	return fmt.Sprintf("%s · %s #%d", projectName(session), agentName, session.WindowIndex)
}

// selectCreatedSession consumes the window ID left by the create prompt and
// moves the picker and live preview to that exact new session.
func (p *picker) selectCreatedSession() bool {
	windowID := tmux.GetWindowOption(windowName, pickerSelectionOption)
	if windowID == "" {
		return false
	}
	for i, session := range p.sessions {
		if session.WindowID != windowID {
			continue
		}
		p.query = ""
		p.selectedIndex = i
		p.activateErr = ""
		p.confirmStopID = ""
		p.confirmLabel = ""
		p.notice = ""
		_ = tmux.SetWindowOption(windowName, pickerSelectionOption, "")
		p.queuePreview(session)
		return true
	}
	return false
}

// activateSession attempts to switch to the session's origin window and pop
// it up. It returns true only once the picker window has actually been
// handed off (killed) — false means the picker should stay open so the user
// can see the error and retry rather than being left in a dead pane.
func (p *picker) activateSession() bool {
	list := p.filtered()
	if p.selectedIndex >= len(list) {
		return false
	}
	session := list[p.selectedIndex]

	width := tmux.GetGlobalOption("@llm_popup_width", "90%")
	height := tmux.GetGlobalOption("@llm_popup_height", "90%")
	parent := tmux.GetGlobalOption("@llm_parent", p.parent)

	origin := session.Origin
	if origin == "" {
		origin = tmux.GetSessionOption(session.Name, "@llm_origin")
	}
	if origin == "" {
		origin = tmux.GetParentSession()
	}
	if origin != "" && origin != session.Name {
		if err := tmux.EnsureOriginWindow(origin, session.Path, parent); err != nil {
			// Don't fall through to opening the popup / killing the picker
			// window on a half-completed switch — that's what leaves the
			// popup stacked on whatever window the client happened to be on
			// before. Surface the failure and let the user retry instead.
			p.activateErr = err.Error()
			p.render()
			return false
		}
	}

	if parent != "" {
		attachCmd := tmux.AttachCommand(session.Name, false) + " \\; select-window -t " + tmux.ShellQuote(session.WindowID)
		cmd := exec.Command("tmux", "display-popup",
			"-c", parent,
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
	return true
}

func (p *picker) killSelected() {
	list := p.filtered()
	if p.selectedIndex >= len(list) {
		return
	}
	session := list[p.selectedIndex]
	label := sessionLabel(session)

	if sessions.EffectiveState(session) != types.Idle && p.confirmStopID != session.WindowID {
		p.confirmStopID = session.WindowID
		p.confirmLabel = label
		p.notice = ""
		p.activateErr = ""
		p.render()
		return
	}
	p.confirmStopID = ""
	p.confirmLabel = ""

	if err := tmux.KillWindow(session.Name + ":" + session.WindowID); err != nil {
		p.notice = fmt.Sprintf("couldn't stop %s: %s", label, err)
		p.noticeError = true
		p.render()
		return
	}

	// If the parent session has no agent windows left, clean it up too.
	remaining := tmux.RunRaw([]string{"list-windows", "-t", session.Name, "-F", "#{@llm_agent}"})
	hasAgent := false
	if remaining.ExitCode == 0 && remaining.Stdout != "" {
		for _, line := range strings.Split(remaining.Stdout, "\n") {
			if strings.TrimSpace(line) != "" {
				hasAgent = true
				break
			}
		}
	}
	if !hasAgent {
		_ = tmux.KillSession(session.Name)
	}

	p.sessions = sessions.GetAllSessions(p.prefix)
	_ = sessions.PublishWaitingStatus(p.sessions)
	p.notice = "stopped " + label
	p.noticeError = false
	p.activateErr = ""
	next := p.filtered()
	if p.selectedIndex >= len(next) {
		p.selectedIndex = max(0, len(next)-1)
	}
	p.renderSelection()
}

// cycleAgent rotates the global @llm_active_agent to the next entry in
// the catalog and re-renders so the header reflects the new choice. Only
// affects new sessions — already-running sessions keep their agent.
func (p *picker) cycleAgent() {
	agent.Cycle()
	p.activateErr = ""
	p.confirmStopID = ""
	p.confirmLabel = ""
	p.notice = ""
	p.render()
}

func (p *picker) handleKeys(data string) (done bool) {
	keys := parseKeys(data)
	for _, key := range keys {
		switch key {
		case "\x1b[200~":
			p.isPasting = true
			continue
		case "\x1b[201~":
			p.isPasting = false
			continue
		}
		if p.isPasting && !p.isSearching {
			continue
		}
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
			p.focusPreview()
		case "o", "\x1b[13;2u", "\x1b[27;2;13~":
			if p.activateSession() {
				return true
			}
		case "/":
			p.searchStart = p.query
			p.searchIndex = p.selectedIndex
			p.isSearching = true
			p.query = ""
			p.selectedIndex = 0
			p.confirmStopID = ""
			p.confirmLabel = ""
			p.notice = ""
			p.render()
		case "a":
			p.confirmStopID = ""
			p.confirmLabel = ""
			p.openCreatePopup()
		case "n":
			p.selectNextWaiting()
		case "s":
			p.cycleAgent()
		case "\x18": // ^x
			p.killSelected()
		case "\x1b": // esc
			if p.confirmStopID != "" {
				p.confirmStopID = ""
				p.confirmLabel = ""
				p.render()
				continue
			}
			if p.query != "" {
				p.query = ""
				p.selectedIndex = 0
				p.notice = ""
				p.renderSelection()
				continue
			}
			_ = tmux.RunRaw([]string{"kill-window", "-t", windowName})
			return true
		case "\x03", "q": // ^c, q
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
	case code == 27: // esc: cancel this search and restore its previous filter
		p.query = p.searchStart
		p.isSearching = false
		p.selectedIndex = p.searchIndex
		if list := p.filtered(); len(list) > 0 {
			p.selectedIndex = min(p.selectedIndex, len(list)-1)
		}
		p.renderSelection()
	case code == 13: // enter: keep the current filter
		p.isSearching = false
		p.render()
	case code == 127 || code == 8: // backspace
		if len(p.query) > 0 {
			p.query = p.query[:len(p.query)-1]
		}
		p.selectedIndex = 0
		p.renderSelection()
	case code >= 32 && code <= 126:
		p.query += key
		p.selectedIndex = 0
		p.renderSelection()
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
	if err := cmd.Start(); err != nil {
		return
	}
	go func() {
		_ = cmd.Wait()
		select {
		case p.popupClosed <- struct{}{}:
		default:
		}
	}()
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
	bytes := make(chan byte, 64)
	go func() {
		defer close(bytes)
		for {
			b, err := reader.ReadByte()
			if err != nil {
				return
			}
			bytes <- b
		}
	}()

	for b := range bytes {
		if b != '\x1b' {
			out <- string(b)
			continue
		}

		// A terminal sends Escape both as a standalone key and as the prefix
		// for CSI sequences. File deadlines are not reliable for tmux PTYs, so
		// use a byte channel and a short timer to distinguish the two without
		// ever blocking a standalone Escape indefinitely.
		var buf strings.Builder
		buf.WriteByte(b)
		timer := time.NewTimer(20 * time.Millisecond)
	escapeSequence:
		for {
			select {
			case next, ok := <-bytes:
				if !ok {
					break escapeSequence
				}
				buf.WriteByte(next)
				if buf.Len() == 2 && next != '[' {
					break escapeSequence
				}
				// CSI sequences end at 0x40-0x7e after params/intermediates.
				if next >= 0x40 && next <= 0x7e && buf.Len() > 2 {
					break escapeSequence
				}
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(20 * time.Millisecond)
			case <-timer.C:
				break escapeSequence
			}
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
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
