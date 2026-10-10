package sessions

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"llm-session-manager/internal/types"
)

func TestSessionNameForPathIgnoresSpellingOfTheSameDirectory(t *testing.T) {
	project := filepath.Join(t.TempDir(), "project")
	if err := os.Mkdir(project, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(project, link); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", filepath.Dir(project))

	want := SessionNameForPath(project, "llm-")
	for _, spelling := range []string{project + "/", project + "/./", link, "~/project"} {
		if got := SessionNameForPath(spelling, "llm-"); got != want {
			t.Errorf("SessionNameForPath(%q) = %q, want %q", spelling, got, want)
		}
	}
	if other := SessionNameForPath(filepath.Dir(project), "llm-"); other == want {
		t.Fatal("different directories share a session name")
	}
}

func TestEffectiveStateUsesLatestReport(t *testing.T) {
	now := time.Now().Unix()
	stale := now - StaleSeconds - 10
	busy := types.Session{State: types.Working, StateAt: stale, SeenAt: now}
	if got := EffectiveState(busy); got != types.Working {
		t.Fatalf("agent still reporting = %s, want working", got)
	}
	silent := types.Session{State: types.Working, StateAt: stale, SeenAt: stale}
	if got := EffectiveState(silent); got != types.Idle {
		t.Fatalf("agent that stopped reporting = %s, want idle", got)
	}
	legacy := types.Session{State: types.Working, StateAt: stale}
	if got := EffectiveState(legacy); got != types.Idle {
		t.Fatalf("window without a heartbeat = %s, want idle", got)
	}
}

func TestFormatPathOnlyShortensTheHomeDirectory(t *testing.T) {
	t.Setenv("HOME", "/Users/brett")
	for path, want := range map[string]string{
		"/Users/brett":             "~",
		"/Users/brett/project":     "~/project",
		"/Users/brettsmith/notes":  "/Users/brettsmith/notes",
		"/opt/Users/brett/project": "/opt/Users/brett/project",
	} {
		if got := FormatPath(path); got != want {
			t.Errorf("FormatPath(%q) = %q, want %q", path, got, want)
		}
	}
}
