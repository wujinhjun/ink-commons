package ink

import (
	"github.com/google/uuid"
	"strings"
	"time"

	"ink-commons/internal/execution"
)

type Assignee struct {
	GitHubCommand string           `json:"github_command,omitempty"`
	GitHubLogin   string           `json:"github_login,omitempty"`
	Route         *execution.Route `json:"route,omitempty"`
}

type Push struct {
	Repo  string          `json:"repo"`
	Ref   string          `json:"ref"`
	SHA   string          `json:"sha"`
	Route execution.Route `json:"route"`
	At    time.Time       `json:"at"`
}

type Pull struct {
	Mergeable      *bool  `json:"mergeable,omitempty"`
	MergeableState string `json:"mergeable_state,omitempty"`
	MergeCommitSHA string `json:"merge_commit_sha,omitempty"`
	Base           struct {
		SHA string `json:"sha"`
	} `json:"base"`
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

type Attempt struct {
	StopRequestedAt time.Time          `json:"stop_requested_at,omitempty"`
	RecoveryReason  string             `json:"recovery_reason,omitempty"`
	RetryAt         time.Time          `json:"retry_at,omitempty"`
	State           string             `json:"state"`
	Legacy          bool               `json:"legacy,omitempty"`
	ID              string             `json:"attempt_id"`
	Number          int                `json:"number"`
	Route           execution.Route    `json:"route"`
	Request         execution.Delivery `json:"request"`
	Receipt         execution.Receipt  `json:"receipt"`
	CreatedAt       time.Time          `json:"created_at"`
	EndedAt         time.Time          `json:"ended_at,omitempty"`
	Error           string             `json:"error,omitempty"`
	ManualReason    string             `json:"manual_reason,omitempty"`
}

type Task struct {
	Handoff   *TaskHandoff `json:"handoff,omitempty"`
	CreatedAt time.Time    `json:"created_at,omitempty"`
	ID        string       `json:"task_id,omitempty"`
	Runs      []Attempt    `json:"runs,omitempty"`
	RetryAt   time.Time    `json:"retry_at,omitempty"`
	Role      string       `json:"role"`
	SHA       string       `json:"sha"`
	ReviewID  int64        `json:"review_id,omitempty"`
	ReviewURL string       `json:"review_url,omitempty"`
	Delivery  string       `json:"delivery"`
	Error     string       `json:"error,omitempty"`
	Attempts  int          `json:"attempts"`
}

type Alert struct {
	ID        string    `json:"id"`
	Code      string    `json:"code"`
	Message   string    `json:"message"`
	SHA       string    `json:"sha"`
	Active    bool      `json:"active"`
	CreatedAt time.Time `json:"created_at"`
}
type Workflow struct {
	State  string `json:"state"`
	Reason string `json:"reason,omitempty"`
}
type Work struct {
	TaskMode      string            `json:"task_mode,omitempty"`
	Version       uint64            `json:"version,omitempty"`
	Review        *ReviewFeatures   `json:"review,omitempty"`
	AutoMerges    []AutoMergeRun    `json:"auto_merges,omitempty"`
	Notifications []NotificationRun `json:"notifications,omitempty"`
	Approvals     []ApprovalRun     `json:"postApproval,omitempty"`
	Workflow      Workflow          `json:"workflow"`
	Alerts        []Alert           `json:"alerts,omitempty"`
	Repo          string            `json:"repo"`
	Pull          Pull              `json:"pull_request"`
	Origin        *execution.Route  `json:"origin,omitempty"`
	Linked        bool              `json:"linked"`
	Developer     Assignee          `json:"developer"`
	Reviewer      Assignee          `json:"reviewer"`
	Phase         string            `json:"phase"`
	Task          *Task             `json:"task,omitempty"`
	History       []*Task           `json:"execution_history,omitempty"`
	Reviews       []Review          `json:"reviews,omitempty"`
}

func (w *Work) derive() {
	if w.Pull.State == "closed" {
		w.Phase = "closed"
		if w.Pull.Merged {
			w.Phase = "merged"
		}
		w.replaceTask(nil)
		return
	}
	if !w.Linked {
		w.Phase = "unclaimed"
		w.replaceTask(nil)
		return
	}
	if w.Pull.Draft {
		w.Phase = "draft"
		w.replaceTask(nil)
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
		w.Phase = "approved"
		w.replaceTask(nil)
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
	task.CreatedAt = time.Now()
	task.ID = uuid.NewString()
	if w.TaskMode == "cooperative" {
		task.Handoff = &TaskHandoff{State: "pending", Assignee: assignee}
	}
	w.replaceTask(&task)
}

func (w *Work) replaceTask(task *Task) {
	if w.Task != nil && (len(w.Task.Runs) > 0 || w.Task.Handoff != nil) {
		if h := w.Task.Handoff; h != nil && h.State == "pending" {
			h.State = "superseded"
		}
		w.History = append(w.History, w.Task)
	}
	w.Task = task
}

func (w *Work) executionTasks() []*Task {
	tasks := append([]*Task{}, w.History...)
	if w.Task != nil {
		tasks = append(tasks, w.Task)
	}
	return tasks
}

func (w *Work) executionBlocked() bool {
	for _, t := range w.executionTasks() {
		if h := t.Handoff; h != nil && (h.State == "claimed" || h.State == "submitted" || h.State == "blocked") {
			return true
		}
		for _, a := range t.Runs {
			if !execution.Terminal(a.State) {
				return true
			}
		}
	}
	return false
}
