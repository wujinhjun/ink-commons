//go:build linux || darwin

package ink

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wujinhjun/ink-commons/internal/execution"
)

func lifecycleRequest(t *testing.T, s *Service, method, path, body string, want int) *httptest.ResponseRecorder {
	t.Helper()
	out := httptest.NewRecorder()
	s.Handler().ServeHTTP(out, httptest.NewRequest(method, path, strings.NewReader(body)))
	if out.Code != want {
		t.Fatalf("%s %s: got %d, want %d: %s", method, path, out.Code, want, out.Body)
	}
	return out
}

func TestRemoveRepositoryBlocksUnsettledState(t *testing.T) {
	for _, tc := range []struct {
		name, reason string
		setup        func(*Work)
	}{
		{"active claim", "task handoff", func(w *Work) { w.Task.Handoff.State = "claimed" }},
		{"submitted claim", "task handoff", func(w *Work) { w.Task.Handoff.State = "submitted" }},
		{"legacy execution", "unresolved execution", func(w *Work) { w.Task.Runs = []Attempt{{State: "running"}} }},
		{"notification ACK", "notification", func(w *Work) {
			w.Notifications = []NotificationRun{{Notification: execution.Notification{EventID: "event"}, State: "awaiting_ack", RequireACK: true}}
		}},
		{"approval hook", "post_approval", func(w *Work) { w.Approvals = []ApprovalRun{{State: "pending"}} }},
		{"pending merge", "in-flight merge", func(w *Work) { w.AutoMerges = []AutoMergeRun{{State: "pending"}} }},
		{"unknown merge", "in-flight merge", func(w *Work) { w.AutoMerges = []AutoMergeRun{{State: "unknown"}} }},
		{"active workspace", "active workspace", func(_ *Work) {}},
		{"preparing workspace", "active workspace", func(_ *Work) {}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := centralConfigService(t, Config{RepositoryDefaults: Repository{ReviewerLogin: "reviewer"}}, nil)
			registerConfigAPI(t, s, `{"repo":"acme/project"}`, 201)
			work := &Work{Repo: "acme/project", Pull: Pull{Number: 42, State: "open"},
				Task: &Task{ID: "task", Handoff: &TaskHandoff{State: "pending"}}}
			tc.setup(work)
			if err := s.store.saveWork(work); err != nil {
				t.Fatal(err)
			}
			if state, rest, _ := strings.Cut(tc.name, " "); rest == "workspace" {
				if err := insertWorkspace(context.Background(), s.store.db, WorkspaceAllocation{
					ID: "allocation", RequestID: "request", TaskID: "task", State: state,
				}, "acme/project"); err != nil {
					t.Fatal(err)
				}
			}
			before, _ := s.store.effectiveConfig(work.Repo)
			response := lifecycleRequest(t, s, "DELETE", "/v1/repositories/acme/project", "", 409)
			if !strings.Contains(response.Body.String(), tc.reason) {
				t.Fatal(response.Body.String())
			}
			after, err := s.store.effectiveConfig(work.Repo)
			stored, loadErr := s.store.loadWork(keyForWork(work))
			if err != nil || loadErr != nil || before.Revision != after.Revision || string(jsonValue(stored)) != string(jsonValue(work)) {
				t.Fatalf("conflicting deletion modified state: %v %v", err, loadErr)
			}
			if strings.HasSuffix(tc.name, "workspace") {
				allocations, err := s.store.exportWorkspaces()
				if err != nil || len(allocations) != 1 || allocations[0].ID != "allocation" || allocations[0].State != strings.Fields(tc.name)[0] {
					t.Fatalf("conflicting deletion changed allocation: %+v, %v", allocations, err)
				}
			}
		})
	}
}

func TestRemoveRepositoryFencesDispatchAndRetainsHistory(t *testing.T) {
	s, path := centralConfigService(t, Config{
		Repositories: []Repository{{Repo: "acme/project", ReviewerLogin: "reviewer"}},
	}, nil)
	route := execution.Route{Provider: "chat", Target: "dev", Session: "dev-session"}
	work := &Work{Repo: "acme/project", Pull: Pull{Number: 42, State: "open"}, Linked: true, Phase: "approved",
		Task:      &Task{ID: "retire-me", Handoff: &TaskHandoff{State: "pending"}},
		Developer: Assignee{Route: &route},
		Notifications: []NotificationRun{{
			Notification: execution.Notification{EventID: "event", Event: "review_approved"}, State: "awaiting_ack", RequireACK: true, Route: &route,
		}},
	}
	if err := s.store.saveWork(work); err != nil {
		t.Fatal(err)
	}
	lifecycleRequest(t, s, "DELETE", "/v1/repositories/acme/project", "", 409)
	lifecycleRequest(t, s, "POST", "/v1/notifications/ack", `{"event_id":"event"}`, 200)
	unlock, _ := s.pullLocks.tryLock(pullKey{work.Repo, 42})
	lifecycleRequest(t, s, "DELETE", "/v1/repositories/acme/project", "", 409)
	unlock()
	unlock, _ = s.pullLocks.tryLock(pullKey{work.Repo, 0})
	lifecycleRequest(t, s, "DELETE", "/v1/repositories/acme/project", "", 409)
	unlock()
	unlock, _ = s.pullLocks.tryLock(pullKey{"another/repo", 0})
	defer unlock()
	lifecycleRequest(t, s, "DELETE", "/v1/repositories/ACME/Project", "", 200)
	lifecycleRequest(t, s, "DELETE", "/v1/repositories/acme/project", "", 404)
	lifecycleRequest(t, s, "POST", "/v1/work/acme/project/42/retry", "", 404)
	lifecycleRequest(t, s, "POST", "/v1/tasks/retire-me/claim", "{}", 404)
	lifecycleRequest(t, s, "GET", "/v1/work/acme/project/42", "", 200)

	stored, task, err := s.store.loadTask("retire-me")
	if err != nil || stored.Task != nil || stored.Notifications[0].State != "acknowledged" || task.Handoff.State != "superseded" {
		t.Fatalf("historical state not retained: %v", err)
	}
	s.up.http.Transport = testTransport(func(req *http.Request) (*http.Response, error) {
		t.Errorf("removed repository was observed: %s", req.URL.Path)
		return httptest.NewRecorder().Result(), nil
	})
	s.Poll(context.Background())
	if err := s.pollPull(context.Background(), pollJob{repo: Repository{Repo: work.Repo}, number: 42, observe: true}); err != nil {
		t.Fatal(err)
	}
	restarted, err := NewService(s.config, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if repos := restarted.repositories(); len(repos) != 0 {
		t.Fatal("restart reseeded removed repository")
	}
}

func TestRemoveRepositoryKeepsDirtyStandaloneWorkspace(t *testing.T) {
	s, _ := centralConfigService(t, Config{Repositories: []Repository{{Repo: "acme/project", ReviewerLogin: "reviewer"}}}, nil)
	dir := t.TempDir()
	for _, args := range [][]string{{"init", dir}, {"-C", dir, "remote", "add", "origin", "https://github.com/acme/new-remote.git"}} {
		if err := exec.Command("git", args...).Run(); err != nil {
			t.Fatal(err)
		}
	}
	file := filepath.Join(dir, "uncommitted.txt")
	if err := os.WriteFile(file, []byte("local work"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := insertWorkspace(context.Background(), s.store.db, WorkspaceAllocation{
		ID: "standalone", RequestID: "standalone-request", State: "active", Source: dir, Path: dir,
	}, "acme/project"); err != nil {
		t.Fatal(err)
	}
	lifecycleRequest(t, s, "DELETE", "/v1/repositories/acme/project", "", 409)
	if got, err := os.ReadFile(file); err != nil || string(got) != "local work" {
		t.Fatalf("local workspace changed: %q, %v", got, err)
	}
}

func TestRepositoryPatchIsAtomic(t *testing.T) {
	s, _ := centralConfigService(t, Config{Repositories: []Repository{{Repo: "acme/project", ReviewerLogin: "reviewer"}}}, nil)
	path := "/v1/repositories/acme/project"
	lifecycleRequest(t, s, "PATCH", "/v1/repositories/acme/missing", `{"reviewer_login":"new"}`, 404)
	lifecycleRequest(t, s, "PATCH", path, `{"reviewer_login":"updated","auto_merge":{"method":"invalid"}}`, 400)
	cfg, err := s.store.effectiveConfig("acme/project")
	if err != nil || cfg.Repository.ReviewerLogin != "reviewer" {
		t.Fatalf("partial update: %+v %v", cfg, err)
	}
	out := lifecycleRequest(t, s, http.MethodPatch, path, `{"reviewer_login":"updated","execution.max_attempts":5,"post_approval":null}`, 200)
	var repo Repository
	if err := json.Unmarshal(out.Body.Bytes(), &repo); err != nil || repo.ReviewerLogin != "updated" {
		t.Fatalf("patch response: %s", out.Body)
	}
	cfg, err = s.store.effectiveConfig("acme/project")
	if err != nil || cfg.Revision != 2 || cfg.Execution.MaxAttempts != 5 {
		t.Fatalf("wrong config generation or policy: %+v %v", cfg, err)
	}
}

func TestRepositoryMutationChecksMergeIdentityWhenReviewerCommandChanges(t *testing.T) {
	merger := filepath.Join(t.TempDir(), "merger")
	if err := os.WriteFile(merger, []byte("#!/bin/sh\n"), 0700); err != nil {
		t.Fatal(err)
	}
	s, _ := centralConfigService(t, Config{
		RepositoryDefaults: Repository{ReviewerLogin: "reviewer"},
		GitHubMerger:       &GitHubCredential{Helper: &GitHubHelperCredential{Command: merger, Identity: "merger"}},
	}, nil)
	registerConfigAPI(t, s, `{"repo":"acme/project","auto_merge":{"enabled":true}}`, 201)
	registerConfigAPI(t, s, `{"repo":"acme/override","reviewer_command":"independent","auto_merge":{"enabled":true}}`, 201)
	cfg, err := s.store.effectiveConfig("acme/project")
	if err != nil {
		t.Fatal(err)
	}
	command := string(jsonValue(merger))
	path := "/v1/repositories/acme/project"
	lifecycleRequest(t, s, http.MethodPatch, path, `{"reviewer_command":`+command+`,"human_developer":"should-not-persist"}`, 400)
	configAPI(t, s, http.MethodPost, "acme/project", "reviewer_command", command, 400)
	configAPI(t, s, http.MethodPost, "", "reviewer_command", command, 400)

	after, err := s.store.effectiveConfig("acme/project")
	if err != nil || after.Revision != cfg.Revision || after.Repository.ReviewerCommand != "" || after.Repository.HumanDeveloper != "" {
		t.Fatalf("failed mutation changed repository policy: %+v, %v", after.Repository, err)
	}

	// PATCH validates the final policy, including simultaneous changes.
	lifecycleRequest(t, s, http.MethodPatch, path, `{"reviewer_command":`+command+`,"auto_merge":{"enabled":false}}`, 200)
	configAPI(t, s, http.MethodPost, "", "reviewer_command", command, 200)
	configAPI(t, s, http.MethodDelete, "acme/override", "reviewer_command", "", 400)
}

func TestRemoveRepositoryRejectsUnknownStandaloneWorkspace(t *testing.T) {
	for _, name := range []string{"no origin", "non-GitHub origin"} {
		t.Run(name, func(t *testing.T) {
			source, _ := workspaceTestSource(t)
			if name == "no origin" {
				workspaceTestGit(t, source, "remote", "rename", "origin", "upstream")
			}
			s, _ := centralConfigService(t, Config{Repositories: []Repository{{Repo: "acme/project", ReviewerLogin: "reviewer"}}}, nil)
			ctx := context.Background()
			allocation, err := s.acquireWorkspace(ctx, workspaceRequest{Source: source, RequestID: "standalone", Preference: "lite"})
			if err != nil || allocation.State != "active" {
				t.Fatalf("acquire standalone workspace: state=%q, err=%v", allocation.State, err)
			}
			var unknown bool
			if err := s.store.db.QueryRow("SELECT repository IS NULL FROM workspace_allocations WHERE id = ?", allocation.ID).Scan(&unknown); err != nil || !unknown {
				t.Fatalf("expected SQL NULL for unknown workspace repository, err=%v", err)
			}
			const path = "/v1/repositories/acme/project"
			lifecycleRequest(t, s, http.MethodDelete, path, "", 409)

			// Verify the guard also protects existing rows with the old encoding.
			if _, err := s.store.db.Exec("UPDATE workspace_allocations SET repository = '' WHERE id = ?", allocation.ID); err != nil {
				t.Fatal(err)
			}
			lifecycleRequest(t, s, http.MethodDelete, path, "", 409)
			if _, err := s.store.effectiveConfig("acme/project"); err != nil {
				t.Fatalf("repository registration was changed: %v", err)
			}
		})
	}
}
