//go:build linux || darwin

package ink

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/wujinhjun/ink-commons/internal/execution"
)

// ReviewFeatures is opt-in. An explicit empty object overrides registration
// defaults; the features can be enabled independently.
type ReviewFeatures struct {
	RequireSameChannel     bool                  `json:"require_same_channel,omitempty"`
	ReuseDevelopmentThread bool                  `json:"reuse_development_thread,omitempty"`
	Context                ReviewContextFeatures `json:"context,omitempty"`
}

type ReviewContextFeatures struct {
	ThreadHistory bool `json:"thread_history,omitempty"`
}

func (r Repository) ValidateReviewFeatures() error {
	if r.TaskMode != "" && r.TaskMode != "legacy" && r.TaskMode != "cooperative" {
		return fmt.Errorf("task_mode must be legacy or cooperative")
	}
	return nil
}

// All checks here are local and must run before reserving an execution. Source
// history still needs provenance when an explicit reviewer session/parent means
// that normal session preparation would otherwise skip the source checks.
func (s *Service) validateReviewPreparation(w *Work) error {
	if w.Task.Role != "reviewer" || w.Review == nil || (!w.Review.RequireSameChannel && !w.Review.ReuseDevelopmentThread && !w.Review.Context.ThreadHistory) {
		return nil
	}
	if w.Reviewer.Route == nil {
		return fmt.Errorf("review thread features require an execution route")
	}
	if w.Review.Context.ThreadHistory {
		if err := s.validateHistorySource(deliveryFor(w, w.Task, "", 0).Source); err != nil {
			return err
		}
	}
	route := *w.Reviewer.Route
	if err := execution.ValidateDestination(s.providers, route, route.Session == ""); err != nil {
		return err
	}
	return execution.ValidatePreparation(s.providers, route, deliveryFor(w, w.Task, "", 0))
}

func (s *Service) validateHistorySource(source *execution.Route) error {
	if source == nil || !source.Valid(false) {
		return fmt.Errorf("history requires a resolved development route")
	}
	provider := s.providers[source.Provider]
	_, reads := provider.(execution.ConversationReader)
	if !execution.Supports(provider, "conversation_context", reads) {
		return fmt.Errorf("development provider does not support conversation_context")
	}
	return execution.ValidateDestination(s.providers, *source, false)
}

func reviewHistoryBefore(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

// reviewContextBinding is the immutable read authorization for one task. Both
// sides of a potentially slow provider read must resolve to the same binding.
type reviewContextBinding struct {
	TaskID string
	SHA    string
	Before string
	Source execution.Route
}

func bindReviewContext(work *Work, repo Repository) (reviewContextBinding, error) {
	var binding reviewContextBinding
	task := work.Task
	if !work.Linked || task == nil || task.Role != "reviewer" || work.Phase != "review_pending" || task.SHA != work.Pull.Head.SHA {
		return binding, apiError{409, "context requires the current reviewer task and its head SHA"}
	}
	features, source, reviewer := repo.Review, reviewSource(work), work.Reviewer.Route
	if snapshot := task.ConfigSnapshot; snapshot != nil {
		// Cooperative tasks have no attempt requests. The task snapshot is the
		// authority for their history permission and development conversation too.
		if snapshot.SHA != task.SHA {
			return binding, apiError{409, "task configuration SHA does not match"}
		}
		features, source, reviewer = snapshot.Repository.Review, snapshot.Source, snapshot.Reviewer.Route
	}
	if features == nil || !features.Context.ThreadHistory {
		return binding, apiError{403, "history was not enabled for this task"}
	}
	anchor := reviewHistoryBefore(task.CreatedAt)
	if anchor == "" || source == nil || !source.Valid(false) || reviewer == nil {
		return binding, apiError{409, "context requires a resolved development route and reviewer"}
	}
	if len(task.Runs) > 0 {
		request := task.Runs[len(task.Runs)-1].Request
		if request.ContextBefore == "" || request.Source == nil || *request.Source != *source || request.SHA != task.SHA {
			return binding, apiError{409, "history was not enabled for this attempt or its development source changed"}
		}
		// Older persisted requests used provider-specific cursors. Task.CreatedAt
		// still supplies the original cutoff for an upgraded adapter.
		if _, err := time.Parse(time.RFC3339Nano, request.ContextBefore); err == nil && request.ContextBefore != anchor {
			return binding, apiError{409, "history cutoff changed"}
		}
	}
	return reviewContextBinding{TaskID: task.ID, SHA: task.SHA, Before: anchor, Source: *source}, nil
}

// reviewContext resolves the source server-side, never from a caller's channel
// ID. The private Unix socket and service-user permissions are the auth boundary.
func (s *Service) reviewContext(w http.ResponseWriter, r *http.Request) {
	repoName := strings.ToLower(r.PathValue("owner") + "/" + r.PathValue("repo"))
	number, err := strconv.Atoi(r.PathValue("number"))
	if err != nil || number <= 0 {
		fail(w, apiError{400, "invalid PR number"})
		return
	}
	s.mu.Lock()
	repo, ok := s.repo(repoName)
	if !ok || repo.Review == nil || !repo.Review.Context.ThreadHistory {
		s.mu.Unlock()
		fail(w, apiError{403, "review.context.thread_history is disabled"})
		return
	}
	work, err := s.store.loadWork(key(repoName, number))
	if err != nil {
		s.mu.Unlock()
		fail(w, err)
		return
	}
	if work == nil {
		s.mu.Unlock()
		fail(w, apiError{404, "PR not observed yet"})
		return
	}
	if r.URL.Query().Get("sha") != work.Pull.Head.SHA {
		s.mu.Unlock()
		fail(w, apiError{409, "context requires the current reviewer task and its head SHA"})
		return
	}
	binding, err := bindReviewContext(work, repo)
	if err != nil {
		s.mu.Unlock()
		fail(w, err)
		return
	}
	route, anchor := binding.Source, binding.Before
	reader, _ := s.providers[route.Provider].(execution.ConversationReader)
	sourceErr := s.validateHistorySource(&route)
	task := deliveryFor(work, work.Task, "", 0)
	task.Source, task.ContextBefore = &route, anchor
	s.mu.Unlock()
	if sourceErr != nil {
		fail(w, apiError{409, sourceErr.Error()})
		return
	}
	before := r.URL.Query().Get("before")
	// Accept the cutoff literal used by the CLI as a request for the first page.
	if before == anchor {
		before = ""
	}
	if len(before) > 512 {
		fail(w, apiError{400, "invalid history cursor"})
		return
	}
	page, err := reader.ReadConversation(r.Context(), route, task, before)
	if err == nil {
		err = validateContextPage(page, binding, before)
	}
	if err != nil {
		fail(w, apiError{502, "cannot read development conversation history: " + err.Error()})
		return
	}
	// A slow history request must not return context for a superseded task or
	// continue granting access after the live feature was disabled. A developer
	// reassignment may not redirect a frozen reviewer task to another source.
	s.mu.Lock()
	currentRepo, enabled := s.repo(repoName)
	live, loadErr := s.store.loadWork(key(repoName, number))
	valid := false
	if loadErr == nil && enabled && currentRepo.Review != nil && currentRepo.Review.Context.ThreadHistory && live != nil {
		currentBinding, bindErr := bindReviewContext(live, currentRepo)
		valid = bindErr == nil && currentBinding == binding
	}
	s.mu.Unlock()
	if loadErr != nil {
		fail(w, loadErr)
		return
	}
	if !valid {
		fail(w, apiError{409, "review context changed while reading; request the current task"})
		return
	}
	reply(w, 200, page)
}

func validateContextPage(page execution.ConversationPage, binding reviewContextBinding, before string) error {
	if page.TaskID != binding.TaskID || page.SHA != binding.SHA || page.Session != binding.Source.Session ||
		page.ReviewBefore != binding.Before || page.Before != before || page.Messages == nil || len(page.Messages) > 50 ||
		len(page.NextBefore) > 512 || (page.NextBefore != "" && (page.NextBefore == before || len(page.Messages) == 0 || !page.Truncated)) {
		return fmt.Errorf("invalid adapter history page binding or pagination")
	}
	cutoff, _ := time.Parse(time.RFC3339Nano, binding.Before)
	var previous time.Time
	ids := make(map[string]bool, len(page.Messages))
	pageBytes := 0
	for _, message := range page.Messages {
		stamp, err := time.Parse(time.RFC3339Nano, message.Timestamp)
		if err != nil || !stamp.Before(cutoff) || stamp.Before(previous) || message.ID == "" || ids[message.ID] {
			return fmt.Errorf("adapter history exceeds the cutoff or has invalid message ordering")
		}
		previous = stamp
		ids[message.ID] = true
		data, err := json.Marshal(message)
		if err != nil {
			return fmt.Errorf("invalid adapter history message")
		}
		pageBytes += len(data)
	}
	if pageBytes > 64*1024 {
		return fmt.Errorf("adapter history exceeds the page size limit")
	}
	return nil
}
