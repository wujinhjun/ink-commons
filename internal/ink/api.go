package ink

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"ink-commons/internal/execution"
)

type workChange struct {
	Role        string           `json:"role,omitempty"`
	GitHubLogin string           `json:"github_login,omitempty"`
	Route       *execution.Route `json:"route,omitempty"`
}

type apiError struct {
	status  int
	message string
}

func (e apiError) Error() string { return e.message }

func (s *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { reply(w, 200, map[string]string{"status": "ok"}) })
	mux.HandleFunc("GET /v1/work", s.listWork)
	mux.HandleFunc("GET /v1/work/{owner}/{repo}/{number}", s.work)
	mux.HandleFunc("POST /v1/work/{owner}/{repo}/{number}/{action}", s.work)
	mux.HandleFunc("POST /v1/pushes", s.pushes)
	return mux
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
	st, err := s.store.load()
	s.mu.Unlock()
	if err != nil {
		fail(w, err)
		return
	}
	list := make([]*Work, 0, len(st.Work))
	for _, work := range st.Work {
		list = append(list, work)
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].Repo != list[j].Repo {
			return list[i].Repo < list[j].Repo
		}
		return list[i].Pull.Number < list[j].Pull.Number
	})
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
	st, err := s.store.load()
	if err != nil {
		fail(w, err)
		return
	}
	now := time.Now()
	prunePushes(&st, now)
	s.recordPushes(&st, input, now)
	if err := s.store.save(st); err != nil {
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
	st, err := s.store.load()
	if err != nil {
		fail(w, err)
		return
	}
	work := st.Work[key(repo, number)]
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
		requested := Assignee{GitHubLogin: strings.TrimSpace(change.GitHubLogin), Route: change.Route}
		if requested.GitHubLogin != "" && sameReviewer(work.Reviewer, requested) {
			reply(w, 200, work)
			return
		}
	}
	if s.delivering[key(repo, number)] {
		fail(w, apiError{409, "delivery in progress"})
		return
	}
	if err := applyChange(work, action, change); err != nil {
		fail(w, err)
		return
	}
	if err := s.store.save(st); err != nil {
		fail(w, err)
		return
	}
	reply(w, 200, work)
}

func applyChange(work *Work, action string, change workChange) error {
	assignee := Assignee{GitHubLogin: strings.TrimSpace(change.GitHubLogin), Route: change.Route}
	switch action {
	case "link":
		if assignee.GitHubLogin == "" && assignee.Route == nil {
			return apiError{400, "specify a developer login or an execution destination"}
		}
		if work.Linked {
			return apiError{409, "PR already linked"}
		}
		work.Linked, work.Origin, work.Developer = true, change.Route, assignee
	case "assign":
		if !work.Linked {
			return apiError{409, "link the PR first"}
		}
		if assignee.GitHubLogin == "" && (change.Role == "reviewer" || assignee.Route == nil) {
			return apiError{400, "specify the assignee GitHub login"}
		}
		if change.Role == "developer" {
			work.Developer = assignee
		} else if change.Role == "reviewer" {
			work.Reviewer = assignee
		} else {
			return apiError{400, "role must be developer or reviewer"}
		}
		if work.Task != nil && work.Task.Role == change.Role {
			work.Task = nil
		}
	case "retry":
		if work.Task == nil || (work.Task.Delivery != "unknown" && work.Task.Delivery != "accepted") {
			return apiError{409, "no retryable task"}
		}
		work.Task.Delivery, work.Task.Error = "pending", ""
	default:
		return apiError{404, fmt.Sprintf("unknown action %q", action)}
	}
	work.derive()
	return nil
}

func sameReviewer(current, requested Assignee) bool {
	if !strings.EqualFold(current.GitHubLogin, requested.GitHubLogin) {
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
