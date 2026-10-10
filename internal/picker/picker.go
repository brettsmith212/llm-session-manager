package picker

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
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
	"llm-session-manager/internal/worktree"
)

const (
	windowName            = "llm-picker"
	pickerSelectionOption = "@llm_picker_selection"
	worktreeHostOption    = "@llm_worktree_host"
	previewDelay          = 75 * time.Millisecond
	refreshInterval       = 2 * time.Second
	gitRefreshInterval    = 4 * time.Second
	navigationKeyTimeout  = 500 * time.Millisecond
)

// Run starts the ANSI session picker.
func Run() error {
	prefix := tmux.GetGlobalOption("@llm_session_prefix", "llm-")
	parent := tmux.GetGlobalOption("@llm_parent", "")

	p := &picker{
		prefix:        prefix,
		parent:        parent,
		sessions:      sessions.GetAllSessions(prefix),
		query:         "",
		selectedIndex: 0,
		isSearching:   false,
		popupClosed:   make(chan struct{}, 1),
		gitUpdated:    make(chan gitRefresh, 1),
		gitByPath:     make(map[string]gitInfo),
	}
	p.refreshRoom()
	p.syncWaitingStatus()
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

	// Agent hooks signal state changes, so the poll only needs to catch what
	// produces no signal: agents exiting, stale heartbeats and relative times.
	ticker := time.NewTicker(refreshInterval)
	defer ticker.Stop()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	changed := make(chan struct{}, 1)
	go watchChanges(ctx, changed)

	input := make(chan string, 16)
	go readInput(input)

	// Paint the interactive pane before doing any tmux work for the preview.
	p.render()
	if first := p.filtered(); len(first) > 0 {
		p.updatePreview(first[0])
	}
	// The room may not be on screen yet while ListCommand switches to it, so
	// the first git refresh ignores visibility.
	p.requestGitRefresh(true)
	p.lastSnapshot = p.snapshot()

	for {
		select {
		case <-ticker.C:
			p.refresh(false)
		case <-changed:
			p.refresh(false)
		case <-p.popupClosed:
			p.refresh(true)
		case result := <-p.gitUpdated:
			p.applyGitRefresh(result)
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

// refresh reloads sessions and room state from tmux, redrawing only when
// something visible changed. force redraws unconditionally.
func (p *picker) refresh(force bool) {
	p.refreshRoom()
	p.replaceSessions(sessions.GetAllSessions(p.prefix))
	p.syncWaitingStatus()
	selectedCreated := p.selectCreatedSession()
	snap := p.snapshot()
	if force || snap != p.lastSnapshot || selectedCreated {
		p.lastSnapshot = snap
		p.render()
		if len(p.filtered()) == 0 {
			p.cancelPreview()
		}
	}
	if p.roomPanes == 1 {
		// The live pane closed, for example because its agent exited. Forget
		// the old preview so the selected agent is shown in a new pane.
		p.previewWindowID = ""
	}
	if visible := p.filtered(); !selectedCreated && p.selectedIndex < len(visible) && visible[p.selectedIndex].WindowID != p.previewWindowID {
		p.queuePreview(visible[p.selectedIndex])
	}
	p.requestGitRefresh(false)
}

// roomInfoFormat reads, in one round trip, the control room's own window and
// the options the picker displays or reacts to.
const roomInfoFormat = "#{window_id}\t#{window_panes}\t#{window_active_clients}\t#{@llm_waiting_count}\t#{@llm_picker_selection}\t#{@llm_active_agent}\t#{@llm_command}"

func (p *picker) refreshRoom() {
	target := os.Getenv("TMUX_PANE")
	if target == "" {
		target = windowName
	}
	result := tmux.RunRaw([]string{"display-message", "-p", "-t", target, roomInfoFormat})
	if result.ExitCode != 0 {
		return
	}
	parts := strings.SplitN(result.Stdout, "\t", 7)
	if len(parts) != 7 {
		return
	}
	p.ownWindowID = parts[0]
	p.roomPanes, _ = strconv.Atoi(parts[1])
	// An unreadable client count must not stop git refreshes for good.
	clients, err := strconv.Atoi(parts[2])
	p.roomVisible = err != nil || clients > 0
	p.publishedWaiting = parts[3]
	p.pendingSelection = parts[4]
	p.activeAgent = parts[5]
	if p.activeAgent == "" {
		p.activeAgent = parts[6]
	}
}

// syncWaitingStatus republishes the status-bar count when it disagrees with
// the sessions on screen, for example after a waiting agent exited without
// reporting. Hooks keep it current otherwise.
func (p *picker) syncWaitingStatus() {
	want := strconv.Itoa(sessions.CountWaiting(p.sessions))
	if p.publishedWaiting == want {
		return
	}
	if sessions.PublishWaitingStatus() == nil {
		p.publishedWaiting = want
	}
}

// watchChanges wakes the picker whenever an agent hook signals a state change.
func watchChanges(ctx context.Context, changed chan<- struct{}) {
	for ctx.Err() == nil {
		result := tmux.RunContext(ctx, []string{"wait-for", sessions.ChangedChannel})
		if ctx.Err() != nil {
			return
		}
		if result.ExitCode != 0 {
			// The regular poll keeps the room current until tmux answers again.
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
			}
			continue
		}
		select {
		case changed <- struct{}{}:
		default:
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
	isEditingLabel    bool
	editLabelID       string
	editLabelValue    string
	editLabelCursor   int
	activateErr       string // set when activateSession fails to reach the origin window
	confirmStopID     string
	confirmLabel      string
	confirmWorktree   string
	notice            string
	noticeError       bool
	popupClosed       chan struct{}
	previewTimer      *time.Timer
	pendingPreview    types.Session
	hasPendingPreview bool
	previewWindowID   string
	gitUpdated        chan gitRefresh
	gitByPath         map[string]gitInfo
	gitRefreshing     bool
	lastGitRefresh    time.Time
	pendingGUntil     time.Time
	lastSnapshot      string
	pathKeys          map[string]string // session path → gitPathKey, reset on each refresh

	// Read from tmux on every refresh by refreshRoom.
	ownWindowID      string
	roomPanes        int
	roomVisible      bool
	publishedWaiting string
	pendingSelection string
	activeAgent      string
}

func (p *picker) filtered() []types.Session {
	terms := strings.Fields(strings.ToLower(p.query))
	// Sort keys are computed once per session rather than inside the
	// comparator, which runs O(n log n) times on every keypress.
	type entry struct {
		session  types.Session
		section  int
		state    int
		order    repositoryOrder
		family   familyRecencyKey
		checkout checkoutRecencyKey
	}
	entries := make([]entry, 0, len(p.sessions))
	for _, s := range p.sessions {
		info := p.gitByPath[p.pathKey(s.Path)]
		if len(terms) > 0 {
			_, stateLabel, _ := statePresentation(sessionState(s))
			haystack := strings.ToLower(strings.Join([]string{
				s.Path, s.Name, s.WindowName, s.Label, info.branch, stateLabel}, " "))
			if !containsAll(haystack, terms) {
				continue
			}
		}
		e := entry{session: s, section: sectionRank(s), state: stateRank(sessionState(s))}
		// A session without git metadata gets the zero gitInfo and sorts by path.
		e.order = repositoryOrderFor(s.Path, info)
		e.family = familyRecencyKey{section: e.section, family: e.order.family}
		e.checkout = checkoutRecencyKey{familyRecencyKey: e.family, path: e.order.path}
		entries = append(entries, e)
	}
	familyRecency := make(map[familyRecencyKey]int64)
	checkoutRecency := make(map[checkoutRecencyKey]int64)
	for _, e := range entries {
		if e.session.StateAt > familyRecency[e.family] {
			familyRecency[e.family] = e.session.StateAt
		}
		if e.session.StateAt > checkoutRecency[e.checkout] {
			checkoutRecency[e.checkout] = e.session.StateAt
		}
	}
	sort.SliceStable(entries, func(i, j int) bool {
		left, right := entries[i], entries[j]
		if left.section != right.section {
			return left.section < right.section
		}
		if familyRecency[left.family] != familyRecency[right.family] {
			return familyRecency[left.family] > familyRecency[right.family]
		}
		if left.order.family != right.order.family {
			return left.order.family < right.order.family
		}
		if checkoutRecency[left.checkout] != checkoutRecency[right.checkout] {
			return checkoutRecency[left.checkout] > checkoutRecency[right.checkout]
		}
		if left.order.linkedWorktree != right.order.linkedWorktree {
			return !left.order.linkedWorktree
		}
		if left.order.checkout != right.order.checkout {
			return left.order.checkout < right.order.checkout
		}
		if left.order.path != right.order.path {
			return left.order.path < right.order.path
		}
		// Sessions sharing a checkout directory sort by most-recent state
		// change first, so the agent that changed state last surfaces above
		// its older siblings regardless of tmux window index. This applies
		// within every section, including PINNED, where it keeps project
		// groups contiguous rather than splitting them by attention rank.
		if left.session.StateAt != right.session.StateAt {
			return left.session.StateAt > right.session.StateAt
		}
		// Attention rank is only a tiebreaker for sessions with identical
		// timestamps. Non-pinned sections already group by state via
		// sectionRank, so this only matters inside the mixed-state PINNED
		// section when two pinned sessions in the same directory share a
		// StateAt value.
		if left.state != right.state {
			return left.state < right.state
		}
		if left.session.Name != right.session.Name {
			return left.session.Name < right.session.Name
		}
		if left.session.WindowIndex != right.session.WindowIndex {
			return left.session.WindowIndex < right.session.WindowIndex
		}
		return left.session.WindowID < right.session.WindowID
	})
	out := make([]types.Session, len(entries))
	for i, e := range entries {
		out[i] = e.session
	}
	return out
}

func containsAll(haystack string, terms []string) bool {
	for _, term := range terms {
		if !strings.Contains(haystack, term) {
			return false
		}
	}
	return true
}

// pathKey memoizes gitPathKey, which resolves symlinks with a syscall per path
// component, for the current set of sessions.
func (p *picker) pathKey(path string) string {
	if path == "" {
		return ""
	}
	if key, ok := p.pathKeys[path]; ok {
		return key
	}
	key := gitPathKey(path)
	if p.pathKeys == nil {
		p.pathKeys = make(map[string]string)
	}
	p.pathKeys[path] = key
	return key
}

type familyRecencyKey struct {
	section int
	family  string
}

type checkoutRecencyKey struct {
	familyRecencyKey
	path string
}

type repositoryOrder struct {
	family         string
	linkedWorktree bool
	checkout       string
	path           string
}

func repositoryOrderFor(sessionPath string, info gitInfo) repositoryOrder {
	path := strings.ToLower(filepath.Clean(sessionPath))
	order := repositoryOrder{family: path, checkout: path, path: path}
	if info.commonDir == "" {
		return order
	}

	order.family = strings.ToLower(filepath.Clean(info.commonDir))
	order.linkedWorktree = info.linkedWorktree
	if !info.linkedWorktree {
		order.checkout = ""
		return order
	}
	if info.worktreeLabel != "" {
		order.checkout = strings.ToLower(info.worktreeLabel)
	} else if info.branch != "" {
		order.checkout = strings.ToLower(info.branch)
	} else if info.checkoutPath != "" {
		order.checkout = strings.ToLower(filepath.Base(info.checkoutPath))
	}
	return order
}

func stateRank(state types.State) int {
	switch state {
	case types.Waiting:
		return 0
	case types.Idle:
		return 2
	default:
		return 1
	}
}

// sectionRank assigns each session to a control-room section. Pinned sessions
// form their own top section regardless of state; the remaining sessions fall
// into the needs-you / active / idle sections by attention rank.
func sectionRank(session types.Session) int {
	if session.Pinned {
		return 0
	}
	return stateRank(sessionState(session)) + 1
}

func sessionState(session types.Session) types.State {
	if types.IsState(string(session.DisplayState)) {
		return session.DisplayState
	}
	return sessions.EffectiveState(session)
}

func (p *picker) selectedWindowID() string {
	list := p.filtered()
	if p.selectedIndex < 0 || p.selectedIndex >= len(list) {
		return ""
	}
	return list[p.selectedIndex].WindowID
}

func (p *picker) replaceSessions(next []types.Session) {
	selectedID := p.selectedWindowID()
	// Re-resolve paths on every refresh so a changed symlink is picked up.
	p.pathKeys = nil
	for i := range next {
		if !types.IsState(string(next[i].DisplayState)) {
			next[i].DisplayState = sessions.EffectiveState(next[i])
		}
	}
	p.sessions = next
	p.restoreSelectedWindow(selectedID)
}

func (p *picker) restoreSelectedWindow(selectedID string) {
	list := p.filtered()
	if selectedID != "" {
		for i, session := range list {
			if session.WindowID == selectedID {
				p.selectedIndex = i
				return
			}
		}
	}
	p.selectedIndex = max(0, min(p.selectedIndex, len(list)-1))
}

// requestGitRefresh starts a background git inspection of every session's
// checkout. Unless force is set it is rate limited and skipped while no client
// is looking at the room, since a backgrounded room would otherwise run git
// in every project all day.
func (p *picker) requestGitRefresh(force bool) {
	if p.gitUpdated == nil || p.gitRefreshing {
		return
	}
	if !force && (!p.roomVisible || time.Since(p.lastGitRefresh) < gitRefreshInterval) {
		return
	}
	seen := make(map[string]bool)
	paths := make([]string, 0, len(p.sessions))
	for _, session := range p.sessions {
		key := p.pathKey(session.Path)
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		paths = append(paths, session.Path)
	}
	p.gitRefreshing = true
	go func() {
		p.gitUpdated <- collectGitInfo(paths)
	}()
}

// applyGitRefresh installs fresh git metadata. Checkouts whose inspection
// timed out keep their previous metadata, so a busy machine does not make
// sessions jump in and out of their repository groups.
func (p *picker) applyGitRefresh(result gitRefresh) {
	for key := range result.failed {
		if previous, ok := p.gitByPath[key]; ok {
			result.info[key] = previous
		}
	}
	p.gitRefreshing = false
	p.lastGitRefresh = time.Now()
	if reflect.DeepEqual(p.gitByPath, result.info) {
		return
	}
	selectedID := p.selectedWindowID()
	p.gitByPath = result.info
	p.restoreSelectedWindow(selectedID)
	p.render()
}

type pathActivity struct {
	total   int
	working int
}

func (p *picker) pathActivity() map[string]pathActivity {
	activity := make(map[string]pathActivity)
	for _, session := range p.sessions {
		if key := p.pathKey(session.Path); key != "" {
			value := activity[key]
			value.total++
			if sessionState(session) == types.Working {
				value.working++
			}
			activity[key] = value
		}
	}
	return activity
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

// snapshot summarizes everything the list displays. It includes the current
// minute so relative times such as "5m" keep advancing on an idle screen.
func (p *picker) snapshot() string {
	parts := make([]string, len(p.sessions)+1)
	parts[0] = fmt.Sprintf("%d:%s", time.Now().Unix()/60, p.activeAgent)
	for i, s := range p.sessions {
		parts[i+1] = fmt.Sprintf("%s:%s:%d:%s:%s:%d:%s:%s:%s:%t", s.Name, s.WindowID, s.WindowIndex, s.State, sessionState(s), s.StateAt, s.Path, s.WindowName, s.Label, s.Pinned)
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
		if sessionState(session) == types.Waiting {
			waiting++
		}
	}

	header := fmt.Sprintf(" %s%s◆ Agents%s  %s%d/%d%s",
		ansi.Foreground(ansi.Blue), ansi.Bold, ansi.Reset,
		ansi.Foreground(ansi.Overlay2), position, len(list), ansi.Reset)
	if waiting > 0 {
		header += fmt.Sprintf("  %s◆ %d needs you%s", ansi.Foreground(ansi.Yellow), waiting, ansi.Reset)
	}
	active := p.activeAgent
	if active == "" {
		header += fmt.Sprintf("  %sagent %snot configured%s", ansi.Foreground(ansi.Overlay0), ansi.Foreground(ansi.Red), ansi.Reset)
	} else {
		header += fmt.Sprintf("  %sagent%s %s%s%s %s▾%s",
			ansi.Foreground(ansi.Overlay0), ansi.Reset,
			ansi.Foreground(agent.BadgeColor(active)), active, ansi.Reset,
			ansi.Foreground(ansi.Surface2), ansi.Reset)
	}
	frame.lineBg(1, header, ansi.Surface0)

	dividerRow := 3
	row := 4
	if p.isEditingLabel {
		editorContext := "selected session"
		for _, session := range p.sessions {
			if session.WindowID != p.editLabelID {
				continue
			}
			agentName := session.WindowName
			if agentName == "" {
				agentName = "agent"
			}
			editorContext = fmt.Sprintf("%s · %s", projectName(session), agentName)
			break
		}
		before := p.editLabelValue[:p.editLabelCursor]
		after := p.editLabelValue[p.editLabelCursor:]
		frame.lineBg(2, fmt.Sprintf("  %s%s✎ EDIT TASK LABEL%s  %s%s%s",
			ansi.Foreground(ansi.Blue), ansi.Bold, ansi.Reset,
			ansi.Foreground(ansi.Text), editorContext, ansi.Reset), ansi.Surface1)
		frame.lineBg(3, fmt.Sprintf("  %s%sTask ›%s %s%s%s█%s%s%s",
			ansi.Foreground(ansi.Mauve), ansi.Bold, ansi.Reset,
			ansi.Foreground(ansi.Text), before, ansi.Foreground(ansi.Blue),
			ansi.Foreground(ansi.Text), after, ansi.Reset), ansi.Surface1)
		frame.lineBg(4, fmt.Sprintf("  %sEnter save · Esc cancel · ^U clear%s",
			ansi.Foreground(ansi.Subtext0), ansi.Reset), ansi.Surface1)
		dividerRow = 5
		row = 6
	} else if p.isSearching {
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
		frame.line(2, fmt.Sprintf("  %s↑↓%s %snav · %s⏎%s %slive · %so%s %spopup · %sr%s %sreview · %s/%s %sfind%s",
			ansi.Foreground(ansi.Surface2), ansi.Reset, ansi.Foreground(ansi.Overlay0),
			ansi.Foreground(ansi.Surface2), ansi.Reset, ansi.Foreground(ansi.Overlay0),
			ansi.Foreground(ansi.Surface2), ansi.Reset, ansi.Foreground(ansi.Overlay0),
			ansi.Foreground(ansi.Surface2), ansi.Reset, ansi.Foreground(ansi.Overlay0),
			ansi.Foreground(ansi.Surface2), ansi.Reset, ansi.Foreground(ansi.Overlay0), ansi.Reset))
	}
	frame.line(dividerRow, fmt.Sprintf("  %s%s%s", ansi.Foreground(ansi.Surface0), strings.Repeat("─", max(0, cols-4)), ansi.Reset))

	if p.confirmWorktree != "" {
		const confirmationSuffix = "?  D confirm · esc cancel · branch kept"
		labelWidth := max(1, cols-utf8.RuneCountInString("  remove "+confirmationSuffix))
		label := truncateVisible(filepath.Base(p.confirmWorktree), "", "", labelWidth)
		frame.line(row, fmt.Sprintf("  %sremove %s%s%s",
			ansi.Foreground(ansi.Yellow), label, confirmationSuffix, ansi.Reset))
		row++
	} else if p.confirmStopID != "" {
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
	} else if strings.HasPrefix(p.notice, "created ") {
		frame.lineBg(row, fmt.Sprintf("  %s%s✓ CREATED%s  %s%s%s",
			ansi.Foreground(ansi.Green), ansi.Bold, ansi.Reset,
			ansi.Foreground(ansi.Text), strings.TrimPrefix(p.notice, "created "), ansi.Reset), ansi.Surface0)
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
		managedWorktree := p.isManagedWorktree(list)
		footer := "  a add · A worktree · e label · n needs you · p pin · s agent · ^x stop · q close"
		if managedWorktree {
			footer = "  a add · A worktree · D cleanup · e label · n needs you · p pin · ^x stop · q close"
		}
		if cols < 60 {
			footer = "  a add · A tree · e label · n next · p pin · ^x stop · q"
			if managedWorktree {
				footer = "  a add · A tree · D clean · n next · p pin · ^x stop · q"
			}
		}
		frame.line(footerRow, fmt.Sprintf("%s%s%s", ansi.Foreground(ansi.Overlay0), footer, ansi.Reset))
	} else {
		footerRow = rows + 1
	}

	availableRows := max(0, footerRow-row)
	listRows := p.buildListRows(list)
	visibleRows := visibleListRows(listRows, p.selectedIndex, availableRows)
	if len(visibleRows) == 0 {
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

	for _, listRow := range visibleRows {
		if row >= footerRow {
			break
		}
		indent := strings.Repeat(" ", max(0, listRow.indent))
		switch listRow.kind {
		case listRowSection:
			text := "  ── " + listRow.text + " "
			text += strings.Repeat("─", max(0, cols-utf8.RuneCountInString(text)-2))
			frame.line(row, fmt.Sprintf("%s%s%s%s", ansi.Foreground(listRow.color), ansi.Bold, text, ansi.Reset))
			row++
		case listRowSpacer:
			row++
		case listRowRepository:
			frame.line(row, fmt.Sprintf("%s%s%s%s%s", indent, ansi.Foreground(ansi.Blue), ansi.Bold, listRow.text, ansi.Reset))
			row++
		case listRowProject:
			frame.line(row, fmt.Sprintf("%s%s%s%s", indent, ansi.Foreground(listRow.color), listRow.text, ansi.Reset))
			row++
		case listRowGit:
			frame.line(row, fmt.Sprintf("%s%s%s%s", indent, ansi.Foreground(ansi.Overlay0), listRow.text, ansi.Reset))
			row++
		case listRowWarning:
			frame.line(row, fmt.Sprintf("%s%s⚠ %s%s", indent, ansi.Foreground(ansi.Yellow), listRow.text, ansi.Reset))
			row++
		case listRowSession:
			row = drawItem(frame, list[listRow.sessionIndex], cols, listRow.sessionIndex == p.selectedIndex, row, listRow.isLast, listRow.indent)
		}
	}
	frame.flush()
}

type listRowKind int

const (
	listRowSection listRowKind = iota
	listRowSpacer
	listRowRepository
	listRowProject
	listRowGit
	listRowWarning
	listRowSession
)

type pickerListRow struct {
	kind         listRowKind
	text         string
	color        ansi.RGB
	sessionIndex int
	isLast       bool
	indent       int
}

func (p *picker) buildListRows(list []types.Session) []pickerListRow {
	if len(list) == 0 {
		return nil
	}
	// sectionCounts indexes by sectionRank: 0=pinned, 1=needs you,
	// 2=active, 3=idle.
	sectionCounts := [4]int{}
	for _, session := range list {
		sectionCounts[sectionRank(session)]++
	}
	activity := p.pathActivity()
	// groupWorking counts working agents per (project, section) so the
	// shared-checkout warning fires whenever a project group within a single
	// section has more than one actively working agent — even in the PINNED
	// section, where a non-working session may lead a mixed-state group.
	groupWorking := make(map[string]int)
	for _, session := range list {
		if sessionState(session) != types.Working {
			continue
		}
		key := p.pathKey(session.Path)
		if key == "" {
			key = session.Name
		}
		groupWorking[key+"\x00"+strconv.Itoa(sectionRank(session))]++
	}
	rows := make([]pickerListRow, 0, len(list)*2)
	previousRank := -1
	previousFamily := ""
	previousProject := ""
	for i, session := range list {
		rank := sectionRank(session)
		if rank != previousRank {
			if previousRank != -1 {
				rows = append(rows, pickerListRow{kind: listRowSpacer})
			}
			name := "ACTIVE"
			color := ansi.Blue
			switch rank {
			case 0:
				name = "PINNED"
				color = ansi.Mauve
			case 1:
				name = "NEEDS YOU"
				color = ansi.Yellow
			case 3:
				name = "IDLE"
				color = ansi.Subtext0
			}
			rows = append(rows, pickerListRow{kind: listRowSection, text: fmt.Sprintf("%s · %d", name, sectionCounts[rank]), color: color})
			previousRank = rank
			previousFamily = ""
			previousProject = ""
		}

		projectKey := p.pathKey(session.Path)
		if projectKey == "" {
			projectKey = session.Name
		}
		info := p.gitByPath[projectKey]
		familyKey := projectKey
		nestedCheckout := info.valid && info.commonDir != ""
		if nestedCheckout {
			familyKey = strings.ToLower(filepath.Clean(info.commonDir))
		}
		if familyKey != previousFamily {
			if nestedCheckout {
				rows = append(rows, pickerListRow{
					kind:   listRowRepository,
					text:   repositoryHeading(info, session),
					indent: 2,
				})
			}
			previousFamily = familyKey
			previousProject = ""
		}

		projectIndent := 2
		detailIndent := 2
		projectColor := ansi.Blue
		if nestedCheckout {
			projectIndent = 4
			detailIndent = 6
			projectColor = ansi.Subtext0
		}
		if projectKey != previousProject {
			path := sessions.FormatPath(session.Path)
			if path == "" {
				path = session.Name
			}
			if nestedCheckout {
				path = "primary checkout"
				if info.linkedWorktree {
					projectColor = ansi.Mauve
					path = "↳ worktree · " + worktreeHeading(info, session)
				}
			}
			rows = append(rows, pickerListRow{kind: listRowProject, text: path, color: projectColor, indent: projectIndent})
			metadata := formatGitInfo(info)
			if shared := activity[projectKey]; shared.total > 1 {
				sharedText := fmt.Sprintf("shared checkout · %d agents", shared.total)
				if metadata == "" {
					metadata = sharedText
				} else {
					metadata += " · " + sharedText
				}
			}
			if metadata != "" {
				rows = append(rows, pickerListRow{kind: listRowGit, text: metadata, indent: detailIndent})
			}
			// Warn once per project group when more than one agent in this
			// specific (project, section) group is actively working. Using the
			// per-group count rather than the leading session's state keeps this
			// correct inside the PINNED section, where a non-working session may
			// lead a group that contains actively working agents.
			if working := groupWorking[projectKey+"\x00"+strconv.Itoa(rank)]; working > 1 {
				rows = append(rows, pickerListRow{kind: listRowWarning, text: fmt.Sprintf("%d agents are active in this checkout", working), indent: detailIndent})
			}
			previousProject = projectKey
		}

		nextSameProject := false
		if i+1 < len(list) {
			next := list[i+1]
			nextKey := p.pathKey(next.Path)
			if nextKey == "" {
				nextKey = next.Name
			}
			nextSameProject = sectionRank(next) == rank && nextKey == projectKey
		}
		rows = append(rows, pickerListRow{kind: listRowSession, sessionIndex: i, isLast: !nextSameProject, indent: detailIndent})
	}
	return rows
}

func repositoryHeading(info gitInfo, session types.Session) string {
	if filepath.Base(info.commonDir) == ".git" {
		return sessions.FormatPath(filepath.Dir(info.commonDir))
	}
	if info.repository != "" {
		return info.repository
	}
	if info.checkoutPath != "" {
		return sessions.FormatPath(info.checkoutPath)
	}
	return sessions.FormatPath(session.Path)
}

func worktreeHeading(info gitInfo, session types.Session) string {
	if info.worktreeLabel != "" {
		return info.worktreeLabel
	}
	if info.checkoutPath != "" {
		return filepath.Base(info.checkoutPath)
	}
	if info.branch != "" {
		return info.branch
	}
	return filepath.Base(session.Path)
}

func visibleListRows(rows []pickerListRow, selected, maxRows int) []pickerListRow {
	if len(rows) == 0 || maxRows < 1 {
		return nil
	}
	selectedRow := 0
	for i, row := range rows {
		if row.kind == listRowSession && row.sessionIndex == selected {
			selectedRow = i
			break
		}
	}
	start := max(0, selectedRow-maxRows/2)
	end := min(len(rows), start+maxRows)
	start = max(0, end-maxRows)
	return rows[start:end]
}

func drawItem(frame *screenFrame, session types.Session, cols int, selected bool, row int, isLastInGroup bool, indent int) int {
	state := sessionState(session)
	stateSymbol, stateLabel, stateColor := statePresentation(state)
	ago := sessions.FormatAgo(session.StateAt)
	agentName := session.WindowName
	if agentName == "" {
		agentName = "?"
	}

	connector := "├─"
	if isLastInGroup {
		connector = "└─"
	}
	if selected {
		connector = "▸ "
	}

	statePadded := fmt.Sprintf("%-9s", stateLabel)
	prefixWidth := indent + 15 // indent + connector + symbol + padded state + separator
	agentWidth := min(16, max(0, cols-prefixWidth-2))
	agentName = truncateVisible(agentName, "", "", agentWidth)
	badge := ""
	if agentName != "" {
		badge = formatBadge(agentName, agent.BadgeColor(session.WindowName))
	}
	suffixWidth := utf8.RuneCountInString(agentName)
	if agentName != "" {
		suffixWidth += 2
	}
	detailWidth := max(0, cols-prefixWidth-suffixWidth)
	detail := strings.TrimSpace(session.Label)
	unnamed := detail == "" && selected
	if detail == "" {
		if selected {
			detail = "unnamed task"
			if detailWidth >= utf8.RuneCountInString(detail)+utf8.RuneCountInString(ago)+3 {
				detail += "  " + ago
			}
		} else {
			detail = ago
		}
	} else if detailWidth >= utf8.RuneCountInString(detail)+utf8.RuneCountInString(ago)+3 {
		detail += "  " + ago
	}
	detail = truncateVisible(detail, "", "", detailWidth)
	padding := strings.Repeat(" ", max(0, detailWidth-utf8.RuneCountInString(detail)))
	separator := ""
	if agentName != "" {
		separator = "  "
	}

	if selected {
		accent := ansi.Foreground(ansi.Blue)
		dot := ansi.Foreground(stateColor)
		txt := ansi.Foreground(ansi.Text)
		muted := ansi.Foreground(ansi.Overlay2)
		detailColor := txt
		if unnamed {
			detailColor = muted
		}

		line1 := strings.Repeat(" ", indent) +
			accent + connector + " " +
			dot + stateSymbol + " " + txt + statePadded + " " +
			detailColor + detail + muted + padding + separator +
			badge + ansi.Background(ansi.Surface0)

		frame.lineBg(row, line1, ansi.Surface0)
	} else {
		tree := ansi.Foreground(ansi.Blue)
		dot := ansi.Foreground(stateColor)
		txt := ansi.Foreground(ansi.Subtext0)
		muted := ansi.Foreground(ansi.Overlay0)

		line1 := strings.Repeat(" ", indent) +
			tree + connector + ansi.Reset + " " +
			dot + stateSymbol + " " + txt + statePadded + " " + ansi.Reset +
			txt + detail + muted + padding + separator + ansi.Reset +
			badge + ansi.Reset

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
		return "·", "idle", ansi.Subtext0
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
	p.setPreviewTitle(session)
	p.previewWindowID = session.WindowID
}

func (p *picker) setPreviewTitle(session types.Session) {
	// Refresh the live pane title with the name + window index so its border
	// always tracks what is shown there and advertises the outer-tmux escape.
	title := fmt.Sprintf("LIVE AGENT · %s", projectName(session))
	if session.Label != "" {
		title += " · " + session.Label
	}
	title += fmt.Sprintf(" · %s · prefix u returns", session.WindowName)
	_ = tmux.RunRaw([]string{"select-pane", "-T",
		title,
		"-t", ":" + windowName + ".1"})
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
	p.confirmWorktree = ""
	p.notice = ""
	list := p.filtered()
	p.selectedIndex = max(0, min(len(list)-1, p.selectedIndex+delta))
	p.renderSelection()
}

func (p *picker) jumpSelection(index int) {
	list := p.filtered()
	if len(list) == 0 {
		return
	}
	p.changeSelection(index - p.selectedIndex)
}

func (p *picker) selectNextWaiting() {
	p.confirmStopID = ""
	p.confirmLabel = ""
	p.confirmWorktree = ""
	currentID := ""
	filtered := p.filtered()
	if p.selectedIndex < len(filtered) {
		currentID = filtered[p.selectedIndex].WindowID
	}

	hasWaiting := false
	for _, session := range p.sessions {
		if sessionState(session) == types.Waiting {
			hasWaiting = true
			break
		}
	}
	if !hasWaiting {
		p.notice = "no sessions need attention"
		p.noticeError = false
		p.render()
		return
	}

	// Attention navigation is intentionally global. A committed project or
	// agent filter should never hide a session that needs the user.
	p.query = ""
	list := p.filtered()
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
		if sessionState(list[index]) != types.Waiting {
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
	if session.Label != "" {
		return fmt.Sprintf("%s · %s · %s", projectName(session), session.Label, agentName)
	}
	return fmt.Sprintf("%s · %s", projectName(session), agentName)
}

// selectCreatedSession consumes the window ID left by the create prompt and
// moves the picker and live preview to that exact new session.
func (p *picker) selectCreatedSession() bool {
	windowID := p.pendingSelection
	if windowID == "" {
		return false
	}
	p.pendingSelection = ""
	var created types.Session
	found := false
	for _, session := range p.sessions {
		if session.WindowID == windowID {
			created = session
			found = true
			break
		}
	}
	if !found {
		// Add sets this option only after marking the new window as managed, so
		// a missing window is stale (for example, the agent exited immediately).
		_ = tmux.SetWindowOption(windowName, pickerSelectionOption, "")
		return false
	}

	p.query = ""
	for i, session := range p.filtered() {
		if session.WindowID != created.WindowID {
			continue
		}
		p.selectedIndex = i
		p.activateErr = ""
		p.confirmStopID = ""
		p.confirmLabel = ""
		p.confirmWorktree = ""
		action := "name task"
		if created.Label != "" {
			action = "edit task"
		}
		p.notice = fmt.Sprintf("created e %s · Enter live · o popup", action)
		p.noticeError = false
		_ = tmux.SetWindowOption(windowName, pickerSelectionOption, "")
		p.queuePreview(session)
		return true
	}
	return false
}

func (p *picker) sessionOrigin(session types.Session) string {
	origin := session.Origin
	if origin == "" {
		origin = tmux.GetSessionOption(session.Name, "@llm_origin")
	}
	if origin == "" {
		origin = tmux.GetParentSession()
	}
	return origin
}

// parentClient returns the client the user is driving this control room from.
// @llm_parent is shared by every room on the server, so with several
// terminals attached it may name another terminal; a client currently showing
// this room's window is preferred.
func (p *picker) parentClient() string {
	preferred := tmux.GetGlobalOption("@llm_parent", p.parent)
	if p.ownWindowID == "" {
		return preferred
	}
	viewer := ""
	for _, client := range tmux.ListClients() {
		if client.WindowID != p.ownWindowID {
			continue
		}
		if client.Client == preferred {
			return preferred
		}
		if viewer == "" {
			viewer = client.Client
		}
	}
	if viewer != "" {
		return viewer
	}
	return preferred
}

func (p *picker) switchToProject(session types.Session, parent string) bool {
	if parent == "" {
		p.activateErr = "no parent tmux client is available"
		p.render()
		return false
	}
	if session.Path == "" {
		p.activateErr = "no project path is available"
		p.render()
		return false
	}
	origin := p.sessionOrigin(session)
	if origin == "" || origin == session.Name {
		p.activateErr = "no project origin is available"
		p.render()
		return false
	}
	if strings.HasPrefix(origin, "@") {
		if resolved, err := tmux.DisplayMessage("#{session_name}", origin); err == nil {
			origin = resolved
		}
	}
	if origin == "" || !tmux.HasSession(origin) {
		p.activateErr = "project origin is no longer available"
		p.render()
		return false
	}
	host, err := tmux.EnsureOriginWindowTarget(origin, session.Path, parent)
	if err != nil {
		p.activateErr = err.Error()
		p.render()
		return false
	}
	if host.Created {
		if manifest, err := worktree.Load(session.Path); err == nil {
			_ = tmux.SetWindowOption(host.ID, worktreeHostOption, manifest.WorktreePath)
		}
	}
	p.activateErr = ""
	return true
}

func (p *picker) reviewProject() {
	list := p.filtered()
	if p.selectedIndex >= len(list) {
		return
	}
	p.switchToProject(list[p.selectedIndex], p.parentClient())
}

// activateSession switches the parent client to the selected project's
// review window and opens the agent over it while the Control Room remains
// alive in the background.
func (p *picker) activateSession() {
	list := p.filtered()
	if p.selectedIndex >= len(list) {
		return
	}
	session := list[p.selectedIndex]
	parent := p.parentClient()
	if !p.switchToProject(session, parent) {
		return
	}
	width := tmux.GetGlobalOption("@llm_popup_width", "90%")
	height := tmux.GetGlobalOption("@llm_popup_height", "90%")
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
	if err := cmd.Start(); err != nil {
		p.activateErr = "couldn't open popup: " + err.Error()
		p.render()
		return
	}
	go func() { _ = cmd.Wait() }()
}

func (p *picker) selectedIsManagedWorktree() bool {
	return p.isManagedWorktree(p.filtered())
}

// isManagedWorktree reports whether the selected entry of list is an
// llmux-created worktree. Git metadata already records manifest ownership,
// so the manifest is only read before the first git refresh.
func (p *picker) isManagedWorktree(list []types.Session) bool {
	if p.selectedIndex < 0 || p.selectedIndex >= len(list) {
		return false
	}
	path := list[p.selectedIndex].Path
	if info, ok := p.gitByPath[p.pathKey(path)]; ok && info.valid {
		return info.managedWorktree
	}
	_, err := worktree.Load(path)
	return err == nil
}

// cleanupSelectedWorktree stops every agent sharing the selected isolated
// checkout, removes only llmux-created host windows, and asks Git to remove the
// clean worktree. Its branch is always retained.
func (p *picker) cleanupSelectedWorktree() {
	list := p.filtered()
	if p.selectedIndex < 0 || p.selectedIndex >= len(list) {
		return
	}
	session := list[p.selectedIndex]
	manifest, err := worktree.ValidateRemoval(session.Path)
	if err != nil {
		p.confirmWorktree = ""
		p.notice = err.Error()
		p.noticeError = true
		p.render()
		return
	}
	if err := p.validateWorktreePanes(session.Name, manifest.WorktreePath); err != nil {
		p.confirmWorktree = ""
		p.notice = err.Error()
		p.noticeError = true
		p.render()
		return
	}
	if p.confirmWorktree != manifest.WorktreePath {
		p.confirmWorktree = manifest.WorktreePath
		p.confirmStopID = ""
		p.confirmLabel = ""
		p.notice = ""
		p.activateErr = ""
		p.render()
		return
	}
	p.confirmWorktree = ""

	if tmux.HasSession(session.Name) {
		if err := tmux.KillSession(session.Name); err != nil {
			p.notice = "couldn't stop worktree agents: " + err.Error()
			p.noticeError = true
			p.render()
			return
		}
	}
	p.killManagedWorktreeHosts(manifest.WorktreePath)
	removed, err := worktree.Remove(manifest.WorktreePath)
	if err != nil {
		p.notice = "agents stopped, but worktree cleanup failed: " + err.Error()
		p.noticeError = true
		p.replaceSessions(sessions.GetAllSessions(p.prefix))
		p.renderSelection()
		return
	}

	p.replaceSessions(sessions.GetAllSessions(p.prefix))
	p.syncWaitingStatus()
	p.notice = "removed isolated worktree; branch " + removed.Branch + " kept"
	p.noticeError = false
	p.activateErr = ""
	if p.selectedIndex >= len(p.filtered()) {
		p.selectedIndex = max(0, len(p.filtered())-1)
	}
	p.renderSelection()
}

func (p *picker) validateWorktreePanes(agentSession, path string) error {
	result := tmux.RunRaw([]string{"list-panes", "-a", "-F",
		"#{session_name}\t#{window_id}\t#{session_windows}\t#{pane_current_path}\t#{@llm_worktree_host}"})
	if result.ExitCode != 0 {
		return fmt.Errorf("couldn't inspect tmux panes before cleanup: %s", result.Stderr)
	}
	targetPath := gitPathKey(path)
	for _, line := range strings.Split(result.Stdout, "\n") {
		parts := strings.SplitN(line, "\t", 5)
		// A shell or editor anywhere inside the checkout would be left in a
		// deleted directory, not only one sitting at its root.
		if len(parts) != 5 || !worktree.Within(parts[3], path) {
			continue
		}
		if parts[0] == agentSession {
			continue
		}
		if parts[2] != "1" && gitPathKey(parts[4]) == targetPath {
			continue
		}
		return fmt.Errorf("worktree is open in tmux window %s; close it before cleanup", parts[1])
	}
	return nil
}

func (p *picker) killManagedWorktreeHosts(path string) {
	result := tmux.RunRaw([]string{"list-windows", "-a", "-F", "#{window_id}\t#{session_windows}\t#{@llm_worktree_host}"})
	if result.ExitCode != 0 {
		return
	}
	targetPath := gitPathKey(path)
	for _, line := range strings.Split(result.Stdout, "\n") {
		parts := strings.SplitN(line, "\t", 3)
		// Never let task cleanup kill an entire user session. Under ordinary
		// operation the origin also contains the Control Room or another
		// project window; a one-window edge case is safer left for the user.
		if len(parts) != 3 || parts[1] == "1" || gitPathKey(parts[2]) != targetPath {
			continue
		}
		_ = tmux.KillWindow(parts[0])
	}
}

func (p *picker) killSelected() {
	list := p.filtered()
	if p.selectedIndex >= len(list) {
		return
	}
	session := list[p.selectedIndex]
	label := sessionLabel(session)

	if sessionState(session) != types.Idle && p.confirmStopID != session.WindowID {
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

	p.replaceSessions(sessions.GetAllSessions(p.prefix))
	p.syncWaitingStatus()
	p.notice = "stopped " + label
	p.noticeError = false
	p.activateErr = ""
	next := p.filtered()
	if p.selectedIndex >= len(next) {
		p.selectedIndex = max(0, len(next)-1)
	}
	p.renderSelection()
}

func (p *picker) beginLabelEdit(session types.Session) {
	p.isEditingLabel = true
	p.editLabelID = session.WindowID
	p.editLabelValue = session.Label
	p.editLabelCursor = len(session.Label)
	p.activateErr = ""
	p.confirmStopID = ""
	p.confirmLabel = ""
	p.confirmWorktree = ""
	p.notice = ""
}

func (p *picker) editSelectedLabel() {
	list := p.filtered()
	if p.selectedIndex >= len(list) {
		return
	}
	p.beginLabelEdit(list[p.selectedIndex])
	p.render()
}

func (p *picker) cancelLabelEdit() {
	p.isEditingLabel = false
	p.editLabelID = ""
	p.editLabelValue = ""
	p.editLabelCursor = 0
	p.render()
}

func (p *picker) saveLabelEdit() {
	label := strings.TrimSpace(p.editLabelValue)
	if err := tmux.SetWindowOption(p.editLabelID, "@llm_label", label); err != nil {
		p.notice = "couldn't save task label: " + err.Error()
		p.noticeError = true
		p.render()
		return
	}
	selectedID := p.editLabelID
	var updated types.Session
	for i := range p.sessions {
		if p.sessions[i].WindowID == selectedID {
			p.sessions[i].Label = label
			updated = p.sessions[i]
			break
		}
	}
	p.isEditingLabel = false
	p.editLabelID = ""
	p.editLabelValue = ""
	p.editLabelCursor = 0
	p.notice = "task label updated"
	if label == "" {
		p.notice = "task label cleared"
	}
	p.noticeError = false

	list := p.filtered()
	selected := false
	for i, session := range list {
		if session.WindowID == selectedID {
			p.selectedIndex = i
			selected = true
			break
		}
	}
	if !selected {
		p.query = ""
		for i, session := range p.filtered() {
			if session.WindowID == selectedID {
				p.selectedIndex = i
				break
			}
		}
	}
	if updated.WindowID != "" && p.previewWindowID == updated.WindowID {
		p.setPreviewTitle(updated)
	}
	p.renderSelection()
}

func (p *picker) handleLabelKey(key string) {
	if len(key) == 0 {
		return
	}
	code := key[0]
	switch {
	case key == "\x1b":
		p.cancelLabelEdit()
		return
	case code == 13:
		p.saveLabelEdit()
		return
	case code == 21: // ctrl-u
		p.editLabelValue = ""
		p.editLabelCursor = 0
	case code == 127 || code == 8:
		if p.editLabelCursor > 0 {
			_, size := utf8.DecodeLastRuneInString(p.editLabelValue[:p.editLabelCursor])
			start := p.editLabelCursor - size
			p.editLabelValue = p.editLabelValue[:start] + p.editLabelValue[p.editLabelCursor:]
			p.editLabelCursor = start
		}
	case key == "\x1b[3~":
		if p.editLabelCursor < len(p.editLabelValue) {
			_, size := utf8.DecodeRuneInString(p.editLabelValue[p.editLabelCursor:])
			p.editLabelValue = p.editLabelValue[:p.editLabelCursor] + p.editLabelValue[p.editLabelCursor+size:]
		}
	case key == "\x1b[D":
		if p.editLabelCursor > 0 {
			_, size := utf8.DecodeLastRuneInString(p.editLabelValue[:p.editLabelCursor])
			p.editLabelCursor -= size
		}
	case key == "\x1b[C":
		if p.editLabelCursor < len(p.editLabelValue) {
			_, size := utf8.DecodeRuneInString(p.editLabelValue[p.editLabelCursor:])
			p.editLabelCursor += size
		}
	case key == "\x1b[H" || key == "\x1b[1~":
		p.editLabelCursor = 0
	case key == "\x1b[F" || key == "\x1b[4~":
		p.editLabelCursor = len(p.editLabelValue)
	case code >= 32 && code <= 126 && len(p.editLabelValue) < 120:
		p.editLabelValue = p.editLabelValue[:p.editLabelCursor] + key + p.editLabelValue[p.editLabelCursor:]
		p.editLabelCursor += len(key)
	}
	p.render()
}

// cycleAgent rotates the global @llm_active_agent to the next entry in
// the catalog and re-renders so the header reflects the new choice. Only
// affects new sessions — already-running sessions keep their agent.
func (p *picker) cycleAgent() {
	p.activeAgent = agent.Cycle()
	p.activateErr = ""
	p.confirmStopID = ""
	p.confirmLabel = ""
	p.confirmWorktree = ""
	p.notice = ""
	p.render()
}

// togglePin pins or unpins the selected session. Pin state is persisted in
// the @llm_pinned window option so it survives picker restarts and tmux
// refreshes; the in-memory copy is updated so the re-render is immediate.
func (p *picker) togglePin() {
	list := p.filtered()
	if p.selectedIndex < 0 || p.selectedIndex >= len(list) {
		return
	}
	session := list[p.selectedIndex]
	selectedID := session.WindowID

	// Persist first. Only flip the in-memory flag after tmux confirms the
	// option change so the UI can never disagree with the durable state.
	if session.Pinned {
		if err := tmux.UnsetWindowOption(selectedID, "@llm_pinned"); err != nil {
			p.notice = "couldn't unpin: " + err.Error()
			p.noticeError = true
			p.render()
			return
		}
	} else {
		if err := tmux.SetWindowOption(selectedID, "@llm_pinned", "1"); err != nil {
			p.notice = "couldn't pin: " + err.Error()
			p.noticeError = true
			p.render()
			return
		}
	}
	for i := range p.sessions {
		if p.sessions[i].WindowID == selectedID {
			p.sessions[i].Pinned = !p.sessions[i].Pinned
			break
		}
	}
	p.restoreSelectedWindow(selectedID)
	p.activateErr = ""
	p.confirmStopID = ""
	p.confirmLabel = ""
	p.confirmWorktree = ""
	p.notice = ""
	p.renderSelection()
}

func (p *picker) handleKeys(data string) (done bool) {
	keys := parseKeys(data)
	for _, key := range keys {
		switch key {
		case "\x1b[200~":
			p.pendingGUntil = time.Time{}
			p.isPasting = true
			continue
		case "\x1b[201~":
			p.pendingGUntil = time.Time{}
			p.isPasting = false
			continue
		}
		if p.isPasting && !p.isSearching && !p.isEditingLabel {
			continue
		}
		if p.isEditingLabel {
			p.pendingGUntil = time.Time{}
			p.handleLabelKey(key)
			continue
		}
		if p.isSearching {
			p.pendingGUntil = time.Time{}
			done := p.handleSearchKey(key)
			if done {
				return true
			}
			continue
		}
		if key == "g" {
			if p.confirmStopID != "" || p.confirmWorktree != "" {
				p.pendingGUntil = time.Time{}
				continue
			}
			now := time.Now()
			if !p.pendingGUntil.IsZero() && now.Before(p.pendingGUntil) {
				p.pendingGUntil = time.Time{}
				p.jumpSelection(0)
			} else {
				p.pendingGUntil = now.Add(navigationKeyTimeout)
			}
			continue
		}
		p.pendingGUntil = time.Time{}
		if key == "G" {
			if p.confirmStopID == "" && p.confirmWorktree == "" {
				p.jumpSelection(len(p.filtered()) - 1)
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
			p.activateSession()
		case "r":
			p.reviewProject()
		case "/":
			p.searchStart = p.query
			p.searchIndex = p.selectedIndex
			p.isSearching = true
			p.query = ""
			p.selectedIndex = 0
			p.confirmStopID = ""
			p.confirmLabel = ""
			p.confirmWorktree = ""
			p.notice = ""
			p.render()
		case "a":
			p.confirmStopID = ""
			p.confirmLabel = ""
			p.confirmWorktree = ""
			p.openCreatePopup()
		case "A":
			p.confirmStopID = ""
			p.confirmLabel = ""
			p.confirmWorktree = ""
			p.openWorktreeCreatePopup()
		case "D":
			p.cleanupSelectedWorktree()
		case "e":
			p.editSelectedLabel()
		case "n":
			p.selectNextWaiting()
		case "s":
			p.cycleAgent()
		case "p":
			p.togglePin()
		case "\x18": // ^x
			p.confirmWorktree = ""
			p.killSelected()
		case "\x1b": // esc
			if p.confirmStopID != "" || p.confirmWorktree != "" {
				p.confirmStopID = ""
				p.confirmLabel = ""
				p.confirmWorktree = ""
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
	defaultPath := p.createDefaultPath()
	p.startCreatePopup(tmux.ShellQuote(binaryPath())+" prompt "+tmux.ShellQuote(defaultPath), "30%")
}

func (p *picker) openWorktreeCreatePopup() {
	defaultPath := p.createDefaultPath()
	worktreeBase := tmux.GetGlobalOption(worktree.TmuxBaseOption, "")
	if _, err := worktree.Inspect(defaultPath, worktreeBase); err != nil {
		p.notice = err.Error()
		p.noticeError = true
		p.render()
		return
	}
	p.startCreatePopup(tmux.ShellQuote(binaryPath())+" worktree-prompt "+tmux.ShellQuote(defaultPath), "55%")
}

func (p *picker) createDefaultPath() string {
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
	return defaultPath
}

func (p *picker) startCreatePopup(command, height string) {
	width := tmux.GetGlobalOption("@llm_popup_width", "90%")
	args := []string{"display-popup", "-w", width, "-h", height, "-E", command}
	if parent := p.parentClient(); parent != "" {
		// The new agent records its origin from @llm_parent, so point it at the
		// terminal this room is shown on before the prompt runs.
		_ = tmux.SetGlobalOption("@llm_parent", parent)
		args = append([]string{"display-popup", "-c", parent}, args[1:]...)
	}
	cmd := exec.Command("tmux", args...)
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
