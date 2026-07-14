package worktree

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"
)

const manifestVersion = 1

// Repository describes the Git checkout used as the source for a new
// isolated worktree.
type Repository struct {
	CheckoutPath string
	CommonDir    string
	Name         string
	Branch       string
	Head         string
	Dirty        bool
	StorageDir   string
}

// Plan is a fully-resolved, collision-free worktree creation plan.
type Plan struct {
	Repository Repository
	Label      string
	Slug       string
	Branch     string
	Path       string
}

// Manifest records an llmux-owned worktree independently from tmux so it can
// still be found and safely removed after the tmux server exits.
type Manifest struct {
	Version      int       `json:"version"`
	Label        string    `json:"label"`
	Repository   string    `json:"repository"`
	CommonDir    string    `json:"common_dir"`
	WorktreePath string    `json:"worktree_path"`
	Branch       string    `json:"branch"`
	BaseCommit   string    `json:"base_commit"`
	CreatedAt    time.Time `json:"created_at"`
}

// Inspect resolves the repository and committed base represented by path.
// Uncommitted source-checkout changes are reported but intentionally excluded
// from worktree creation.
func Inspect(path string) (Repository, error) {
	checkout, err := gitOutputAt(path, "rev-parse", "--show-toplevel")
	if err != nil {
		return Repository{}, fmt.Errorf("not a Git working tree: %s", path)
	}
	checkout, err = normalizePath(checkout)
	if err != nil {
		return Repository{}, err
	}

	commonDir, err := gitOutputAt(checkout, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		commonDir, err = gitOutputAt(checkout, "rev-parse", "--git-common-dir")
		if err != nil {
			return Repository{}, fmt.Errorf("couldn't resolve Git common directory: %w", err)
		}
		if !filepath.IsAbs(commonDir) {
			commonDir = filepath.Join(checkout, commonDir)
		}
	}
	commonDir, err = normalizePath(commonDir)
	if err != nil {
		return Repository{}, err
	}

	head, err := gitOutputAt(checkout, "rev-parse", "HEAD")
	if err != nil {
		return Repository{}, errors.New("the repository has no commits to use as a worktree base")
	}
	branch, err := gitOutputAt(checkout, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil || branch == "" {
		shortHead := head
		if len(shortHead) > 7 {
			shortHead = shortHead[:7]
		}
		branch = "detached@" + shortHead
	}
	status, err := gitOutputAt(checkout, "status", "--porcelain", "--untracked-files=normal")
	if err != nil {
		return Repository{}, fmt.Errorf("couldn't inspect Git status: %w", err)
	}

	name := filepath.Base(filepath.Dir(commonDir))
	if filepath.Base(commonDir) != ".git" || name == "." || name == string(filepath.Separator) {
		name = filepath.Base(checkout)
	}
	storageDir := filepath.Join(WorktreeRoot(), repoStorageName(name, commonDir))
	return Repository{
		CheckoutPath: checkout,
		CommonDir:    commonDir,
		Name:         name,
		Branch:       branch,
		Head:         head,
		Dirty:        status != "",
		StorageDir:   storageDir,
	}, nil
}

// NewPlan builds a collision-free branch and destination for label.
func NewPlan(repository Repository, label string) (Plan, error) {
	label = strings.TrimSpace(label)
	if label == "" {
		return Plan{}, errors.New("enter a task name")
	}
	if repository.CommonDir == "" || repository.Head == "" || repository.StorageDir == "" {
		return Plan{}, errors.New("invalid worktree repository")
	}

	baseSlug := Slug(label)
	for suffix := 1; suffix < 10_000; suffix++ {
		slug := baseSlug
		if suffix > 1 {
			slug = fmt.Sprintf("%s-%d", baseSlug, suffix)
		}
		branch := "llmux/" + slug
		path := filepath.Join(repository.StorageDir, slug)
		if pathExists(path) || manifestExists(path) || branchExists(repository.CommonDir, branch) {
			continue
		}
		return Plan{
			Repository: repository,
			Label:      label,
			Slug:       slug,
			Branch:     branch,
			Path:       path,
		}, nil
	}
	return Plan{}, errors.New("couldn't find an available worktree name")
}

// Create checks out the plan's base commit into its isolated directory and records
// ownership in an XDG manifest. If manifest persistence fails, the just-created
// checkout and branch are rolled back.
func Create(plan Plan) error {
	if err := os.MkdirAll(filepath.Dir(plan.Path), 0o755); err != nil {
		return fmt.Errorf("couldn't create worktree directory: %w", err)
	}
	if _, err := gitOutputAt(plan.Repository.CheckoutPath,
		"worktree", "add", "-b", plan.Branch, plan.Path, plan.Repository.Head); err != nil {
		removeEmptyDir(filepath.Dir(plan.Path))
		return fmt.Errorf("couldn't create Git worktree: %w", err)
	}

	manifest := Manifest{
		Version:      manifestVersion,
		Label:        plan.Label,
		Repository:   plan.Repository.Name,
		CommonDir:    plan.Repository.CommonDir,
		WorktreePath: plan.Path,
		Branch:       plan.Branch,
		BaseCommit:   plan.Repository.Head,
		CreatedAt:    time.Now(),
	}
	if err := writeManifest(manifest); err != nil {
		_ = discardCreated(plan)
		return err
	}
	return nil
}

// DiscardCreated removes a newly-created worktree and its branch. It is only
// intended for rolling back a failed agent launch immediately after Create.
func DiscardCreated(plan Plan) error {
	if _, err := Load(plan.Path); err != nil {
		return err
	}
	return discardCreated(plan)
}

func discardCreated(plan Plan) error {
	var errs []error
	if _, err := gitOutputWithDir(plan.Repository.CommonDir,
		"worktree", "remove", "--force", plan.Path); err != nil {
		errs = append(errs, err)
	}
	if _, err := gitOutputWithDir(plan.Repository.CommonDir,
		"branch", "-D", plan.Branch); err != nil {
		errs = append(errs, err)
	}
	if err := os.Remove(manifestPath(plan.Path)); err != nil && !errors.Is(err, os.ErrNotExist) {
		errs = append(errs, err)
	}
	removeEmptyDir(filepath.Dir(plan.Path))
	return errors.Join(errs...)
}

// ValidateRemoval verifies that path is an llmux-owned, clean worktree. It
// never modifies the worktree.
func ValidateRemoval(path string) (Manifest, error) {
	manifest, err := Load(path)
	if err != nil {
		return Manifest{}, err
	}
	if _, err := os.Stat(manifest.WorktreePath); err != nil {
		return Manifest{}, fmt.Errorf("worktree directory is unavailable: %w", err)
	}
	status, err := gitOutputAt(manifest.WorktreePath,
		"status", "--porcelain", "--untracked-files=normal")
	if err != nil {
		return Manifest{}, fmt.Errorf("couldn't inspect worktree status: %w", err)
	}
	if status != "" {
		return Manifest{}, errors.New("worktree has uncommitted or untracked changes; commit or remove them first")
	}
	return manifest, nil
}

// Remove deletes a clean llmux-owned worktree and its manifest. The branch is
// deliberately retained so committed work cannot be lost by cleanup.
func Remove(path string) (Manifest, error) {
	owned, err := Load(path)
	if err != nil {
		return Manifest{}, err
	}
	if _, statErr := os.Stat(owned.WorktreePath); errors.Is(statErr, os.ErrNotExist) {
		return removeMissing(owned)
	} else if statErr != nil {
		return Manifest{}, fmt.Errorf("worktree directory is unavailable: %w", statErr)
	}

	manifest, err := ValidateRemoval(path)
	if err != nil {
		return Manifest{}, err
	}
	if _, err := gitOutputWithDir(manifest.CommonDir,
		"worktree", "remove", manifest.WorktreePath); err != nil {
		return Manifest{}, fmt.Errorf("couldn't remove Git worktree: %w", err)
	}
	_, _ = gitOutputWithDir(manifest.CommonDir, "worktree", "prune")
	if err := os.Remove(manifestPath(manifest.WorktreePath)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return Manifest{}, fmt.Errorf("worktree was removed but its llmux manifest remains: %w", err)
	}
	removeEmptyDir(filepath.Dir(manifest.WorktreePath))
	return manifest, nil
}

// removeMissing forgets a manifest only after Git also stops recognizing its
// vanished checkout. This recovers from manual directory deletion and from a
// previous cleanup that removed the checkout but failed to remove the manifest.
func removeMissing(manifest Manifest) (Manifest, error) {
	if _, err := os.Stat(manifest.CommonDir); err == nil {
		_, _ = gitOutputWithDir(manifest.CommonDir, "worktree", "prune")
		registered, listErr := isRegistered(manifest)
		if listErr != nil {
			return Manifest{}, fmt.Errorf("couldn't verify missing worktree registration: %w", listErr)
		}
		if registered {
			return Manifest{}, errors.New("worktree directory is missing but Git still considers it active")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return Manifest{}, fmt.Errorf("Git common directory is unavailable: %w", err)
	}
	if err := os.Remove(manifestPath(manifest.WorktreePath)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return Manifest{}, fmt.Errorf("couldn't remove stale worktree manifest: %w", err)
	}
	removeEmptyDir(filepath.Dir(manifest.WorktreePath))
	return manifest, nil
}

func isRegistered(manifest Manifest) (bool, error) {
	output, err := gitOutputWithDir(manifest.CommonDir, "worktree", "list", "--porcelain")
	if err != nil {
		return false, err
	}
	target, err := normalizePath(manifest.WorktreePath)
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(output, "\n") {
		if !strings.HasPrefix(line, "worktree ") {
			continue
		}
		path, err := normalizePath(strings.TrimPrefix(line, "worktree "))
		if err == nil && path == target {
			return true, nil
		}
	}
	return false, nil
}

// Load returns the manifest for path only when it represents the exact
// llmux-owned worktree recorded there.
func Load(path string) (Manifest, error) {
	normalized, err := normalizePath(path)
	if err != nil {
		return Manifest{}, err
	}
	data, err := os.ReadFile(manifestPath(normalized))
	if errors.Is(err, os.ErrNotExist) {
		return Manifest{}, errors.New("selected session is not an llmux worktree")
	}
	if err != nil {
		return Manifest{}, fmt.Errorf("couldn't read worktree manifest: %w", err)
	}
	var manifest Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return Manifest{}, fmt.Errorf("invalid worktree manifest: %w", err)
	}
	recorded, err := normalizePath(manifest.WorktreePath)
	if err != nil || manifest.Version != manifestVersion || recorded != normalized {
		return Manifest{}, errors.New("worktree manifest does not match the selected path")
	}
	manifest.WorktreePath = recorded
	return manifest, nil
}

// List returns all readable llmux worktree manifests, including worktrees
// whose agent sessions are no longer running.
func List() ([]Manifest, error) {
	entries, err := os.ReadDir(manifestDir())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	manifests := make([]Manifest, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(manifestDir(), entry.Name()))
		if err != nil {
			continue
		}
		var manifest Manifest
		if json.Unmarshal(data, &manifest) == nil && manifest.Version == manifestVersion {
			manifests = append(manifests, manifest)
		}
	}
	sort.Slice(manifests, func(i, j int) bool {
		return manifests[i].CreatedAt.Before(manifests[j].CreatedAt)
	})
	return manifests, nil
}

// WorktreeRoot is the centralized checkout root. XDG_DATA_HOME is honored so
// tests and non-default XDG layouts remain isolated.
func WorktreeRoot() string {
	return filepath.Join(dataHome(), "llmux", "worktrees")
}

// Slug converts a task label into a conservative branch/path component.
func Slug(label string) string {
	var out strings.Builder
	lastDash := false
	characters := 0
	for _, r := range strings.ToLower(strings.TrimSpace(label)) {
		if characters >= 48 {
			break
		}
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			out.WriteRune(r)
			lastDash = false
			characters++
			continue
		}
		if out.Len() > 0 && !lastDash {
			out.WriteByte('-')
			lastDash = true
			characters++
		}
	}
	slug := strings.Trim(out.String(), "-")
	if slug == "" {
		return "task"
	}
	return slug
}

func repoStorageName(name, commonDir string) string {
	sum := sha256.Sum256([]byte(commonDir))
	return fmt.Sprintf("%s-%x", Slug(name), sum[:4])
}

func branchExists(commonDir, branch string) bool {
	command := exec.Command("git", "--git-dir", commonDir,
		"show-ref", "--verify", "--quiet", "refs/heads/"+branch)
	return command.Run() == nil
}

func writeManifest(manifest Manifest) error {
	if err := os.MkdirAll(manifestDir(), 0o700); err != nil {
		return fmt.Errorf("couldn't create worktree manifest directory: %w", err)
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	path := manifestPath(manifest.WorktreePath)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("couldn't write worktree manifest: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("couldn't publish worktree manifest: %w", err)
	}
	return nil
}

func manifestDir() string {
	return filepath.Join(dataHome(), "llmux", "tasks")
}

func manifestPath(path string) string {
	if normalized, err := normalizePath(path); err == nil {
		path = normalized
	}
	sum := sha256.Sum256([]byte(filepath.Clean(path)))
	return filepath.Join(manifestDir(), fmt.Sprintf("%x.json", sum[:12]))
}

func manifestExists(path string) bool {
	_, err := os.Stat(manifestPath(path))
	return err == nil
}

func dataHome() string {
	if value := os.Getenv("XDG_DATA_HOME"); value != "" {
		return value
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".local", "share")
	}
	return filepath.Join(os.TempDir(), "llmux-data")
}

func normalizePath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	abs = filepath.Clean(abs)
	probe := abs
	missing := []string{}
	for {
		if resolved, err := filepath.EvalSymlinks(probe); err == nil {
			parts := append([]string{resolved}, missing...)
			return filepath.Join(parts...), nil
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			break
		}
		missing = append([]string{filepath.Base(probe)}, missing...)
		probe = parent
	}
	return abs, nil
}

func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil || !errors.Is(err, os.ErrNotExist)
}

func removeEmptyDir(path string) {
	_ = os.Remove(path)
	_ = os.Remove(filepath.Dir(path))
}

func gitOutputAt(path string, args ...string) (string, error) {
	commandArgs := append([]string{"-C", path}, args...)
	output, err := exec.Command("git", commandArgs...).Output()
	if err != nil {
		message := ""
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			message = strings.TrimSpace(string(exitErr.Stderr))
		}
		if message == "" {
			message = err.Error()
		}
		return "", errors.New(message)
	}
	return strings.TrimSpace(string(output)), nil
}

func gitOutputWithDir(commonDir string, args ...string) (string, error) {
	commandArgs := append([]string{"--git-dir", commonDir}, args...)
	output, err := exec.Command("git", commandArgs...).Output()
	if err != nil {
		message := ""
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			message = strings.TrimSpace(string(exitErr.Stderr))
		}
		if message == "" {
			message = err.Error()
		}
		return "", errors.New(message)
	}
	return strings.TrimSpace(string(output)), nil
}
