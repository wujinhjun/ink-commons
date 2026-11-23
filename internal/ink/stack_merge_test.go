package ink

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testStackRequestID = "630b9d5e-3f2a-4f7e-8b0c-2d5f9a8c1e42"

func (f *autoMergeFixture) setStack(position int, base string) {
	f.pull.Base.Ref = base
	f.pull.Stack = &PullStack{ID: 13, Position: position, Size: 2, Base: PullBase{Ref: "main"}}
}

func (f *autoMergeFixture) stackTransport(t *testing.T, handle func(http.ResponseWriter, *http.Request)) {
	t.Helper()
	f.installTransport(t)
	fallback := f.s.up.http.Transport
	f.s.up.http.Transport = testTransport(func(r *http.Request) (*http.Response, error) {
		if !strings.Contains(r.URL.Path, "/merge-async") {
			return fallback.RoundTrip(r)
		}
		if r.Header.Get("X-GitHub-Api-Version") != stackAPIVersion {
			t.Fatalf("missing Stack API version on %s", r.URL)
		}
		w := httptest.NewRecorder()
		handle(w, r)
		return w.Result(), nil
	})
}

func TestStackMergePollsPersistedRequestAfterRestart(t *testing.T) {
	f := newAutoMergeFixture(t)
	f.setStack(1, "main")
	ctx := context.Background()
	path := stackMergePath(f.repo, 42)
	submits, polls := 0, 0
	handler := func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == path && r.Method == http.MethodPut:
			submits++
			var input map[string]string
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				t.Fatal(err)
			}
			if input["sha"] != f.pull.Head.SHA || input["merge_method"] != "merge" || input["merge_action"] != "direct_merge" || input["bypass_rules"] != "" {
				t.Fatalf("unexpected merge options: %v", input)
			}
			work := notificationWork(t, f.s, f.repo, f.pull)
			if len(work.AutoMerges) != 1 || work.AutoMerges[0].State != "unknown" {
				t.Fatalf("no reservation before submission: %+v", work.AutoMerges)
			}
			reply(w, 202, map[string]any{"status": "pending", "details": map[string]string{"uuid": testStackRequestID}})
		case r.URL.Path == path+"/"+testStackRequestID && r.Method == http.MethodGet:
			polls++
			if polls == 1 {
				w.WriteHeader(503)
				return
			}
			reply(w, 200, map[string]any{"status": "merged", "details": map[string]string{"sha": strings.Repeat("d", 40)}})
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL)
		}
	}
	f.stackTransport(t, handler)
	if err := f.s.autoMerge(ctx, f.repo, 42); err != nil {
		t.Fatal(err)
	}
	work := notificationWork(t, f.s, f.repo, f.pull)
	if run := work.AutoMerges[0]; run.State != "pending" || run.RequestID != testStackRequestID || run.MergeSHA != "" {
		t.Fatalf("unexpected pending run: %+v", run)
	}
	if status := mergeStatus(work, f.repo, f.pull.Head.SHA); status.State != "waiting" {
		t.Fatalf("pending request presented as %+v", status)
	}

	f.s.Close()
	disabled := f.repo
	disabled.AutoMerge = &AutoMergeConfig{}
	s, err := NewService(Config{Repositories: []Repository{disabled}}, f.path, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.s = s
	f.stackTransport(t, handler)
	if err := s.autoMerge(ctx, disabled, 42); err != nil {
		t.Fatal(err)
	}
	work = notificationWork(t, s, f.repo, f.pull)
	if run := work.AutoMerges[0]; run.State != "unknown" || run.RequestID != testStackRequestID {
		t.Fatalf("transient error lost the request: %+v", run)
	}
	if err := s.autoMerge(ctx, disabled, 42); err != nil {
		t.Fatal(err)
	}
	work = notificationWork(t, s, f.repo, f.pull)
	if run := work.AutoMerges[0]; run.State != "merged" || run.MergeSHA != strings.Repeat("d", 40) || submits != 1 || polls != 2 || f.writes != 0 {
		t.Fatalf("run=%+v submits=%d polls=%d sync=%d", run, submits, polls, f.writes)
	}
}

func TestStackMergeWaitsForRetargetAndApprovalOfRebasedHead(t *testing.T) {
	f := newAutoMergeFixture(t)
	f.setStack(2, "feature/parent")
	ctx := context.Background()
	submits := 0
	f.stackTransport(t, func(w http.ResponseWriter, r *http.Request) {
		submits++
		var input map[string]string
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Fatal(err)
		}
		if input["sha"] != f.pull.Head.SHA {
			t.Fatalf("wrong merge head: %q", input["sha"])
		}
		f.pull.State, f.pull.Merged = "closed", true
		f.pull.MergeCommitSHA = strings.Repeat("d", 40)
		reply(w, 200, map[string]any{"status": "merged", "details": map[string]string{"sha": f.pull.MergeCommitSHA}})
	})
	poll := func() {
		t.Helper()
		if err := f.s.observe(f.repo, f.pull, f.reviews); err != nil {
			t.Fatal(err)
		}
		if err := f.s.autoMerge(ctx, f.repo, 42); err != nil {
			t.Fatal(err)
		}
	}
	poll()
	work := notificationWork(t, f.s, f.repo, f.pull)
	if run := work.AutoMerges[0]; run.State != "waiting" || !strings.Contains(run.Reason, "bottom") {
		t.Fatalf("upper stack member did not wait: %+v", run)
	}
	f.pull.Base.Ref = "main"
	f.pull.Head.SHA = strings.Repeat("e", 40)
	poll()
	if submits != 0 || f.writes != 0 {
		t.Fatal("merged a stack member before its current head was approved")
	}
	f.reviews[0].SHA = f.pull.Head.SHA
	poll()
	work = notificationWork(t, f.s, f.repo, f.pull)
	if run := work.AutoMerges[len(work.AutoMerges)-1]; submits != 1 || f.writes != 0 || run.State != "merged" || run.SHA != f.pull.Head.SHA {
		t.Fatalf("stack submits=%d synchronous writes=%d run=%+v", submits, f.writes, run)
	}
}

func TestStackMembershipChangeDuringPreflightPreventsMerge(t *testing.T) {
	f := newAutoMergeFixture(t)
	fallback := f.s.up.http.Transport
	f.s.up.http.Transport = testTransport(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/pulls/42") && f.reads == 1 {
			f.setStack(2, "feature/parent")
		}
		return fallback.RoundTrip(r)
	})
	if err := f.s.autoMerge(context.Background(), f.repo, 42); err != nil {
		t.Fatal(err)
	}
	work := notificationWork(t, f.s, f.repo, f.pull)
	if run := work.AutoMerges[0]; run.State != "waiting" || run.Reason != "PR changed during preflight" || f.writes != 0 {
		t.Fatalf("stack change did not block the write: %+v", run)
	}
}

func TestStackMergeOnlyAdoptsConflictForMatchingRequest(t *testing.T) {
	for _, matching := range []bool{false, true} {
		name := "mismatched head"
		if matching {
			name = "same request options"
		}
		t.Run(name, func(t *testing.T) {
			f := newAutoMergeFixture(t)
			f.setStack(1, "main")
			head := strings.Repeat("e", 40)
			if matching {
				head = f.pull.Head.SHA
			}
			submits := 0
			f.stackTransport(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.Method {
				case http.MethodPut:
					submits++
					reply(w, 409, map[string]any{"status": "pending", "details": map[string]string{
						"uuid": testStackRequestID, "expected_head_sha": head, "merge_method": "merge", "merge_action": "direct_merge",
					}})
				case http.MethodGet:
					if !matching {
						t.Fatal("polled a request for a different head")
					}
					reply(w, 200, map[string]string{"status": "failed"})
				}
			})
			for i := 0; i < 3; i++ {
				if err := f.s.autoMerge(context.Background(), f.repo, 42); err != nil {
					t.Fatal(err)
				}
			}
			work := notificationWork(t, f.s, f.repo, f.pull)
			run := work.AutoMerges[0]
			if submits != 1 || (matching && run.State != "failed") || (!matching && (run.State != "unknown" || run.RequestID != "")) {
				t.Fatalf("unexpected result: %+v, submits %d", run, submits)
			}
		})
	}
}

func TestStackMergeRejectedBeforeAcceptanceReturnsToPreflight(t *testing.T) {
	f := newAutoMergeFixture(t)
	f.setStack(1, "main")
	ctx := context.Background()
	submits := 0
	f.stackTransport(t, func(w http.ResponseWriter, r *http.Request) {
		submits++
		if submits == 1 {
			f.pull.Draft = true
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		reply(w, 202, map[string]any{"status": "pending", "details": map[string]string{"uuid": testStackRequestID}})
	})
	step := func(state string) {
		t.Helper()
		if err := f.s.autoMerge(ctx, f.repo, 42); err != nil {
			t.Fatal(err)
		}
		work := notificationWork(t, f.s, f.repo, f.pull)
		if run := work.AutoMerges[0]; run.State != state {
			t.Fatalf("expected %s, got %+v", state, run)
		}
	}
	step("waiting")
	step("waiting")
	f.pull.Draft = false
	f.checkState = "in_progress"
	step("waiting")
	if submits != 1 {
		t.Fatalf("submitted %d times before preflight passed", submits)
	}
	f.checkState = "completed"
	step("pending")
	if submits != 2 {
		t.Fatalf("submitted %d times, want 2", submits)
	}
}

func TestStackMergeAlreadyMergedResponseRequiresMatchingClosedHead(t *testing.T) {
	for _, scenario := range []struct {
		name, want string
		closed     bool
		changeHead bool
	}{
		{name: "approved head merged", want: "merged", closed: true},
		{name: "different head merged", want: "superseded", closed: true, changeHead: true},
		{name: "closed state not yet observed", want: "unknown"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			f := newAutoMergeFixture(t)
			f.setStack(1, "main")
			approved := f.pull.Head.SHA
			mergeSHA := strings.Repeat("d", 40)
			f.stackTransport(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPut {
					t.Fatalf("unexpected request: %s %s", r.Method, r.URL)
				}
				if scenario.closed {
					f.pull.State, f.pull.Merged = "closed", true
					f.pull.MergeCommitSHA = mergeSHA
				}
				if scenario.changeHead {
					f.pull.Head.SHA = strings.Repeat("e", 40)
				}
				reply(w, 200, map[string]any{"status": "merged", "details": map[string]string{"sha": mergeSHA}})
			})
			if err := f.s.autoMerge(context.Background(), f.repo, 42); err != nil {
				t.Fatal(err)
			}
			work := notificationWork(t, f.s, f.repo, f.pull)
			run := work.AutoMerges[0]
			if f.reads != 3 || run.State != scenario.want || run.SHA != approved {
				t.Fatalf("reads=%d, run=%+v", f.reads, run)
			}
			notifications := 0
			for _, n := range work.Notifications {
				if n.Event == "pr_merged" && n.SHA == approved {
					notifications++
				}
			}
			if scenario.want == "merged" {
				if run.MergeSHA != mergeSHA || notifications != 1 {
					t.Fatalf("run=%+v, notifications=%d", run, notifications)
				}
			} else if run.MergeSHA != "" || notifications != 0 {
				t.Fatalf("attributed an unverified merge: run=%+v, notifications=%d", run, notifications)
			}
		})
	}
}

func TestSharedMergeAppIsOptInAndPollingUsesRecordedMode(t *testing.T) {
	f := newAutoMergeFixture(t)
	f.setStack(1, "main")
	ctx := context.Background()
	dir := t.TempDir()
	write := func(name string, value any) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), jsonValue(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("pull", f.pull)
	write("reviews", f.reviews)
	write("checks", map[string]any{"check_runs": []any{}})
	write("accepted", map[string]any{"status": "pending", "details": map[string]string{"uuid": testStackRequestID}})
	write("result", map[string]any{"status": "merged", "details": map[string]string{"sha": strings.Repeat("d", 40)}})
	t.Setenv("INK_TEST_SHARED_APP_DIR", dir)
	command := filepath.Join(dir, "reader")
	script := `#!/bin/sh
set -eu
dir=$INK_TEST_SHARED_APP_DIR
test "$1" = "--profile"
test "$3" = "-R" && test "$4" = "acme/project"
test "$5" = "api" && test "$6" = "--include" && test "$7" = "--method"
case "$2 $8 $9" in
    'inspect GET /repos/acme/project/pulls/42') file=pull ;;
    'inspect GET /repos/acme/project/pulls/42/reviews?'*) file=reviews ;;
    'inspect GET /repos/acme/project/commits/'*'/check-runs?'*) file=checks ;;
    'code PUT /repos/acme/project/pulls/42/merge-async')
        cat > "$dir/submission"
        printf 'HTTP/1.1 202 Accepted\r\n\r\n'
        cat "$dir/accepted"
        exit 0 ;;
    'code GET /repos/acme/project/pulls/42/merge-async/630b9d5e-3f2a-4f7e-8b0c-2d5f9a8c1e42')
        file=result ;;
    *) exit 2 ;;
esac
printf 'HTTP/1.1 200 OK\r\n\r\n'
cat "$dir/$file"
`
	if err := os.WriteFile(command, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	f.s.up.command = command
	if err := f.s.autoMerge(ctx, f.repo, 42); err != nil {
		t.Fatal(err)
	}
	run := notificationWork(t, f.s, f.repo, f.pull).AutoMerges[0]
	if run.State != "waiting" || !strings.Contains(run.Reason, "dedicated") {
		t.Fatalf("default used or failed to require dedicated identity: %+v", run)
	}
	optIn := `{"enabled":true,"reuse_reader_identity":true}`
	configAPI(t, f.s, "POST", f.repo.Repo, "auto_merge", optIn, 200)
	shared, _ := f.s.repo(f.repo.Repo)
	if err := f.s.autoMerge(ctx, shared, 42); err != nil {
		t.Fatal(err)
	}
	run = notificationWork(t, f.s, f.repo, f.pull).AutoMerges[0]
	if run.State != "pending" || !run.ReuseReaderIdentity || run.RequestID != testStackRequestID {
		t.Fatalf("shared identity did not reserve a pending request: %+v", run)
	}
	body, err := os.ReadFile(filepath.Join(dir, "submission"))
	if err != nil {
		t.Fatal(err)
	}
	var submission map[string]any
	if err := json.Unmarshal(body, &submission); err != nil || submission["sha"] != f.pull.Head.SHA ||
		submission["merge_action"] != stackMergeAction || submission["bypass_rules"] != nil {
		t.Fatalf("unexpected submission: %s (%v)", body, err)
	}

	configAPI(t, f.s, "POST", f.repo.Repo, "auto_merge", `{"enabled":false}`, 200)
	f.s.Close()
	s, err := NewService(Config{GitHubCommand: command}, f.path, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.s = s
	disabled, _ := s.repo(f.repo.Repo)
	if err := s.autoMerge(ctx, disabled, 42); err != nil {
		t.Fatal(err)
	}
	run = notificationWork(t, s, f.repo, f.pull).AutoMerges[0]
	if run.State != "merged" || run.MergeSHA != strings.Repeat("d", 40) || !run.ReuseReaderIdentity {
		t.Fatalf("pending request lost its original mode after policy change and restart: %+v", run)
	}
}
