package worktree

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCreateIsolatesChangesAndSafeRemoveKeepsBranch(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	repository := createTestRepository(t)

	info, err := Inspect(repository, "")
	if err != nil {
		t.Fatal(err)
	}
	plan, err := NewPlan(info, "Try Nixvim Upgrade")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(plan.Path, WorktreeRoot("")+string(filepath.Separator)) {
		t.Fatalf("worktree path %q is not under %q", plan.Path, WorktreeRoot(""))
	}
	if plan.Branch != "llmux/try-nixvim-upgrade" {
		t.Fatalf("branch = %q", plan.Branch)
	}
	if err := Create(plan); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = discardCreated(plan) })

	if got := readTestFile(t, filepath.Join(plan.Path, "config.nix")); got != "committed\n" {
		t.Fatalf("worktree content = %q", got)
	}
	if err := os.WriteFile(filepath.Join(plan.Path, "config.nix"), []byte("isolated\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := readTestFile(t, filepath.Join(repository, "config.nix")); got != "committed\n" {
		t.Fatalf("main checkout was modified: %q", got)
	}
	if _, err := ValidateRemoval(plan.Path); err == nil || !strings.Contains(err.Error(), "uncommitted") {
		t.Fatalf("dirty removal error = %v", err)
	}

	runGitTest(t, plan.Path, "add", "config.nix")
	runGitTest(t, plan.Path, "commit", "-m", "isolated change")
	manifest, err := Remove(plan.Path)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Branch != plan.Branch {
		t.Fatalf("removed branch = %q", manifest.Branch)
	}
	if _, err := os.Stat(plan.Path); !os.IsNotExist(err) {
		t.Fatalf("worktree still exists: %v", err)
	}
	if output := runGitTest(t, repository, "branch", "--list", plan.Branch); !strings.Contains(output, plan.Branch) {
		t.Fatalf("cleanup deleted retained branch: %q", output)
	}
	if _, err := Load(plan.Path); err == nil {
		t.Fatal("manifest still exists after cleanup")
	}
}

func TestCreateStartsFromCommittedHeadAndAvoidsCollisions(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	repository := createTestRepository(t)
	if err := os.WriteFile(filepath.Join(repository, "config.nix"), []byte("uncommitted\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	worktreeBase := t.TempDir()
	info, err := Inspect(repository, worktreeBase)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Dirty {
		t.Fatal("dirty source checkout was not reported")
	}
	first, err := NewPlan(info, "Darwin Settings")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(first.Path, WorktreeRoot(worktreeBase)+string(filepath.Separator)) {
		t.Fatalf("custom worktree path %q is not under %q", first.Path, WorktreeRoot(worktreeBase))
	}
	if err := Create(first); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = discardCreated(first) })
	if got := readTestFile(t, filepath.Join(first.Path, "config.nix")); got != "committed\n" {
		t.Fatalf("worktree included source checkout changes: %q", got)
	}

	second, err := NewPlan(info, "Darwin Settings")
	if err != nil {
		t.Fatal(err)
	}
	if second.Slug != "darwin-settings-2" || second.Branch != "llmux/darwin-settings-2" {
		t.Fatalf("collision plan = %#v", second)
	}
}

func TestRemovePrunesManifestForMissingWorktree(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	repository := createTestRepository(t)
	info, err := Inspect(repository, "")
	if err != nil {
		t.Fatal(err)
	}
	plan, err := NewPlan(info, "Missing Checkout")
	if err != nil {
		t.Fatal(err)
	}
	if err := Create(plan); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(plan.Path); err != nil {
		t.Fatal(err)
	}

	removed, err := Remove(plan.Path)
	if err != nil {
		t.Fatal(err)
	}
	if removed.Branch != plan.Branch {
		t.Fatalf("removed branch = %q", removed.Branch)
	}
	manifests, err := List()
	if err != nil {
		t.Fatal(err)
	}
	if len(manifests) != 0 {
		t.Fatalf("stale manifests remain: %#v", manifests)
	}
	if output := runGitTest(t, repository, "branch", "--list", plan.Branch); !strings.Contains(output, plan.Branch) {
		t.Fatalf("stale cleanup deleted retained branch: %q", output)
	}
}

func TestSlug(t *testing.T) {
	tests := map[string]string{
		"  Nixvim: 2026 Upgrade!  ": "nixvim-2026-upgrade",
		"---":                       "task",
		"Home Manager / Darwin":     "home-manager-darwin",
	}
	for input, want := range tests {
		if got := Slug(input); got != want {
			t.Errorf("Slug(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestWorktreeRootUsesOptionalBaseDirectory(t *testing.T) {
	dataHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataHome)
	if got, want := WorktreeRoot(""), filepath.Join(dataHome, "llmux", "worktrees"); got != want {
		t.Fatalf("default worktree root = %q, want %q", got, want)
	}

	home := t.TempDir()
	t.Setenv("HOME", home)
	if got, want := WorktreeRoot("~/Developer"), filepath.Join(home, "Developer", "llmux", "worktrees"); got != want {
		t.Fatalf("configured worktree root = %q, want %q", got, want)
	}
	if got, want := WorktreeRoot("Developer"), filepath.Join(home, "Developer", "llmux", "worktrees"); got != want {
		t.Fatalf("home-relative worktree root = %q, want %q", got, want)
	}
}

func createTestRepository(t *testing.T) string {
	t.Helper()
	repository := t.TempDir()
	runGitTest(t, repository, "init", "-b", "main")
	runGitTest(t, repository, "config", "user.name", "llmux test")
	runGitTest(t, repository, "config", "user.email", "llmux@example.test")
	if err := os.WriteFile(filepath.Join(repository, "config.nix"), []byte("committed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, repository, "add", "config.nix")
	runGitTest(t, repository, "commit", "-m", "initial")
	return repository
}

func runGitTest(t *testing.T, directory string, args ...string) string {
	t.Helper()
	commandArgs := append([]string{"-C", directory}, args...)
	output, err := exec.Command("git", commandArgs...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
	return strings.TrimSpace(string(output))
}

func readTestFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
