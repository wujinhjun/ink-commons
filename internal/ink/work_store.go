//go:build linux || darwin

package ink

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/wujinhjun/ink-commons/internal/execution"
	"github.com/wujinhjun/ink-commons/internal/ink/statedb"
)

func decodeWork(revision int64, payload string, err error) (*Work, error) {
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	work := &Work{storedRevision: revision}
	if err := json.Unmarshal([]byte(payload), work); err != nil {
		return nil, err
	}
	return work, nil
}

func (s *stateStore) loadWork(id string) (*Work, error) {
	row, err := statedb.New(s.db).LoadWork(context.Background(), id)
	return decodeWork(row.Revision, row.Payload, err)
}

func (s *stateStore) loadTask(id string) (*Work, *Task, error) {
	row, err := statedb.New(s.db).LoadTaskWork(context.Background(), id)
	work, err := decodeWork(row.Revision, row.Payload, err)
	return work, work.findTask(id), err
}

func (s *stateStore) loadNotification(id string) (*Work, error) {
	row, err := statedb.New(s.db).LoadNotificationWork(context.Background(), id)
	return decodeWork(row.Revision, row.Payload, err)
}

func (s *stateStore) listWork() ([]*Work, error) {
	rows, err := statedb.New(s.db).ListWork(context.Background())
	if err != nil {
		return nil, err
	}
	works := make([]*Work, 0, len(rows))
	for _, row := range rows {
		work, err := decodeWork(row.Revision, row.Payload, nil)
		if err != nil {
			return nil, err
		}
		works = append(works, work)
	}
	return works, nil
}

// The payload and the projected lookup and pending indexes commit together.
// The revision fences stale aggregates while Service.mu still protects the
// existing read-modify-write and external side effect boundaries.
func (s *stateStore) saveWork(work *Work) error {
	if !ValidRepository(work.Repo) || work.Pull.Number <= 0 {
		return fmt.Errorf("invalid PR identity %q", keyForWork(work))
	}
	payload, err := json.Marshal(work)
	if err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	ctx := context.Background()
	q := statedb.New(tx)
	params := workIndex(work)
	params.Payload = string(payload)
	updated, err := q.SaveWork(ctx, params)
	if err != nil {
		return err
	}
	if updated == 0 {
		row, err := q.LoadWork(ctx, params.ID)
		if err != nil {
			return err
		}
		if row.Revision == work.storedRevision && row.Payload == params.Payload {
			return nil
		}
		return fmt.Errorf("work %s changed since it was read", params.ID)
	}
	if err := q.DeleteWorkTasks(ctx, params.ID); err != nil {
		return err
	}
	for _, t := range work.executionTasks() {
		if t.ID != "" {
			if err := q.PutWorkTask(ctx, statedb.PutWorkTaskParams{TaskID: t.ID, WorkID: params.ID}); err != nil {
				return err
			}
		}
	}
	if err := q.DeleteWorkNotifications(ctx, params.ID); err != nil {
		return err
	}
	for _, n := range work.Notifications {
		if n.EventID != "" {
			if err := q.PutWorkNotification(ctx, statedb.PutWorkNotificationParams{EventID: n.EventID, WorkID: params.ID}); err != nil {
				return err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	work.storedRevision = params.Revision
	return nil
}

func workIndex(w *Work) statedb.SaveWorkParams {
	row := statedb.SaveWorkParams{
		ID: keyForWork(w), Repo: w.Repo, Number: int64(w.Pull.Number), Revision: w.storedRevision + 1,
		PollPending: w.Pull.State == "open" || hasInFlightAutoMerge(w),
	}
	// Approval receipts for an unmerged closed PR resume on reopen, which
	// GitHub's open listing will discover even without a local poll entry.
	if w.Pull.Merged && !row.PollPending {
		for _, n := range w.Notifications {
			row.PollPending = row.PollPending || n.pendingDelivery()
		}
	}
	// The current task may need a retry or a time-based workflow alert even
	// after the attempt ends. Retired tasks need recovery only while unresolved.
	row.ExecutionPending = w.Task != nil && len(w.Task.Runs) > 0
	for _, t := range w.executionTasks() {
		if h := t.Handoff; h != nil {
			row.HandoffPending = row.HandoffPending || h.State == "submitted"
			row.ExecutionPending = row.ExecutionPending || h.State == "claimed"
			row.BlockedNoticePending = row.BlockedNoticePending || hasBlockedNotice(t)
		}
		for _, a := range t.Runs {
			row.ExecutionPending = row.ExecutionPending || !execution.Terminal(a.State)
		}
	}
	return row
}
