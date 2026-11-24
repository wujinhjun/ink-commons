//go:build linux || darwin

package ink

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/wujinhjun/ink-commons/internal/ink/statedb"
)

func repositoryPath(r *http.Request) string {
	return strings.ToLower(r.PathValue("owner") + "/" + r.PathValue("repo"))
}

func (s *Service) updateRepository(w http.ResponseWriter, r *http.Request) {
	name := repositoryPath(r)
	if !ValidRepository(name) {
		fail(w, apiError{400, "expected owner/repo"})
		return
	}
	var values ConfigValues
	if err := decode(w, r, &values); err != nil {
		fail(w, err)
		return
	}
	if len(values) == 0 {
		fail(w, apiError{400, "specify at least one configuration override"})
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	repo, err := s.patchRepository(name, values)
	if err != nil {
		fail(w, err)
		return
	}
	reply(w, 200, repo)
}

func (s *Service) patchRepository(name string, values ConfigValues) (Repository, error) {
	tx, err := s.store.db.Begin()
	if err != nil {
		return Repository{}, err
	}
	defer tx.Rollback()
	records, err := readConfigRecords(tx)
	if err != nil {
		return Repository{}, err
	}
	indexed := recordMap(records)
	record, exists := indexed[name]
	if !exists {
		return Repository{}, apiError{404, "repository is not registered"}
	}
	for key, value := range values {
		record.Values[key] = value
	}
	global := indexed[""]
	effective, err := s.resolveRepositoryPolicy(name, global, record.Values)
	if err != nil {
		return Repository{}, err
	}
	if _, changed := values["auto_merge"]; changed {
		if err := validateAutoMergeIdentity(s.up, effective.Repository); err != nil {
			return Repository{}, apiError{400, err.Error()}
		}
	}
	global.Revision++
	record.Revision = global.Revision
	if err := putConfigRecord(tx, global); err != nil {
		return Repository{}, err
	}
	if err := putConfigRecord(tx, record); err != nil {
		return Repository{}, err
	}
	return effective.Repository, tx.Commit()
}

func (s *Service) resolveRepositoryPolicy(name string, global ConfigRecord, values ConfigValues) (EffectiveConfig, error) {
	effective, err := resolveConfig(name, GlobalConfig(global.Values), RepositoryOverride(values), global.Revision)
	if err == nil {
		err = s.validateEffective(effective.Repository)
	}
	if err != nil {
		return effective, apiError{400, StartupDiagnostic(err)}
	}
	return effective, nil
}

func (s *Service) removeRepository(w http.ResponseWriter, r *http.Request) {
	name := repositoryPath(r)
	if !ValidRepository(name) {
		fail(w, apiError{400, "expected owner/repo"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	// Workspace acquisition takes this lock before Service.mu.
	if !s.workspaceMu.TryLock() {
		fail(w, apiError{409, "workspace operation in progress; retry removal after it finishes"})
		return
	}
	defer s.workspaceMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pullLocks.activeRepository(name) {
		fail(w, apiError{409, "repository background operation in progress; retry removal after it finishes"})
		return
	}
	if err := s.unregister(ctx, name); err != nil {
		fail(w, err)
		return
	}
	reply(w, 200, map[string]string{"repo": name, "status": "removed"})
}

func (s *Service) unregister(ctx context.Context, name string) error {
	tx, err := s.store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	records, err := readConfigRecords(tx)
	if err != nil {
		return err
	}
	indexed := recordMap(records)
	if _, ok := indexed[name]; !ok {
		return apiError{404, "repository is not registered"}
	}
	q := statedb.New(tx)
	rows, err := q.ListRepositoryWork(ctx, name)
	if err != nil {
		return err
	}
	tasks := make(map[string]bool)
	for _, row := range rows {
		work, err := decodeWork(row.Revision, row.Payload, nil)
		if err != nil {
			return err
		}
		if reason := s.removalBlocker(work); reason != "" {
			return apiError{409, reason}
		}
		for _, task := range work.executionTasks() {
			tasks[task.ID] = true
		}
		if work.Task != nil {
			work.replaceTask(nil)
			work.Version++
			if _, err := writeWork(tx, work); err != nil {
				return err
			}
		}
	}
	allocations, err := q.ListLiveWorkspaceClaims(ctx)
	if err != nil {
		return err
	}
	for _, allocation := range allocations {
		if allocation.TaskID != "" {
			if !tasks[allocation.TaskID] {
				continue
			}
		} else if !allocation.Repository.Valid {
			// Older standalone allocations carry no immutable repository binding.
			return apiError{409, "a live standalone workspace has no recorded repository; release it before removing repositories"}
		} else if allocation.Repository.String != name {
			continue
		}
		return apiError{409, "repository has a preparing or active workspace; release it before removing the repository"}
	}
	if err := q.DeleteRepositoryPushes(ctx, name); err != nil {
		return err
	}
	if err := q.DeleteRuntimeConfig(ctx, name); err != nil {
		return err
	}
	global := indexed[""]
	global.Revision++
	if err := putConfigRecord(tx, global); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Service) removalBlocker(work *Work) string {
	if work.executionBlocked() || s.delivering[keyForWork(work)] {
		return "repository has an unresolved execution or task handoff; settle it before removal"
	}
	for _, task := range work.executionTasks() {
		if (task.Preparation != nil && task.Preparation.Preparation == "preparing") ||
			(task.Handoff != nil && task.Handoff.Wake.Preparation == "preparing") {
			return "repository has unresolved conversation preparation; settle it before removal"
		}
	}
	for _, notice := range work.Notifications {
		if notice.pendingDelivery() || notice.State == "awaiting_ack" {
			return "repository has an unresolved notification; acknowledge or settle it before removal"
		}
	}
	for _, approval := range work.Approvals {
		if approval.State == "pending" {
			return "repository has a pending post_approval hook; settle it before removal"
		}
	}
	if hasInFlightAutoMerge(work) {
		return "repository has an in-flight merge; wait for reconciliation before removal"
	}
	return ""
}
