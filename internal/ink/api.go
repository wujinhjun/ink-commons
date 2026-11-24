//go:build linux || darwin

package ink

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/wujinhjun/ink-commons/internal/execution"
)

type workChange struct {
	GitHubCommand string           `json:"github_command,omitempty"`
	TaskID        string           `json:"task_id,omitempty"`
	AttemptID     string           `json:"attempt_id,omitempty"`
	Stopped       bool             `json:"stopped,omitempty"`
	Reason        string           `json:"reason,omitempty"`
	Role          string           `json:"role,omitempty"`
	GitHubLogin   string           `json:"github_login,omitempty"`
	Route         *execution.Route `json:"route,omitempty"`
}

type apiError struct {
	status  int
	message string
}

func (e apiError) Error() string { return e.message }

func (s *Service) Handler() http.Handler {
	const workPath = "/v1/work/{owner}/{repo}/{number}"
	const taskPath = "/v1/tasks/{task}"
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		gate := "closed"
		if s.pollReady() {
			gate = "open"
		}
		reply(w, 200, map[string]string{"status": "ok", "revision": BuildRevision, "gate": gate,
			"last_poll": timestamp(s.lastPoll.Load()), "last_github_ok": timestamp(s.up.lastOK.Load())})
	})
	mux.HandleFunc("GET /v1/self-check", s.selfCheck)
	mux.HandleFunc("GET /v1/config", s.configuration)
	mux.HandleFunc("GET /v1/config/export", s.exportConfiguration)
	mux.HandleFunc("POST /v1/config", s.configuration)
	mux.HandleFunc("DELETE /v1/config", s.configuration)
	mux.HandleFunc("GET /v1/repositories", s.listRepositories)
	mux.HandleFunc("POST /v1/repositories", s.registerRepository)
	mux.HandleFunc("GET /v1/work", s.listWork)
	mux.HandleFunc("GET /v1/workspaces", s.workspaces)
	mux.HandleFunc("POST /v1/workspaces", s.workspaces)
	mux.HandleFunc("POST /v1/workspaces/{workspace}/{action}", s.workspaces)
	mux.HandleFunc("GET "+workPath, s.work)
	mux.HandleFunc("GET "+workPath+"/context", s.reviewContext)
	mux.HandleFunc("POST "+workPath+"/{action}", s.work)
	mux.HandleFunc("GET "+taskPath, s.handoffTask)
	mux.HandleFunc("POST "+taskPath+"/{action}", s.handoffTask)
	mux.HandleFunc("POST /v1/pushes", s.pushes)
	mux.HandleFunc("POST /v1/notifications/ack", s.ackNotification)
	mux.HandleFunc("GET /v1/notifications/status", s.notificationStatus)

	pausedWrites := map[string]bool{"POST /v1/config": true, "DELETE /v1/config": true}
	allowWhilePaused := func(path, action string, handle http.HandlerFunc) {
		pattern := "POST " + path + "/" + action
		pausedWrites[pattern] = true
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			r.SetPathValue("action", action)
			handle(w, r)
		})
	}
	allowWhilePaused(workPath, "assign", s.work)
	allowWhilePaused(workPath, "confirm-stopped", s.work)
	allowWhilePaused(taskPath, "submit", s.handoffTask)
	allowWhilePaused(taskPath, "retry", s.handoffTask)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && !s.pollReady() {
			_, pattern := mux.Handler(r)
			if !s.config.ConfigOnly || !pausedWrites[pattern] {
				fail(w, apiError{503, "deployment verification in progress"})
				return
			}
		}
		mux.ServeHTTP(w, r)
	})
}

func (s *Service) pollReady() bool {
	if s.config.ConfigOnly {
		return false
	}
	if s.config.PollGate == "" {
		return true
	}
	_, err := os.Stat(s.config.PollGate)
	return err == nil
}

func timestamp(unix int64) string {
	if unix == 0 {
		return ""
	}
	return time.Unix(unix, 0).UTC().Format(time.RFC3339)
}

// 验证常驻进程自己的凭据、代理环境和 PR 读权限；不触发业务轮询或派发。
func (s *Service) selfCheck(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	for _, repo := range s.repositories() {
		var pulls []Pull
		if _, err := s.up.gitHubGet(ctx, "/repos/"+repo.Repo+"/pulls?state=open&per_page=1", &pulls); err != nil {
			reply(w, 503, map[string]string{"repository": repo.Repo, "error": err.Error()})
			return
		}
	}
	reply(w, 200, map[string]string{"status": "ok"})
}

func reply(w http.ResponseWriter, status int, result any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(result)
}

func fail(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	message := "state operation failed"
	var known apiError
	if errors.As(err, &known) {
		status, message = known.status, known.message
	}
	reply(w, status, map[string]string{"error": message})
}

func decode(w http.ResponseWriter, r *http.Request, result any) error {
	body := http.MaxBytesReader(w, r.Body, 1<<20)
	defer body.Close()
	d := json.NewDecoder(body)
	d.DisallowUnknownFields()
	if err := d.Decode(result); err != nil {
		return apiError{400, "invalid JSON request"}
	}
	var extra json.RawMessage
	if d.Decode(&extra) != io.EOF {
		return apiError{400, "expected one JSON value"}
	}
	return nil
}

func (s *Service) listWork(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	list, err := s.store.listWork()
	s.mu.Unlock()
	if err != nil {
		fail(w, err)
		return
	}
	reply(w, 200, list)
}

func (s *Service) pushes(w http.ResponseWriter, r *http.Request) {
	var input []Push
	if err := decode(w, r, &input); err != nil {
		fail(w, err)
		return
	}
	for i := range input {
		p := &input[i]
		p.Repo = strings.ToLower(p.Repo)
		if !repoName.MatchString(p.Repo) || p.Ref == "" || !validSHA(p.SHA) || execution.ValidateDestination(s.providers, p.Route, false) != nil {
			fail(w, apiError{400, "invalid push observation"})
			return
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.recordPushes(input, s.now()); err != nil {
		fail(w, err)
		return
	}
	reply(w, 202, map[string]string{"status": "recorded"})
}

func (s *Service) work(w http.ResponseWriter, r *http.Request) {
	repo := strings.ToLower(r.PathValue("owner") + "/" + r.PathValue("repo"))
	number, err := strconv.Atoi(r.PathValue("number"))
	if err != nil || number <= 0 {
		fail(w, apiError{400, "invalid PR number"})
		return
	}
	action := r.PathValue("action")
	var change workChange
	if r.Method == http.MethodPost && action != "retry" {
		if err := decode(w, r, &change); err != nil {
			fail(w, err)
			return
		}
		if change.Route != nil {
			allocate := action == "assign" && change.Role == "reviewer"
			if err := execution.ValidateDestination(s.providers, *change.Route, allocate); err != nil {
				fail(w, apiError{400, "invalid execution destination: " + err.Error()})
				return
			}
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	work, err := s.store.loadWork(key(repo, number))
	if err != nil {
		fail(w, err)
		return
	}
	if work == nil {
		fail(w, apiError{404, "PR not observed yet"})
		return
	}
	if r.Method == http.MethodGet {
		reply(w, 200, work)
		return
	}
	// Return an existing reviewer assignment even during delivery so a
	// repeated request cannot reset the task or allocate another session.
	if action == "assign" && change.Role == "reviewer" && work.Linked {
		requested := Assignee{GitHubLogin: strings.TrimSpace(change.GitHubLogin), GitHubCommand: strings.TrimSpace(change.GitHubCommand), Route: change.Route}
		if requested.GitHubLogin != "" && sameReviewer(work.Reviewer, requested) {
			// Equal values still express an explicit PR override. Preserve the active
			// task/session while recording that future rounds must not inherit.
			if work.ReviewerOverride == nil || !bytes.Equal(jsonValue(work.ReviewerOverride), jsonValue(requested)) {
				work.ReviewerOverride = cloneAssignee(requested)
				work.Version++
				if err := s.store.saveWork(work); err != nil {
					fail(w, err)
					return
				}
			}
			reply(w, 200, work)
			return
		}
	}
	if s.delivering[key(repo, number)] {
		fail(w, apiError{409, "delivery in progress"})
		return
	}
	if action == "assign" && !work.Linked {
		fail(w, apiError{409, "link the PR first"})
		return
	}
	if action == "assign" && change.Role == "reviewer" && change.Route != nil {
		route := *change.Route
		preparation := execution.Delivery{Role: "reviewer", Source: reviewSource(work)}
		if repository, ok := s.repo(repo); ok {
			route = allocationRoute(repository, route)
			repository.ReviewerRoute = change.Route
			if err := repository.ValidateReviewFeatures(); err != nil {
				fail(w, apiError{400, err.Error()})
				return
			}
			if repository.Review != nil && repository.Review.Context.ThreadHistory && (reviewSource(work) == nil || reviewSource(work).Provider != "cc-connect") {
				fail(w, apiError{400, "thread history requires a Discord development source"})
				return
			}
			if repository.Review != nil {
				preparation.RequireSameChannel = repository.Review.RequireSameChannel
				preparation.ReuseDevelopmentThread = repository.Review.ReuseDevelopmentThread
			}
		}
		if route.Session == "" || preparation.RequireSameChannel {
			if err := execution.ValidatePreparation(s.providers, route, preparation); err != nil {
				fail(w, apiError{400, err.Error()})
				return
			}
		}
	}
	if err := applyChange(work, action, change, s.now()); err != nil {
		fail(w, err)
		return
	}
	s.refreshWorkflow(work, s.now())
	if err := s.store.saveWork(work); err != nil {
		fail(w, err)
		return
	}
	reply(w, 200, work)
}

func applyChange(work *Work, action string, change workChange, now time.Time) error {
	assignee := Assignee{GitHubLogin: strings.TrimSpace(change.GitHubLogin), GitHubCommand: strings.TrimSpace(change.GitHubCommand), Route: change.Route}
	switch action {
	case "link":
		if assignee.GitHubLogin == "" && assignee.Route == nil {
			return apiError{400, "specify a developer login or an execution destination"}
		}
		if work.Linked {
			return apiError{409, "PR already linked"}
		}
		work.Linked, work.Origin, work.Developer = true, change.Route, assignee
		work.DeveloperOverride = cloneAssignee(assignee)
	case "assign":
		if !work.Linked {
			return apiError{409, "link the PR first"}
		}
		if assignee.GitHubLogin == "" && (change.Role == "reviewer" || assignee.Route == nil) {
			return apiError{400, "specify the assignee GitHub login"}
		}
		if change.Role == "developer" {
			work.Developer = assignee
			work.DeveloperOverride = cloneAssignee(assignee)
		} else if change.Role == "reviewer" {
			work.Reviewer = assignee
			work.ReviewerOverride = cloneAssignee(assignee)
			work.RoundConfig = nil
		} else {
			return apiError{400, "role must be developer or reviewer"}
		}
		if work.Task != nil && work.Task.Role == change.Role {
			work.replaceTask(nil)
		}
	case "confirm-stopped":
		if !change.Stopped || strings.TrimSpace(change.Reason) == "" || change.TaskID == "" || change.AttemptID == "" {
			return apiError{400, "task_id, attempt_id, stopped=true and reason are required"}
		}
		found := false
		for _, t := range work.executionTasks() {
			if t.ID != change.TaskID {
				continue
			}
			for i := range t.Runs {
				a := &t.Runs[i]
				if a.ID != change.AttemptID {
					continue
				}
				if execution.Terminal(a.State) {
					return apiError{409, "attempt already ended"}
				}
				a.State, a.EndedAt, a.ManualReason, a.Error = "stopped", now, change.Reason, ""
				found = true
			}
		}
		if !found {
			return apiError{404, "execution attempt not found"}
		}
	case "retry":
		if work.Task != nil && work.Task.Handoff != nil {
			return apiError{409, "use task submit/status; reassign a blocked task only after handoff"}
		}
		if work.executionBlocked() {
			return apiError{409, "confirm all previous executions stopped before retry"}
		}
		if work.Task == nil || (work.Task.Delivery != "unknown" && work.Task.Delivery != "accepted") {
			return apiError{409, "no retryable task"}
		}
		work.Task.Delivery, work.Task.Error, work.Task.RetryAt = "pending", "", time.Time{}
		work.Task.AttemptsExhausted = false
	default:
		return apiError{404, fmt.Sprintf("unknown action %q", action)}
	}
	work.Version++
	work.derive(now)
	return nil
}

func sameReviewer(current, requested Assignee) bool {
	if current.GitHubCommand != requested.GitHubCommand || !strings.EqualFold(current.GitHubLogin, requested.GitHubLogin) {
		return false
	}
	if current.Route == nil || requested.Route == nil {
		return current.Route == nil && requested.Route == nil
	}
	want := *requested.Route
	if want.Session == "" {
		want.Session = current.Route.Session
	}
	return *current.Route == want
}
