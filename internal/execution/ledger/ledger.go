// Package ledger provides durable admission and ownership for HTTP executors.
// The runtime must call Start before side effects, retain the returned ownership
// token for every update, and join/disable all tools before Finish or Stop.
package ledger

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/wujinhjun/ink-commons/internal/execution"
	"github.com/wujinhjun/ink-commons/internal/execution/ledger/ledgerdb"
	_ "modernc.org/sqlite"
)

//go:embed sql/schema.sql
var schema string

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
	_, err = db.Exec("PRAGMA busy_timeout=5000;\n" + schema)
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
	var v record
	row, err := ledgerdb.New(tx).ReadExecution(context.Background(), id)
	if err != nil {
		return v, err
	}
	if err = json.Unmarshal([]byte(row.Request), &v.request); err != nil {
		return v, err
	}
	v.owner = row.Owner
	err = json.Unmarshal([]byte(row.Receipt), &v.receipt)
	return v, err
}
func save(tx *sql.Tx, v record) error {
	b, err := json.Marshal(v.receipt)
	if err != nil {
		return err
	}
	return ledgerdb.New(tx).SaveExecution(context.Background(), ledgerdb.SaveExecutionParams{Receipt: string(b), Owner: v.owner, AttemptID: v.request.AttemptID})
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
	if err = ledgerdb.New(tx).ReserveWriter(context.Background()); err != nil {
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
	err := ledgerdb.New(tx).InsertExecution(context.Background(), ledgerdb.InsertExecutionParams{AttemptID: d.AttemptID, TaskID: d.TaskID, Number: int64(d.Attempt), Resource: d.Repository + fmt.Sprintf("#%d", d.Number), Request: string(b), Receipt: string(r)})
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
		q := ledgerdb.New(tx)
		highest, err := q.HighestAttempt(context.Background(), d.TaskID)
		if err != nil {
			return err
		}
		if int64(d.Attempt) <= highest {
			return ErrConflict
		}
		receipts, err := q.ResourceReceipts(context.Background(), d.Repository+fmt.Sprintf("#%d", d.Number))
		if err != nil {
			return err
		}
		for _, raw := range receipts {
			var receipt execution.Receipt
			if err := json.Unmarshal([]byte(raw), &receipt); err != nil {
				return err
			}
			if !execution.Terminal(receipt.State) {
				return ErrConflict
			}
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
		var e error
		session, e = ledgerdb.New(tx).ReadSession(context.Background(), taskID)
		if e == nil {
			if session == "" {
				return ErrUnknown
			}
			return nil
		}
		if e != sql.ErrNoRows {
			return e
		}
		e = ledgerdb.New(tx).ReserveSession(context.Background(), taskID)
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
		current, err := ledgerdb.New(tx).ReadSession(context.Background(), taskID)
		if err != nil {
			return err
		}
		if current != "" && current != session {
			return ErrConflict
		}
		return ledgerdb.New(tx).SetSession(context.Background(), ledgerdb.SetSessionParams{Session: session, TaskID: taskID})
	})
}
