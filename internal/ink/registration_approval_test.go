package ink

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRegistrationPersistsAndPollsWithoutRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), stateDatabase)
	cfg := Config{RepositoryDefaults: Repository{ReviewerLogin: "reviewer", Review: &ReviewFeatures{Context: ReviewContextFeatures{ThreadHistory: true}}}}
	s, err := NewService(cfg, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	calls := 0
	s.up.http.Transport = testTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		w := httptest.NewRecorder()
		if strings.Contains(r.URL.Path, "denied") {
			w.WriteHeader(403)
		} else {
			reply(w, 200, []Pull{})
		}
		return w.Result(), nil
	})
	request := func(body string, want int) {
		t.Helper()
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, httptest.NewRequest("POST", "/v1/repositories", strings.NewReader(body)))
		if w.Code != want {
			t.Fatalf("register: %d %s", w.Code, w.Body)
		}
	}
	request(`{"repo":"ACME/Project"}`, 201)
	request(`{"repo":"acme/project"}`, 200)
	request(`{"repo":"acme/project","reviewer_login":"another"}`, 409)
	request(`{"repo":"acme/denied"}`, 422)
	request(`{"repo":"invalid"}`, 400)
	request(`{"repo":"acme/other","postApproval":{"command":["relative"]}}`, 400)
	before := calls
	s.Poll(context.Background())
	if calls != before+1 {
		t.Fatal("registered repo not polled")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = NewService(cfg, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	repos := s.repositories()
	if len(repos) != 1 || repos[0].Repo != "acme/project" || repos[0].ReviewerLogin != "reviewer" || repos[0].Review == nil || !repos[0].Review.Context.ThreadHistory {
		t.Fatalf("lost registration: %+v", repos)
	}
}

func TestApprovalHookFilteringDedupRestartAndNewSHA(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, stateDatabase)
	output := filepath.Join(dir, "events")
	script := filepath.Join(dir, "hook")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ncat >> \"$1\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	repo := Repository{Repo: "acme/project", ReviewerLogin: "reviewer", PostApproval: &ApprovalHook{Command: []string{script, output}}}
	cfg := Config{Repositories: []Repository{repo}}
	s, err := NewService(cfg, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close() }()
	var pull Pull
	pull.Number = 42
	pull.State = "open"
	pull.Head.SHA = strings.Repeat("a", 40)
	// The daemon only executes hooks on PRs whose ownership is resolved.
	st, _ := s.store.load()
	st.Work[key(repo.Repo, 42)] = &Work{Repo: repo.Repo, Pull: pull, Linked: true, Reviewer: Assignee{GitHubLogin: "reviewer"}}
	if err := s.store.save(st); err != nil {
		t.Fatal(err)
	}
	review := Review{ID: 1, State: "APPROVED", SHA: pull.Head.SHA}
	review.User.Login = "other"
	check := func(rs []Review) {
		t.Helper()
		if err := s.observe(repo, pull, rs); err != nil {
			t.Fatal(err)
		}
		if err := s.postApproval(context.Background(), repo, 42); err != nil {
			t.Fatal(err)
		}
	}
	check([]Review{review}) // Wrong reviewer.
	review.User.Login = "reviewer"
	review.SHA = strings.Repeat("b", 40)
	check([]Review{review}) // Old head.
	review.SHA = pull.Head.SHA
	pull.Draft = true
	check([]Review{review})
	pull.Draft = false
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatal("hook ran before valid approval")
	}
	check([]Review{review})
	check([]Review{review})
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = NewService(cfg, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	check([]Review{review})
	// Reapproval of the same SHA is still one event, even after dismissal.
	review.State = "DISMISSED"
	check([]Review{review})
	review.ID = 2
	review.State = "APPROVED"
	check([]Review{review})
	pull.Head.SHA = strings.Repeat("c", 40)
	review.ID = 3
	review.SHA = pull.Head.SHA
	check([]Review{review})
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimSpace(data), []byte("\n"))
	if len(lines) != 2 {
		t.Fatalf("expected 2 events: %s", data)
	}
	var event ApprovalEvent
	if err := json.Unmarshal(lines[1], &event); err != nil {
		t.Fatal(err)
	}
	if event.SHA != pull.Head.SHA || event.ReviewID != 3 || event.Event != "postApproval" || event.Repository != repo.Repo {
		t.Fatalf("bad payload: %+v", event)
	}
	st, _ = s.store.load()
	runs := st.Work[key(repo.Repo, 42)].Approvals
	if len(runs) != 2 || runs[0].State != "succeeded" || runs[1].State != "succeeded" {
		t.Fatalf("bad hook outcomes: %+v", runs)
	}
}

func TestApprovalHookFailureNotReplayed(t *testing.T) {
	for _, mode := range []string{"failed", "unknown", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			script := filepath.Join(dir, "hook")
			body := "#!/bin/sh\nexit 1\n"
			if mode == "timeout" {
				body = "#!/bin/sh\nsleep 10\n"
			}
			if err := os.WriteFile(script, []byte(body), 0700); err != nil {
				t.Fatal(err)
			}
			repo := Repository{Repo: "acme/project", ReviewerLogin: "reviewer", PostApproval: &ApprovalHook{Command: []string{script}, TimeoutSeconds: 1}}
			s, err := NewService(Config{}, filepath.Join(dir, stateDatabase), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			var pull Pull
			pull.Number = 42
			pull.State = "open"
			pull.Head.SHA = strings.Repeat("a", 40)
			review := Review{ID: 1, State: "APPROVED", SHA: pull.Head.SHA}
			review.User.Login = "reviewer"
			work := &Work{Repo: repo.Repo, Pull: pull, Linked: true, Reviewer: Assignee{GitHubLogin: "reviewer"}, Reviews: []Review{review}}
			work.derive()
			if mode == "unknown" {
				work.Approvals = []ApprovalRun{{ApprovalEvent: ApprovalEvent{SHA: pull.Head.SHA}, State: "unknown"}}
			}
			st, _ := s.store.load()
			st.Work[key(repo.Repo, 42)] = work
			if err := s.store.save(st); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 2; i++ {
				if err := s.postApproval(context.Background(), repo, 42); err != nil {
					t.Fatal(err)
				}
			}
			st, _ = s.store.load()
			runs := st.Work[key(repo.Repo, 42)].Approvals
			want := "failed"
			if mode == "unknown" {
				want = "unknown"
			}
			if len(runs) != 1 || runs[0].State != want {
				t.Fatalf("unexpected runs: %+v", runs)
			}
		})
	}
}

func TestSQLiteV1RegistrationMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), stateDatabase)
	s, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	original := storedFixture()
	if err := s.save(original); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("DROP TABLE repositories; PRAGMA user_version=1"); err != nil {
		t.Fatal(err)
	}
	s.db.Close()
	s, err = openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.db.Close()
	checkState(t, s, original)
	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != schemaVersion {
		t.Fatal(version, err)
	}
}
