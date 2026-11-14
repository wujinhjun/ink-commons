package ink

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

type Route struct {
	Project    string `json:"project"`
	SessionKey string `json:"session_key"`
}

func (r Route) valid() bool {
	platform, remainder, ok := strings.Cut(r.SessionKey, ":")
	channel, user, hasUser := strings.Cut(remainder, ":")
	return strings.TrimSpace(r.Project) != "" && ok && platform == "discord" && snowflake(channel) && (!hasUser || snowflake(user))
}

func snowflake(value string) bool {
	return value != "" && strings.Trim(value, "0123456789") == ""
}

type Assignee struct {
	GitHubLogin string `json:"github_login,omitempty"`
	Route       *Route `json:"route,omitempty"`
}

type Push struct {
	Repo  string    `json:"repo"`
	Ref   string    `json:"ref"`
	SHA   string    `json:"sha"`
	Route Route     `json:"route"`
	At    time.Time `json:"at"`
}

type Pull struct {
	Number int    `json:"number"`
	URL    string `json:"html_url"`
	Title  string `json:"title"`
	State  string `json:"state"`
	Draft  bool   `json:"draft"`
	Merged bool   `json:"merged"`
	Head   struct {
		Ref  string `json:"ref"`
		SHA  string `json:"sha"`
		Repo *struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"head"`
}

type Review struct {
	ID          int64     `json:"id"`
	SubmittedAt time.Time `json:"submitted_at"`
	SHA         string    `json:"commit_id"`
	URL         string    `json:"html_url"`
	State       string    `json:"state"`
	User        struct {
		Login string `json:"login"`
	} `json:"user"`
}

type Task struct {
	Role      string `json:"role"`
	SHA       string `json:"sha"`
	ReviewID  int64  `json:"review_id,omitempty"`
	ReviewURL string `json:"review_url,omitempty"`
	Delivery  string `json:"delivery"`
	Error     string `json:"error,omitempty"`
	Attempts  int    `json:"attempts"`
}

type Work struct {
	Repo         string   `json:"repo"`
	Pull         Pull     `json:"pull_request"`
	Origin       *Route   `json:"origin,omitempty"`
	Linked       bool     `json:"linked"`
	Developer    Assignee `json:"developer"`
	Reviewer     Assignee `json:"reviewer"`
	ReviewThread string   `json:"review_thread_id,omitempty"`
	Phase        string   `json:"phase"`
	Task         *Task    `json:"task,omitempty"`
	Reviews      []Review `json:"reviews,omitempty"`
}

func (w *Work) derive() {
	if w.Pull.State == "closed" {
		w.Phase = "closed"
		if w.Pull.Merged {
			w.Phase = "merged"
		}
		w.Task = nil
		return
	}
	if !w.Linked {
		w.Phase, w.Task = "unclaimed", nil
		return
	}
	if w.Pull.Draft {
		w.Phase, w.Task = "draft", nil
		return
	}
	var latest *Review
	for i := range w.Reviews {
		r := &w.Reviews[i]
		if r.SHA == w.Pull.Head.SHA && w.Reviewer.GitHubLogin != "" && strings.EqualFold(r.User.Login, w.Reviewer.GitHubLogin) && (r.State == "APPROVED" || r.State == "CHANGES_REQUESTED") {
			latest = r
		}
	}
	if latest == nil {
		w.Phase = "review_pending"
		w.setTask(Task{Role: "reviewer", SHA: w.Pull.Head.SHA}, w.Reviewer)
		return
	}
	if latest.State == "APPROVED" {
		w.Phase, w.Task = "approved", nil
		return
	}
	w.Phase = "changes_requested"
	w.setTask(Task{Role: "developer", SHA: w.Pull.Head.SHA, ReviewID: latest.ID, ReviewURL: latest.URL}, w.Developer)
}

func (w *Work) setTask(task Task, assignee Assignee) {
	if w.Task != nil && w.Task.Role == task.Role && w.Task.SHA == task.SHA && w.Task.ReviewID == task.ReviewID {
		return
	}
	task.Delivery = "manual"
	if assignee.Route != nil {
		task.Delivery = "pending"
	}
	w.Task = &task
}

type state struct {
	Pushes []Push           `json:"pushes"`
	Work   map[string]*Work `json:"work"`
}

func openState(path string) (state, error) {
	s := state{Work: make(map[string]*Work)}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(data, &s); err != nil {
		return s, fmt.Errorf("read state: %w", err)
	}
	if s.Work == nil {
		return s, errors.New("state is missing its work index")
	}
	return s, nil
}

func saveState(path string, s state) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".ink-state-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	if err := json.NewEncoder(file).Encode(s); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), path)
}

func lockState(path string) (*os.File, error) {
	f, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("lock daemon state: %w", err)
	}
	return f, nil
}
