//go:build linux || darwin

package ink

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/wujinhjun/ink-commons/internal/execution"
	"github.com/wujinhjun/ink-commons/internal/workspace"
)

const workspaceColumns = "id, request_id, common_dir, source, path, kind, preference, ref, branch, head, previous_head, state, owner_route, task_id"

type WorkspaceAllocation struct {
	ID         string           `json:"id"`
	RequestID  string           `json:"request_id"`
	Common     string           `json:"git_common_dir"`
	Source     string           `json:"source"`
	Path       string           `json:"path"`
	Kind       string           `json:"kind"`
	Preference string           `json:"preference"`
	Ref        string           `json:"ref,omitempty"`
	Branch     string           `json:"branch,omitempty"`
	Head       string           `json:"head"`
	Previous   string           `json:"previous_head,omitempty"`
	Owner      *execution.Route `json:"owner,omitempty"`
	TaskID     string           `json:"task_id,omitempty"`
	State      string           `json:"state"`
}

type workspaceRequest struct {
	Source     string           `json:"source"`
	RequestID  string           `json:"request_id"`
	Preference string           `json:"preference"`
	Ref        string           `json:"ref"`
	Branch     string           `json:"branch"`
	Owner      *execution.Route `json:"owner,omitempty"`
	TaskID     string           `json:"task_id,omitempty"`
	ClaimToken string           `json:"claim_token,omitempty"`
}

var workspaceKey = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,128}$`)

func readWorkspace(row interface{ Scan(...any) error }) (WorkspaceAllocation, error) {
	var a WorkspaceAllocation
	var route string
	err := row.Scan(&a.ID, &a.RequestID, &a.Common, &a.Source, &a.Path, &a.Kind, &a.Preference, &a.Ref, &a.Branch, &a.Head, &a.Previous, &a.State, &route, &a.TaskID)
	if err == nil {
		err = json.Unmarshal([]byte(route), &a.Owner)
	}
	return a, err
}

func (s *Service) workspaces(w http.ResponseWriter, r *http.Request) {
	// Git reads repository configuration and can run configured filters. Workspace
	// operations are available only in the unprivileged user's daemon.
	if os.Geteuid() == 0 {
		fail(w, apiError{403, "workspace operations require an unprivileged daemon"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	s.workspaceMu.Lock()
	defer s.workspaceMu.Unlock()
	var result any
	var err error
	switch r.Method {
	case http.MethodGet:
		result, err = s.workspaceInventory(ctx, r.URL.Query().Get("repo"))
	case http.MethodPost:
		if id := r.PathValue("workspace"); id != "" {
			result, err = s.finishWorkspace(ctx, id, r.PathValue("action"))
		} else {
			var request workspaceRequest
			if err = decode(w, r, &request); err == nil {
				result, err = s.acquireWorkspace(ctx, request)
			}
		}
	}
	if err != nil {
		fail(w, err)
		return
	}
	reply(w, 200, result)
}

func (s *Service) workspaceInventory(ctx context.Context, source string) (any, error) {
	repo, err := workspace.Inspect(ctx, source)
	if err != nil {
		return nil, apiError{400, err.Error()}
	}
	rows, err := s.store.db.QueryContext(ctx, "SELECT "+workspaceColumns+`
		FROM workspace_allocations a WHERE common_dir = ?
		AND rowid = (SELECT MAX(rowid) FROM workspace_allocations WHERE path = a.path)
		AND state <> 'removed' ORDER BY path`, repo.Common)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	allocations := []WorkspaceAllocation{}
	for rows.Next() {
		a, err := readWorkspace(rows)
		if err != nil {
			return nil, err
		}
		allocations = append(allocations, a)
	}
	return struct {
		workspace.Repository
		Allocations []WorkspaceAllocation `json:"allocations"`
	}{repo, allocations}, rows.Err()
}

func (s *Service) acquireWorkspace(ctx context.Context, req workspaceRequest) (WorkspaceAllocation, error) {
	if !filepath.IsAbs(req.Source) || !workspaceKey.MatchString(req.RequestID) {
		return WorkspaceAllocation{}, apiError{400, "an absolute repo path and a stable request ID are required"}
	}
	if req.Preference == "" {
		req.Preference = "auto"
	}
	if req.Preference != "auto" && req.Preference != "full" && req.Preference != "lite" {
		return WorkspaceAllocation{}, apiError{400, "kind must be auto, full, or lite"}
	}
	common, err := workspace.CommonDir(ctx, req.Source)
	if err != nil {
		return WorkspaceAllocation{}, apiError{400, err.Error()}
	}
	a, err := readWorkspace(s.store.db.QueryRowContext(ctx, "SELECT "+workspaceColumns+" FROM workspace_allocations WHERE request_id = ?", req.RequestID))
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return a, err
	}
	if err == nil && (a.Common != common || a.Preference != req.Preference || a.Ref != req.Ref ||
		a.Branch != req.Branch || a.TaskID != req.TaskID || !sameWorkspaceOwner(a.Owner, req.Owner)) {
		return a, apiError{409, "request ID is already bound to different workspace parameters"}
	}

	skipped := map[string]bool{}
	pinnedHead := ""
	for {
		if a.ID == "" {
			a, err = s.reserveWorkspace(ctx, req, common, pinnedHead, skipped)
			if err != nil {
				return a, err
			}
		}
		if a.State != "preparing" {
			return a, nil
		}
		active, unchanged, err := s.activateWorkspace(ctx, req, a)
		if !unchanged {
			return active, err
		}
		// This reservation can go away without changing the last released
		// allocation. No client received it as active.
		if _, deleteErr := s.store.db.ExecContext(ctx, "DELETE FROM workspace_allocations WHERE id = ? AND state = 'preparing'", a.ID); deleteErr != nil {
			return a, deleteErr
		}
		if req.Preference != "auto" {
			return WorkspaceAllocation{}, err
		}
		skipped[a.Path] = true
		pinnedHead = a.Head
		a = WorkspaceAllocation{}
	}
}

func (s *Service) activateWorkspace(ctx context.Context, req workspaceRequest, a WorkspaceAllocation) (WorkspaceAllocation, bool, error) {
	if a.TaskID != "" {
		// Don't allow handoff to invalidate this claim during Git preparation.
		s.mu.Lock()
		defer s.mu.Unlock()
		if err := s.validateWorkspaceOwner(ctx, req, a.Head); err != nil {
			return a, false, err
		}
	}
	if err := prepareWorkspace(ctx, a); err != nil {
		if workspaceStillReleased(ctx, a) {
			return a, true, apiError{409, err.Error()}
		}
		return a, false, apiError{409, fmt.Sprintf("%s; workspace %s remains preparing; inspect ink workspace list, then retry the same request or run ink workspace release --id %s", StartupDiagnostic(err), a.ID, a.ID)}
	}
	active, err := s.workspaceState(ctx, a, "active")
	return active, false, err
}

func workspaceStillReleased(ctx context.Context, a WorkspaceAllocation) bool {
	if a.Previous == "" {
		return false
	}
	repo, err := workspace.Inspect(ctx, a.Source)
	if err != nil || repo.Common != a.Common {
		return false
	}
	tree, err := managedWorkspaceTree(ctx, repo, a.Path)
	return err == nil && tree != nil && tree.Head == a.Previous && tree.Branch == "" && !tree.Dirty &&
		workspace.CheckIdle(ctx, a.Path) == nil
}

func sameWorkspaceOwner(a, b *execution.Route) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Provider == b.Provider && a.Target == b.Target && a.Session == b.Session
}

func (s *Service) validateWorkspaceOwner(ctx context.Context, req workspaceRequest, sha string) error {
	if req.Owner != nil && !req.Owner.Valid(false) {
		return apiError{400, "workspace owner route is incomplete"}
	}
	if req.TaskID == "" {
		return nil
	}
	w, task, err := s.store.loadTask(req.TaskID)
	if err != nil {
		return err
	}
	if w == nil || task == nil || w.Task != task || task.Handoff == nil ||
		task.Handoff.State != "claimed" || len(req.ClaimToken) < 32 || task.Handoff.ClaimHash != claimHash(req.ClaimToken) ||
		req.Owner == nil || !sameWorkspaceOwner(req.Owner, task.Handoff.Assignee.Route) {
		return apiError{409, "workspace task is not currently claimed by this route"}
	}
	remote, err := workspace.Git(ctx, req.Source, "remote", "get-url", "origin")
	if err != nil || GitHubRepository(string(remote)) != strings.ToLower(w.Repo) || task.SHA != sha {
		return apiError{409, "workspace repository or starting SHA does not match the claimed task"}
	}
	return nil
}

func (s *Service) reserveWorkspace(ctx context.Context, req workspaceRequest, common, pinnedHead string, skipped map[string]bool) (WorkspaceAllocation, error) {
	ref := req.Ref
	if ref == "" {
		ref = "HEAD"
	}
	if req.Branch != "" {
		// check-ref-format --branch expands @{-1}; validate the literal full ref.
		if strings.HasPrefix(req.Branch, "-") {
			return WorkspaceAllocation{}, apiError{400, "invalid workspace branch"}
		}
		if _, err := workspace.Git(ctx, req.Source, "check-ref-format", "refs/heads/"+req.Branch); err != nil {
			return WorkspaceAllocation{}, apiError{400, "invalid workspace branch"}
		}
		if req.Ref == "" {
			if _, err := workspace.Git(ctx, req.Source, "show-ref", "--verify", "--quiet", "refs/heads/"+req.Branch); err == nil {
				ref = "refs/heads/" + req.Branch
			}
		}
	}
	head, err := workspace.Git(ctx, req.Source, "rev-parse", "--verify", "--end-of-options", ref+"^{commit}")
	if err != nil {
		return WorkspaceAllocation{}, apiError{400, "workspace ref is not an available local commit"}
	}
	if pinnedHead != "" && strings.TrimSpace(string(head)) != pinnedHead {
		return WorkspaceAllocation{}, apiError{409, "workspace ref moved while finding an alternate slot; retry with a fixed ref"}
	}
	if req.Branch != "" {
		branchHead, err := workspace.Git(ctx, req.Source, "rev-parse", "--verify", "--end-of-options", "refs/heads/"+req.Branch)
		if err == nil && string(branchHead) != string(head) {
			return WorkspaceAllocation{}, apiError{409, "existing branch does not match the requested ref"}
		}
	}
	if err := s.validateWorkspaceOwner(ctx, req, strings.TrimSpace(string(head))); err != nil {
		return WorkspaceAllocation{}, err
	}
	// Use a stable clone-specific directory. A workspace checkout must never
	// live beneath the clone's working tree or share slots with a second clone.
	dir := filepath.Join(s.workspaceRoot, fmt.Sprintf("%x", sha256.Sum256([]byte(common))))
	repo, err := workspace.Inspect(ctx, req.Source)
	if err != nil {
		return WorkspaceAllocation{}, apiError{400, err.Error()}
	}
	a := WorkspaceAllocation{ID: uuid.NewString(), RequestID: req.RequestID, Common: common,
		Preference: req.Preference, Ref: req.Ref, Branch: req.Branch,
		Head: strings.TrimSpace(string(head)), State: "preparing", Kind: "lite", Owner: req.Owner, TaskID: req.TaskID}
	if len(repo.Worktree) == 0 {
		return a, apiError{409, "source clone has no Git worktree registration"}
	}
	a.Source = repo.Worktree[0].Path
	if relative, err := filepath.Rel(a.Source, dir); err == nil &&
		relative != ".." && !strings.HasPrefix(relative, ".."+string(os.PathSeparator)) {
		return a, apiError{409, "workspace storage must be outside the source clone"}
	}
	for _, tree := range repo.Worktree {
		if req.Branch != "" && tree.Branch == req.Branch {
			return a, apiError{409, "branch is already checked out; inspect ink workspace list"}
		}
	}
	if req.Preference != "lite" {
		for slot := 1; slot <= s.fullWorkspaceSlots; slot++ {
			path := filepath.Join(dir, fmt.Sprintf("full-%d", slot))
			if skipped[path] {
				continue
			}
			previous, available, err := s.availableWorkspaceSlot(ctx, repo, path)
			if err != nil {
				return a, err
			}
			if available {
				a.Path, a.Previous, a.Kind = path, previous, "full"
				break
			}
		}
	}
	if a.Path == "" {
		if req.Preference == "full" {
			return a, apiError{409, "no Full workspace slot is available"}
		}
		a.Path = filepath.Join(dir, "lite-"+a.ID)
	}
	remote, _ := workspace.Git(ctx, a.Source, "remote", "get-url", "origin")
	return a, insertWorkspace(ctx, s.store.db, a, GitHubRepository(strings.TrimSpace(string(remote))))
}

func insertWorkspace(ctx context.Context, db interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, a WorkspaceAllocation, repo string) error {
	route, err := json.Marshal(a.Owner)
	if err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, "INSERT INTO workspace_allocations ("+workspaceColumns+", repository) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		a.ID, a.RequestID, a.Common, a.Source, a.Path, a.Kind, a.Preference, a.Ref, a.Branch, a.Head, a.Previous, a.State, string(route), a.TaskID, repo)
	return err
}

func (s *stateStore) exportWorkspaces() ([]WorkspaceAllocation, error) {
	rows, err := s.db.Query("SELECT " + workspaceColumns + " FROM workspace_allocations ORDER BY rowid")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var allocations []WorkspaceAllocation
	for rows.Next() {
		a, err := readWorkspace(rows)
		if err != nil {
			return nil, err
		}
		allocations = append(allocations, a)
	}
	return allocations, rows.Err()
}

func (s *Service) availableWorkspaceSlot(ctx context.Context, repo workspace.Repository, path string) (string, bool, error) {
	a, err := readWorkspace(s.store.db.QueryRowContext(ctx, "SELECT "+workspaceColumns+" FROM workspace_allocations WHERE path = ? ORDER BY rowid DESC LIMIT 1", path))
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", false, err
	}
	if errors.Is(err, sql.ErrNoRows) || a.State == "removed" {
		_, statErr := os.Lstat(path)
		return "", errors.Is(statErr, os.ErrNotExist), nil
	}
	if a.State != "released" {
		return "", false, nil
	}
	for _, tree := range repo.Worktree {
		if tree.Path == path {
			return tree.Head, tree.Error == "" && !tree.Dirty && !tree.Locked && !tree.Prunable &&
				tree.Branch == "" && tree.Head == a.Head, nil
		}
	}
	return "", false, nil
}

func prepareWorkspace(ctx context.Context, a WorkspaceAllocation) error {
	repo, err := workspace.Inspect(ctx, a.Source)
	if err != nil {
		return err
	}
	if repo.Common != a.Common {
		return errors.New("source clone identity changed")
	}
	tree, err := managedWorkspaceTree(ctx, repo, a.Path)
	if err != nil {
		return err
	}
	if tree == nil {
		if a.Previous != "" {
			return errors.New("reserved Full worktree is missing")
		}
		if err := os.MkdirAll(filepath.Dir(a.Path), 0700); err != nil {
			return err
		}
		if _, err := workspace.Git(ctx, a.Source, "worktree", "add", "--detach", "--", a.Path, a.Head); err != nil {
			return err
		}
		tree = &workspace.Worktree{Head: a.Head}
	}
	if err := workspace.CheckIdle(ctx, a.Path); err != nil {
		return err
	}
	if tree.Dirty || (tree.Head != a.Head && tree.Head != a.Previous) || (tree.Branch != "" && tree.Branch != a.Branch) {
		return errors.New("reserved worktree changed; inspect it before retrying")
	}
	if tree.Head != a.Head {
		if _, err := workspace.Git(ctx, a.Path, "checkout", "--no-overwrite-ignore", "--detach", a.Head); err != nil {
			return err
		}
	}
	if a.Branch == "" || tree.Branch == a.Branch {
		return nil
	}
	branchRef := "refs/heads/" + a.Branch
	existing, err := workspace.Git(ctx, a.Path, "rev-parse", "--verify", "--end-of-options", branchRef)
	if err == nil {
		if strings.TrimSpace(string(existing)) != a.Head {
			return errors.New("workspace branch moved after reservation")
		}
		_, err = workspace.Git(ctx, a.Path, "checkout", "--no-overwrite-ignore", "--no-guess", a.Branch, "--")
	} else {
		_, err = workspace.Git(ctx, a.Path, "checkout", "--no-overwrite-ignore", "-b", a.Branch, a.Head)
	}
	return err
}

func managedWorkspaceTree(ctx context.Context, repo workspace.Repository, path string) (*workspace.Worktree, error) {
	for _, tree := range repo.Worktree {
		if tree.Path != path {
			continue
		}
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil || resolved != path || tree.Main || tree.Locked || tree.Prunable || tree.Error != "" {
			return nil, errors.New("managed worktree is unavailable, locked, or has inconsistent registration")
		}
		common, err := workspace.CommonDir(ctx, path)
		if err != nil || common != repo.Common {
			return nil, errors.New("managed worktree clone identity changed")
		}
		return &tree, nil
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		return nil, errors.New("managed worktree path exists outside Git registration")
	}
	return nil, nil
}

func (s *Service) workspaceState(ctx context.Context, a WorkspaceAllocation, state string) (WorkspaceAllocation, error) {
	_, err := s.store.db.ExecContext(ctx, "UPDATE workspace_allocations SET state = ?, head = ? WHERE id = ?", state, a.Head, a.ID)
	if err == nil {
		a.State = state
	}
	return a, err
}

func (s *Service) finishWorkspace(ctx context.Context, id, action string) (WorkspaceAllocation, error) {
	a, err := readWorkspace(s.store.db.QueryRowContext(ctx, "SELECT "+workspaceColumns+" FROM workspace_allocations WHERE id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return a, apiError{404, "workspace not found"}
	}
	if err != nil {
		return a, err
	}
	switch action {
	case "release":
		if a.State == "released" || a.State == "removed" {
			return a, nil
		}
		if a.State != "active" && a.State != "preparing" {
			return a, apiError{409, "workspace is not active"}
		}
	case "remove":
		if a.State == "removed" {
			return a, nil
		}
		if a.State != "released" || a.Kind != "lite" {
			return a, apiError{409, "only released Lite worktrees can be removed"}
		}
	default:
		return a, apiError{404, "unknown workspace action"}
	}
	repo, err := workspace.Inspect(ctx, a.Source)
	if err != nil || repo.Common != a.Common {
		return a, apiError{409, "source clone is unavailable or changed"}
	}
	tree, err := managedWorkspaceTree(ctx, repo, a.Path)
	if err != nil {
		return a, apiError{409, err.Error()}
	}
	if tree == nil && (action == "remove" || a.State == "preparing") {
		return s.workspaceState(ctx, a, "removed")
	}
	if tree == nil || tree.Dirty {
		return a, apiError{409, "worktree is missing or has uncommitted changes"}
	}
	if action == "remove" && tree.Ignored {
		return a, apiError{409, "Lite worktree contains ignored files; inspect and remove them before retrying"}
	}
	if err := s.checkWorkspacePublished(ctx, a.Source, *tree); err != nil {
		return a, apiError{409, err.Error()}
	}
	// Remote verification may take time; don't act on a stale local observation.
	head, err := workspace.Git(ctx, a.Path, "rev-parse", "HEAD")
	dirty, ignored, statusErr := workspace.Status(ctx, a.Path)
	if err != nil || statusErr != nil || strings.TrimSpace(string(head)) != tree.Head || dirty || (action == "remove" && ignored) {
		return a, apiError{409, "worktree changed during verification; retry after inspecting it"}
	}
	if err := workspace.CheckIdle(ctx, a.Path); err != nil {
		return a, apiError{409, err.Error()}
	}
	a.Head = tree.Head
	if action == "release" {
		if tree.Branch != "" {
			if _, err := workspace.Git(ctx, a.Path, "checkout", "--no-overwrite-ignore", "--detach", tree.Head); err != nil {
				return a, apiError{409, err.Error()}
			}
		}
		return s.workspaceState(ctx, a, "released")
	}
	if _, err := workspace.Git(ctx, a.Source, "worktree", "remove", "--", a.Path); err != nil {
		return a, apiError{409, err.Error()}
	}
	return s.workspaceState(ctx, a, "removed")
}

func (s *Service) checkWorkspacePublished(ctx context.Context, source string, tree workspace.Worktree) error {
	remote, err := workspace.Git(ctx, source, "remote", "get-url", "origin")
	if err != nil {
		return errors.New("cannot verify that HEAD is published: origin is missing")
	}
	var heads []string
	if repo := GitHubRepository(string(remote)); repo != "" {
		var refs []struct {
			Object struct {
				SHA string `json:"sha"`
			} `json:"object"`
		}
		if _, err := s.up.gitHubGet(ctx, "/repos/"+repo+"/git/matching-refs/heads", &refs); err != nil {
			return errors.New("cannot verify that HEAD is published: GitHub origin could not be checked")
		}
		for _, ref := range refs {
			heads = append(heads, ref.Object.SHA)
		}
	} else {
		refs, err := workspace.Git(ctx, source, "ls-remote", "--heads", "origin")
		if err != nil {
			return errors.New("cannot verify that HEAD is published: origin could not be checked")
		}
		for _, line := range strings.Split(string(refs), "\n") {
			if sha, _, ok := strings.Cut(line, "\t"); ok {
				heads = append(heads, sha)
			}
		}
	}
	for _, head := range heads {
		if tree.Head == head && head != "" {
			return nil
		}
	}
	for _, head := range heads {
		if len(head) != 40 && len(head) != 64 {
			continue
		}
		if _, err := workspace.Git(ctx, source, "merge-base", "--is-ancestor", tree.Head, head); err == nil {
			return nil
		}
	}
	return errors.New("HEAD could not be verified on an origin branch; push or fetch before retrying")
}
