package ink

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"ink-commons/internal/execution"
	"ink-commons/internal/execution/ccconnect"
)

// ReviewFeatures is opt-in. An explicit empty object overrides registration
// defaults; the two features can be enabled independently.
type ReviewFeatures struct {
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
	if r.Review != nil && (r.Review.ReuseDevelopmentThread || r.Review.Context.ThreadHistory) && r.ReviewerRoute != nil && r.ReviewerRoute.Provider != ccconnect.Name {
		return fmt.Errorf("review thread features require a cc-connect reviewer route")
	}
	return nil
}

// All checks here are local and must run before reserving an execution. Source
// history still needs provenance when an explicit reviewer session/parent means
// that normal session preparation would otherwise skip the source checks.
func (s *Service) validateReviewPreparation(w *Work) error {
	if w.Task.Role != "reviewer" || w.Review == nil || (!w.Review.ReuseDevelopmentThread && !w.Review.Context.ThreadHistory) {
		return nil
	}
	if w.Reviewer.Route == nil || w.Reviewer.Route.Provider != ccconnect.Name {
		return fmt.Errorf("review thread features require a cc-connect reviewer route")
	}
	if w.Review.Context.ThreadHistory {
		source := reviewSource(w)
		if source == nil || source.Provider != ccconnect.Name {
			return fmt.Errorf("history requires a Discord development source")
		}
		if err := execution.ValidateDestination(s.providers, *source, false); err != nil {
			return err
		}
	}
	route := *w.Reviewer.Route
	if repo, ok := s.repo(w.Repo); ok {
		route = allocationRoute(repo, route)
	}
	if err := execution.ValidateDestination(s.providers, route, route.Session == ""); err != nil {
		return err
	}
	return execution.ValidatePreparation(s.providers, route, deliveryFor(w, w.Task, "", 0))
}

func reviewHistoryBefore(t time.Time) string {
	// Discord snowflakes at or after the start of this millisecond are excluded.
	const discordEpoch = int64(1420070400000)
	if t.IsZero() || t.UnixMilli() <= discordEpoch {
		return ""
	}
	return strconv.FormatUint(uint64(t.UnixMilli()-discordEpoch)<<22, 10)
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
	st, err := s.store.load()
	if err != nil {
		s.mu.Unlock()
		fail(w, err)
		return
	}
	work := st.Work[key(repoName, number)]
	if work == nil {
		s.mu.Unlock()
		fail(w, apiError{404, "PR not observed yet"})
		return
	}
	task := work.Task
	if !work.Linked || task == nil || task.Role != "reviewer" || work.Phase != "review_pending" || r.URL.Query().Get("sha") != work.Pull.Head.SHA {
		s.mu.Unlock()
		fail(w, apiError{409, "context requires the current reviewer task and its head SHA"})
		return
	}
	anchor := reviewHistoryBefore(task.CreatedAt)
	source := reviewSource(work)
	if anchor == "" || source == nil || source.Provider != ccconnect.Name || work.Reviewer.Route == nil || work.Reviewer.Route.Provider != ccconnect.Name {
		s.mu.Unlock()
		fail(w, apiError{409, "context requires a Discord development thread and cc-connect reviewer"})
		return
	}
	route := *source
	if len(task.Runs) > 0 {
		request := task.Runs[len(task.Runs)-1].Request
		if request.ContextBefore == "" || request.Source == nil || *request.Source != route || request.SHA != task.SHA {
			s.mu.Unlock()
			fail(w, apiError{409, "history was not enabled for this attempt or its development source changed"})
			return
		}
		anchor = request.ContextBefore
	}
	reader, ok := s.providers[route.Provider].(execution.ConversationReader)
	taskID, sha := task.ID, task.SHA
	s.mu.Unlock()
	if !ok {
		fail(w, apiError{409, "execution provider cannot read conversation history"})
		return
	}
	before := r.URL.Query().Get("before")
	if before == "" {
		before = anchor
	}
	cursor, cursorErr := strconv.ParseUint(before, 10, 64)
	boundary, _ := strconv.ParseUint(anchor, 10, 64)
	if cursorErr != nil || cursor == 0 || cursor > boundary {
		fail(w, apiError{400, "before must be a positive message ID at or before the review boundary"})
		return
	}
	page, err := reader.ReadConversation(r.Context(), route, before)
	if err != nil {
		fail(w, apiError{502, "cannot read development thread history: " + err.Error()})
		return
	}
	// A slow history request must not return context for a superseded task or
	// continue granting access after the feature was disabled/reassigned.
	s.mu.Lock()
	currentRepo, enabled := s.repo(repoName)
	latest, loadErr := s.store.load()
	live := latest.Work[key(repoName, number)]
	valid := loadErr == nil && enabled && currentRepo.Review != nil && currentRepo.Review.Context.ThreadHistory && live != nil && live.Task != nil && live.Task.ID == taskID && live.Task.SHA == sha && live.Phase == "review_pending" && reviewSource(live) != nil && *reviewSource(live) == route
	s.mu.Unlock()
	if loadErr != nil {
		fail(w, loadErr)
		return
	}
	if !valid {
		fail(w, apiError{409, "review context changed while reading; request the current task"})
		return
	}
	page.TaskID = taskID
	page.SHA = sha
	page.ReviewBefore = anchor
	reply(w, 200, page)
}
