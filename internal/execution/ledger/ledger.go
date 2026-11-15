// Package ledger provides durable admission and ownership for HTTP executors.
// The runtime must call Start before side effects, retain the returned ownership
// token for every update, and join/disable all tools before Finish or Stop.
package ledger

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"
	"ink-commons/internal/execution"
	_ "modernc.org/sqlite"
)

var ErrConflict = errors.New("execution ownership conflict")
var ErrUnknown = errors.New("execution outcome unknown")

type Ledger struct {
	db *sql.DB
	mu sync.Mutex
}

func Open(path string) (*Ledger, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	// Create with private permissions before SQLite opens it.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	f.Close()
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return nil, fmt.Errorf("ledger must be a private regular file")
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	_, err = db.Exec(`PRAGMA busy_timeout=5000;
 CREATE TABLE IF NOT EXISTS executions (
 attempt_id TEXT PRIMARY KEY, task_id TEXT NOT NULL, number INTEGER NOT NULL,
 resource TEXT NOT NULL, request TEXT NOT NULL, receipt TEXT NOT NULL,
 owner TEXT NOT NULL DEFAULT '', UNIQUE(task_id,number));
 CREATE TABLE IF NOT EXISTS sessions(task_id TEXT PRIMARY KEY, session TEXT NOT NULL);
 `)
	if err != nil {
		db.Close()
		return nil, err
	}
	return &Ledger{db: db}, nil
}
func (l *Ledger) Close() error { return l.db.Close() }

type record struct {
	request execution.Delivery
	receipt execution.Receipt
	owner   string
}

func read(tx *sql.Tx, id string) (record, error) {
	var d, r string
	var v record
	err := tx.QueryRow("SELECT request,receipt,owner FROM executions WHERE attempt_id=?", id).Scan(&d, &r, &v.owner)
	if err != nil {
		return v, err
	}
	if err = json.Unmarshal([]byte(d), &v.request); err != nil {
		return v, err
	}
	err = json.Unmarshal([]byte(r), &v.receipt)
	return v, err
}
func save(tx *sql.Tx, v record) error {
	b, err := json.Marshal(v.receipt)
	if err != nil {
		return err
	}
	_, err = tx.Exec("UPDATE executions SET receipt=?,owner=? WHERE attempt_id=?", string(b), v.owner, v.request.AttemptID)
	return err
}
func same(a, b execution.Delivery) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}
func valid(d execution.Delivery) bool {
	return d.TaskID != "" && d.AttemptID != "" && d.Attempt > 0 && d.Repository != "" && d.Number > 0
}
func (l *Ledger) transaction(fn func(*sql.Tx) error) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	tx, err := l.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Acquire SQLite's writer reservation before reading, also across processes.
	if _, err = tx.Exec("UPDATE executions SET owner=owner WHERE 0"); err != nil {
		return err
	}
	if err = fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}
func insert(tx *sql.Tx, d execution.Delivery, state string) (record, error) {
	v := record{request: d, receipt: execution.Receipt{TaskID: d.TaskID, AttemptID: d.AttemptID, Sequence: 1, State: state}}
	if state == "stopped" {
		v.receipt.FinishedAt = time.Now().UTC()
	}
	b, _ := json.Marshal(d)
	r, _ := json.Marshal(v.receipt)
	_, err := tx.Exec("INSERT INTO executions(attempt_id,task_id,number,resource,request,receipt) VALUES(?,?,?,?,?,?)", d.AttemptID, d.TaskID, d.Attempt, d.Repository+fmt.Sprintf("#%d", d.Number), string(b), string(r))
	return v, err
}

// Accept returns the existing durable result for exact duplicates. A new round
// cannot displace an active round, even when it belongs to a new task version.
func (l *Ledger) Accept(d execution.Delivery) (execution.Receipt, error) {
	var out execution.Receipt
	if !valid(d) {
		return out, fmt.Errorf("missing execution identity")
	}
	err := l.transaction(func(tx *sql.Tx) error {
		v, err := read(tx, d.AttemptID)
		if err == nil {
			if !same(v.request, d) {
				return ErrConflict
			}
			out = v.receipt
			return nil
		}
		if err != sql.ErrNoRows {
			return err
		}
		var highest int
		if err = tx.QueryRow("SELECT COALESCE(MAX(number),0) FROM executions WHERE task_id=?", d.TaskID).Scan(&highest); err != nil {
			return err
		}
		if d.Attempt <= highest {
			return ErrConflict
		}
		rows, err := tx.Query("SELECT receipt FROM executions WHERE resource=?", d.Repository+fmt.Sprintf("#%d", d.Number))
		if err != nil {
			return err
		}
		for rows.Next() {
			var raw string
			if err = rows.Scan(&raw); err != nil {
				rows.Close()
				return err
			}
			var receipt execution.Receipt
			if err = json.Unmarshal([]byte(raw), &receipt); err != nil {
				rows.Close()
				return err
			}
			if !execution.Terminal(receipt.State) {
				rows.Close()
				return ErrConflict
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		v, err = insert(tx, d, "accepted")
		out = v.receipt
		return err
	})
	return out, err
}

func (l *Ledger) Status(d execution.Delivery) (execution.Receipt, error) {
	var out execution.Receipt
	err := l.transaction(func(tx *sql.Tx) error {
		v, err := read(tx, d.AttemptID)
		if err == sql.ErrNoRows {
			return ErrUnknown
		}
		if err != nil {
			return err
		}
		if !same(v.request, d) {
			return ErrConflict
		}
		out = v.receipt
		return nil
	})
	return out, err
}

// Start atomically transfers an accepted attempt to one runtime owner. Repeated
// or restarted delivery cannot obtain a second owner. Session is not ownership.
func (l *Ledger) Start(d execution.Delivery) (string, execution.Receipt, error) {
	owner := uuid.NewString()
	var out execution.Receipt
	err := l.transaction(func(tx *sql.Tx) error {
		v, err := read(tx, d.AttemptID)
		if err != nil {
			return err
		}
		if !same(v.request, d) || v.receipt.State != "accepted" {
			return ErrConflict
		}
		now := time.Now().UTC()
		v.owner = owner
		v.receipt.Sequence++
		v.receipt.State = "running"
		v.receipt.StartedAt, v.receipt.HeartbeatAt = now, now
		out = v.receipt
		return save(tx, v)
	})
	if err != nil {
		owner = ""
	}
	return owner, out, err
}

func (l *Ledger) Heartbeat(d execution.Delivery, owner string) (execution.Receipt, error) {
	return l.update(d, owner, "")
}

// Finish is permitted only after the runtime has joined the agent and all its
// tools. Persisting a terminal state is a promise that no side effects remain.
func (l *Ledger) Finish(d execution.Delivery, owner, state string) (execution.Receipt, error) {
	if state != "succeeded" && state != "failed" {
		return execution.Receipt{}, fmt.Errorf("invalid finish state")
	}
	return l.update(d, owner, state)
}
func (l *Ledger) update(d execution.Delivery, owner, state string) (execution.Receipt, error) {
	var out execution.Receipt
	err := l.transaction(func(tx *sql.Tx) error {
		v, err := read(tx, d.AttemptID)
		if err != nil {
			return err
		}
		if !same(v.request, d) || owner == "" || owner != v.owner || v.receipt.State != "running" {
			return ErrConflict
		}
		v.receipt.Sequence++
		v.receipt.HeartbeatAt = time.Now().UTC()
		if state != "" {
			v.receipt.State = state
			v.receipt.FinishedAt = v.receipt.HeartbeatAt
			v.owner = ""
		}
		out = v.receipt
		return save(tx, v)
	})
	return out, err
}

// Stop serializes with Start and persists a tombstone for delayed deliveries.
// For a running attempt, stopAndJoin must verify actual termination, including
// after runtime restart; inability to locate an old process is not confirmation.
// The callback must not call the ledger; cancellation must not wait for Finish.
func (l *Ledger) Stop(ctx context.Context, d execution.Delivery, stopAndJoin func(context.Context, string) error) (execution.Receipt, error) {
	var out execution.Receipt
	if !valid(d) {
		return out, fmt.Errorf("missing execution identity")
	}
	err := l.transaction(func(tx *sql.Tx) error {
		v, err := read(tx, d.AttemptID)
		if err == sql.ErrNoRows {
			v, err = insert(tx, d, "stopped")
			out = v.receipt
			return err
		}
		if err != nil {
			return err
		}
		if !same(v.request, d) {
			return ErrConflict
		}
		if execution.Terminal(v.receipt.State) {
			out = v.receipt
			return nil
		}
		if v.receipt.State == "running" {
			if stopAndJoin == nil {
				return ErrUnknown
			}
			if err = stopAndJoin(ctx, v.owner); err != nil {
				return err
			}
		}
		v.receipt.Sequence++
		v.receipt.State = "stopped"
		v.receipt.FinishedAt = time.Now().UTC()
		v.owner = ""
		out = v.receipt
		return save(tx, v)
	})
	return out, err
}

// ReserveSession must precede creating an external conversation. An unfinished
// reservation survives restart and requires reconciliation, never reallocation.
func (l *Ledger) ReserveSession(taskID string) (session string, reserved bool, err error) {
	if taskID == "" {
		return "", false, fmt.Errorf("missing task identity")
	}
	err = l.transaction(func(tx *sql.Tx) error {
		e := tx.QueryRow("SELECT session FROM sessions WHERE task_id=?", taskID).Scan(&session)
		if e == nil {
			if session == "" {
				return ErrUnknown
			}
			return nil
		}
		if e != sql.ErrNoRows {
			return e
		}
		_, e = tx.Exec("INSERT INTO sessions(task_id,session) VALUES(?,'')", taskID)
		reserved = e == nil
		return e
	})
	return
}

// SetSession records a newly created or externally reconciled conversation.
func (l *Ledger) SetSession(taskID, session string) error {
	if session == "" {
		return fmt.Errorf("empty session")
	}
	return l.transaction(func(tx *sql.Tx) error {
		var current string
		if err := tx.QueryRow("SELECT session FROM sessions WHERE task_id=?", taskID).Scan(&current); err != nil {
			return err
		}
		if current != "" && current != session {
			return ErrConflict
		}
		_, err := tx.Exec("UPDATE sessions SET session=? WHERE task_id=?", session, taskID)
		return err
	})
}
