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
	// seenDelay is how long the live pane must show a finished agent, while
	// the room is on screen, before its result counts as seen.
	seenDelay            = 2 * time.Second
	refreshInterval      = 2 * time.Second
	gitRefreshInterval   = 4 * time.Second
	navigationKeyTimeout = 500 * time.Millisecond
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
	p.previewTimer = stoppedTimer()
	defer p.previewTimer.Stop()
	p.seenTimer = stoppedTimer()
	defer p.seenTimer.Stop()

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
		case <-p.seenTimer.C:
			p.markPreviewSeen()
		case keys := <-input:
			if done := p.handleKeys(keys); done {
				return nil
			}
		}
	}
}

func stoppedTimer() *time.Timer {
	timer := time.NewTimer(time.Hour)
	if !timer.Stop() {
		<-timer.C
	}
	return timer
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
	p.syncPreviewBorder()
	p.requestGitRefresh(false)
}

// syncPreviewBorder keeps the live pane's title and state color current when
// the agent it shows changes state or gains a reason.
func (p *picker) syncPreviewBorder() {
	if p.previewWindowID == "" {
		return
	}
	for _, session := range p.sessions {
		if session.WindowID == p.previewWindowID {
			p.setPreviewTitle(session)
			return
		}
	}
}

// roomInfoFormat reads, in one round trip, the control room's own window and
// the options the picker displays or reacts to.
const roomInfoFormat = "#{window_id}\t#{window_panes}\t#{window_active_clients}\t#{@llm_waiting_count}\t#{@llm_picker_selection}\t#{@llm_active_agent}\t#{@llm_command}\t#{@llm_done_count}\t#{@llm_theme}"

const roomInfoFields = 9

func (p *picker) refreshRoom() {
	target := os.Getenv("TMUX_PANE")
	if target == "" {
		target = windowName
	}
	result := tmux.RunRaw([]string{"display-message", "-p", "-t", target, roomInfoFormat})
	if result.ExitCode != 0 {
		return
	}
	parts := strings.SplitN(result.Stdout, "\t", roomInfoFields)
	if len(parts) != roomInfoFields {
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
	p.publishedDone = parts[7]
	if p.theme.Name == "" || p.themeOption != parts[8] {
		p.themeOption = parts[8]
		p.theme = ansi.ThemeNamed(parts[8])
	}
}

// syncWaitingStatus republishes the status-bar count when it disagrees with
// the sessions on screen, for example after a waiting agent exited without
// reporting. Hooks keep it current otherwise.
func (p *picker) syncWaitingStatus() {
	waiting := strconv.Itoa(sessions.CountWaiting(p.sessions))
	done := strconv.Itoa(sessions.CountDone(p.sessions))
	if p.publishedWaiting == waiting && (p.publishedDone == done || p.publishedDone == "" && done == "0") {
		return
	}
	if sessions.PublishWaitingStatus() == nil {
		p.publishedWaiting, p.publishedDone = waiting, done
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
	showHelp          bool
	seenTimer         *time.Timer
	seenWindowID      string // the finished agent the seen timer is watching
	previewTitleText  string // last title written to the live pane
	previewStateText  string // last state written to the live pane border
	theme             ansi.Theme
	themeOption       string

	// Read from tmux on every refresh by refreshRoom.
	ownWindowID      string
	roomPanes        int
	roomVisible      bool
	publishedWaiting string
	publishedDone    string
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
			_, stateLabel, _ := presentation(p.palette(), s)
			haystack := strings.ToLower(strings.Join([]string{
				s.Path, s.Name, s.WindowName, s.Label, s.AutoLabel, s.Reason, info.branch, stateLabel}, " "))
			if !containsAll(haystack, terms) {
				continue
			}
		}
		e := entry{session: s, section: sectionRank(s), state: attentionRank(s)}
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

// attentionRank orders states by how much they need the user. It only
// breaks ties inside the mixed-state Pinned section.
func attentionRank(session types.Session) int {
	switch sessionState(session) {
	case types.Waiting, stateExited:
		return 0
	case types.Working:
		return 1
	case types.Idle:
		if session.Unseen {
			return 2
		}
		return 3
	default:
		return 1
	}
}

// sectionRank assigns each session to a control-room section. Pinned sessions
// form their own top section regardless of state; the rest fall into Needs
// You (waiting or exited), Active, Done (finished, not yet viewed) and Idle.
func sectionRank(session types.Session) int {
	if session.Pinned {
		return sectionPinned
	}
	switch sessionState(session) {
	case types.Waiting, stateExited:
		return sectionNeedsYou
	case types.Idle:
		if session.Unseen {
			return sectionDone
		}
		return sectionIdle
	case types.Working:
		return sectionActive
	}
	// An agent that has not reported yet is starting up.
	return sectionActive
}

func sessionState(session types.Session) types.State {
	if session.Exited {
		return stateExited
	}
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

// snapshot summarizes everything the list displays. It includes the current
// minute so relative times such as "5m" keep advancing on an idle screen.
func (p *picker) snapshot() string {
	parts := make([]string, len(p.sessions)+1)
	parts[0] = fmt.Sprintf("%d:%s:%s", time.Now().Unix()/60, p.activeAgent, p.theme.Name)
	for i, s := range p.sessions {
		parts[i+1] = fmt.Sprintf("%s:%s:%d:%s:%s:%d:%s:%s:%s:%t:%s:%s:%s:%t:%d", s.Name, s.WindowID, s.WindowIndex, s.State, sessionState(s), s.StateAt, s.Path, s.WindowName, s.Label, s.Pinned,
			s.AutoLabel, s.ReasonKind, s.Reason, s.Unseen, s.ExitStatus)
	}
	return strings.Join(parts, "|")
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
	p.previewTitleText, p.previewStateText = "", ""
	p.setPreviewTitle(session)
	p.previewWindowID = session.WindowID
	p.watchSeen(session)
}

// setPreviewTitle names the agent in the live pane's border and records its
// state there, so the border can take the state's color. Both are written
// only when they change.
func (p *picker) setPreviewTitle(session types.Session) {
	title, state := liveTitle(session), previewState(session)
	if title == p.previewTitleText && state == p.previewStateText {
		return
	}
	target := ":" + windowName + ".1"
	if tmux.RunRaw(tmux.Batch(
		[]string{"select-pane", "-T", title, "-t", target},
		[]string{"set-option", "-p", "-t", target, "@llm_preview_state", state},
	)).ExitCode == 0 {
		p.previewTitleText, p.previewStateText = title, state
	}
}

// watchSeen starts the seen timer when the live pane begins showing a
// finished agent whose result has not been viewed.
func (p *picker) watchSeen(session types.Session) {
	if p.seenTimer == nil {
		return
	}
	if !p.seenTimer.Stop() {
		select {
		case <-p.seenTimer.C:
		default:
		}
	}
	p.seenWindowID = ""
	if session.Unseen {
		p.seenWindowID = session.WindowID
		p.seenTimer.Reset(seenDelay)
	}
}

// markPreviewSeen clears the finished mark of the agent the live pane has
// been showing, provided someone is looking at the room.
func (p *picker) markPreviewSeen() {
	if p.seenWindowID == "" || p.seenWindowID != p.previewWindowID || !p.roomVisible {
		return
	}
	p.markSeen(p.seenWindowID)
}

// markSeen clears a session's finished-but-unseen mark and republishes the
// status-bar counts.
func (p *picker) markSeen(windowID string) {
	for i := range p.sessions {
		if p.sessions[i].WindowID != windowID || !p.sessions[i].Unseen {
			continue
		}
		if err := tmux.UnsetWindowOption(windowID, "@llm_unseen"); err != nil {
			return
		}
		p.sessions[i].Unseen = false
		if p.seenWindowID == windowID {
			p.seenWindowID = ""
		}
		_ = sessions.PublishWaitingStatus()
		p.publishedDone = strconv.Itoa(sessions.CountDone(p.sessions))
		selectedID := p.selectedWindowID()
		p.restoreSelectedWindow(selectedID)
		p.lastSnapshot = p.snapshot()
		p.render()
		if windowID == p.previewWindowID {
			p.setPreviewTitle(p.sessions[i])
		}
		return
	}
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
	p.markSeen(list[p.selectedIndex].WindowID)
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

// selectNextAttention moves to the next agent that needs the user, or, when
// none does, to the next finished result not yet viewed. It is global: a
// committed filter never hides an agent that needs the user.
func (p *picker) selectNextAttention() {
	p.confirmStopID = ""
	p.confirmLabel = ""
	p.confirmWorktree = ""
	currentID := p.selectedWindowID()

	needsYou := func(session types.Session) bool {
		return sectionRank(session) == sectionNeedsYou || session.Pinned && attentionRank(session) == 0
	}
	finished := func(session types.Session) bool { return session.Unseen }
	for _, wanted := range []func(types.Session) bool{needsYou, finished} {
		found := false
		for _, session := range p.sessions {
			if wanted(session) {
				found = true
				break
			}
		}
		if !found {
			continue
		}
		p.query = ""
		list := p.filtered()
		start := -1
		for i, session := range list {
			if session.WindowID == currentID {
				start = i
				break
			}
		}
		for offset := 1; offset <= len(list); offset++ {
			index := (start + offset + len(list)) % len(list)
			if !wanted(list[index]) {
				continue
			}
			p.selectedIndex = index
			p.notice = ""
			p.activateErr = ""
			p.renderSelection()
			return
		}
	}
	p.notice = "nothing needs you"
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
	if title := session.Title(); title != "" {
		return fmt.Sprintf("%s · %s · %s", projectName(session), title, agentName)
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
	p.markSeen(session.WindowID)
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

	if state := sessionState(session); state != types.Idle && state != stateExited && p.confirmStopID != session.WindowID {
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
	// The automatic name is a starting point; saving it makes it the label.
	p.editLabelValue = session.Title()
	p.editLabelCursor = len(p.editLabelValue)
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
		if p.showHelp {
			// Any key closes the help overlay.
			p.pendingGUntil = time.Time{}
			p.showHelp = false
			p.render()
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
			p.selectNextAttention()
		case "d":
			p.reviewDiff()
		case "R":
			p.restartSelected()
		case "?":
			p.showHelp = true
			p.confirmStopID = ""
			p.confirmLabel = ""
			p.confirmWorktree = ""
			p.render()
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
	p.startPopup([]string{"-w", width, "-h", height}, command)
}

// startPopup opens a popup on the terminal showing this room and refreshes
// the room when it closes.
func (p *picker) startPopup(options []string, command string) {
	args := append([]string{"display-popup"}, options...)
	if parent := p.parentClient(); parent != "" {
		// A new agent records its origin from @llm_parent, so point it at the
		// terminal this room is shown on before the popup runs.
		_ = tmux.SetGlobalOption("@llm_parent", parent)
		args = append(args, "-c", parent)
	}
	args = append(args, "-E", command)
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

// reviewCommandOption names the command `d` runs to review a checkout.
const reviewCommandOption = "@llm_review_command"

// reviewDiff opens the configured diff viewer in a popup rooted at the
// selected checkout. LLMUX_BASE is empty for an ordinary checkout, where the
// review covers uncommitted work, and is the starting commit of an llmux
// worktree, so the agent's commits are reviewed as well.
func (p *picker) reviewDiff() {
	list := p.filtered()
	if p.selectedIndex >= len(list) {
		return
	}
	session := list[p.selectedIndex]
	info, ok := p.gitByPath[p.pathKey(session.Path)]
	if !ok || !info.valid {
		info = inspectGit(session.Path)
	}
	if !info.valid {
		p.notice = "d reviews Git checkouts; this agent is not in one"
		p.noticeError = true
		p.render()
		return
	}
	base := info.baseCommit
	if base == "" {
		if manifest, err := worktree.Load(session.Path); err == nil {
			base = manifest.BaseCommit
		}
	}
	directory := session.Path
	if info.checkoutPath != "" {
		directory = info.checkoutPath
	}
	width := tmux.GetGlobalOption("@llm_popup_width", "90%")
	height := tmux.GetGlobalOption("@llm_popup_height", "90%")
	p.startPopup([]string{"-w", width, "-h", height, "-d", directory,
		"-e", "LLMUX_BASE=" + base, "-e", "LLMUX_PATH=" + directory,
		"-T", " " + projectName(session) + " · diff "}, reviewCommand(tmux.GetGlobalOption(reviewCommandOption, "")))
	p.markSeen(session.WindowID)
}

// reviewCommand returns the configured review command, or hunk when it is
// installed, or a paged git diff.
func reviewCommand(configured string) string {
	if strings.TrimSpace(configured) != "" {
		return configured
	}
	if _, err := exec.LookPath("hunk"); err == nil {
		return "hunk diff $LLMUX_BASE --watch"
	}
	return "git -c color.ui=always diff ${LLMUX_BASE:-HEAD} | less -R"
}

// restartSelected restarts an agent that exited, resuming its conversation
// when the agent reported one llmux knows how to resume.
func (p *picker) restartSelected() {
	list := p.filtered()
	if p.selectedIndex >= len(list) {
		return
	}
	session := list[p.selectedIndex]
	if sessionState(session) != stateExited {
		p.notice = "R restarts agents that exited; this one is still running"
		p.noticeError = false
		p.render()
		return
	}
	args := []string{"respawn-pane", "-k", "-t", session.WindowID}
	if command := resumeCommand(session); command != "" {
		args = append(args, command)
	}
	now := strconv.FormatInt(time.Now().Unix(), 10)
	_, err := tmux.Run(tmux.Batch(
		args,
		tmux.SetWindowOptionArgs(session.WindowID, "@llm_state", string(types.Idle)),
		tmux.SetWindowOptionArgs(session.WindowID, "@llm_state_at", now),
		[]string{"set-window-option", "-u", "-t", session.WindowID, "@llm_reason_kind"},
		[]string{"set-window-option", "-u", "-t", session.WindowID, "@llm_reason"},
	))
	if err != nil {
		p.notice = "couldn't restart " + sessionLabel(session) + ": " + err.Error()
		p.noticeError = true
		p.render()
		return
	}
	p.notice = "restarted " + sessionLabel(session)
	if resumeCommand(session) != "" {
		p.notice = "resumed " + sessionLabel(session)
	}
	p.noticeError = false
	p.previewWindowID = ""
	p.replaceSessions(sessions.GetAllSessions(p.prefix))
	p.syncWaitingStatus()
	p.renderSelection()
}

// resumeCommand rebuilds an exited agent's command so it continues the
// conversation it reported, or returns "" to rerun the original command.
func resumeCommand(session types.Session) string {
	if session.AgentSession == "" || session.StartCommand == "" {
		return ""
	}
	id := tmux.ShellQuote(session.AgentSession)
	fields := strings.Fields(session.StartCommand)
	switch agent.Name(session.StartCommand) {
	case "claude":
		// A pane restarted before already carries a resume flag; replace it.
		kept := fields[:1]
		for i := 1; i < len(fields); i++ {
			switch fields[i] {
			case "--resume", "-r":
				i++
			case "--continue", "-c":
			default:
				kept = append(kept, fields[i])
			}
		}
		return strings.Join(append(kept, "--resume", id), " ")
	case "codex":
		rest := fields[1:]
		if len(rest) > 0 && rest[0] == "resume" {
			rest = rest[1:]
			if len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
				rest = rest[1:]
			}
		}
		return agent.LaunchCommand(strings.Join(append([]string{fields[0], "resume", id}, rest...), " "))
	}
	return ""
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
