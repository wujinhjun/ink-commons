package ink

import (
	"strings"
	"time"

	"ink-commons/internal/execution"
)

type Assignee struct {
	GitHubLogin string           `json:"github_login,omitempty"`
	Route       *execution.Route `json:"route,omitempty"`
}

type Push struct {
	Repo  string          `json:"repo"`
	Ref   string          `json:"ref"`
	SHA   string          `json:"sha"`
	Route execution.Route `json:"route"`
	At    time.Time       `json:"at"`
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
	Repo      string           `json:"repo"`
	Pull      Pull             `json:"pull_request"`
	Origin    *execution.Route `json:"origin,omitempty"`
	Linked    bool             `json:"linked"`
	Developer Assignee         `json:"developer"`
	Reviewer  Assignee         `json:"reviewer"`
	Phase     string           `json:"phase"`
	Task      *Task            `json:"task,omitempty"`
	Reviews   []Review         `json:"reviews,omitempty"`
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
