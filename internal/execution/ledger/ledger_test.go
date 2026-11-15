package ledger

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"ink-commons/internal/execution"
)

func request(task, attempt string, n int) execution.Delivery {
	return execution.Delivery{TaskID: task, AttemptID: attempt, Attempt: n, Repository: "acme/project", Number: 42, Role: "reviewer", SHA: "abc"}
}
func openTest(t *testing.T, path string) *Ledger {
	t.Helper()
	l, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return l
}
func TestDurableOwnershipAndLostACK(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.db")
	l := openTest(t, path)
	d := request("task", "attempt", 1)
	first, err := l.Accept(d)
	if err != nil {
		t.Fatal(err)
	}
	l.Close()
	l = openTest(t, path)
	again, err := l.Accept(d)
	if err != nil || first != again {
		t.Fatalf("lost ACK replay: %+v %v", again, err)
	}
	owner, started, err := l.Start(d)
	if err != nil || started.State != "running" {
		t.Fatal(started, err)
	}
	if _, _, err = l.Start(d); !errors.Is(err, ErrConflict) {
		t.Fatal("duplicate start", err)
	}
	l.Close()
	l = openTest(t, path)
	if _, err = l.Accept(request("new-task", "next", 1)); !errors.Is(err, ErrConflict) {
		t.Fatal("restart lost ownership", err)
	}
	if _, err = l.Stop(context.Background(), d, nil); !errors.Is(err, ErrUnknown) {
		t.Fatal("missing runtime treated as stopped", err)
	}
	called := false
	stopped, err := l.Stop(context.Background(), d, func(_ context.Context, actual string) error {
		called = true
		if actual != owner {
			t.Fatal("wrong runtime owner")
		}
		return nil
	})
	if err != nil || !called || stopped.State != "stopped" {
		t.Fatal(stopped, err)
	}
	if _, err = l.Heartbeat(d, owner); !errors.Is(err, ErrConflict) {
		t.Fatal("old owner still writes", err)
	}
	if _, err = l.Finish(d, owner, "succeeded"); !errors.Is(err, ErrConflict) {
		t.Fatal("old owner still finishes", err)
	}
	if _, err = l.Accept(request("task", "next", 2)); err != nil {
		t.Fatal(err)
	}
	if _, err = l.Accept(request("task", "outdated", 1)); !errors.Is(err, ErrConflict) {
		t.Fatal("old round admitted", err)
	}
}
func TestStopBeforeDelayedDeliveryAndFailedStop(t *testing.T) {
	l := openTest(t, filepath.Join(t.TempDir(), "ledger.db"))
	d := request("task", "late", 1)
	tombstone, err := l.Stop(context.Background(), d, nil)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := l.Accept(d)
	if err != nil || replay != tombstone {
		t.Fatal(replay, err)
	}
	if _, _, err = l.Start(d); !errors.Is(err, ErrConflict) {
		t.Fatal("delayed request resurrected", err)
	}
	next := request("task", "next", 2)
	l.Accept(next)
	owner, _, _ := l.Start(next)
	if _, err = l.Stop(context.Background(), next, func(context.Context, string) error { return ErrUnknown }); !errors.Is(err, ErrUnknown) {
		t.Fatal(err)
	}
	if _, err = l.Heartbeat(next, owner); err != nil {
		t.Fatal("failed stop revoked running owner", err)
	}
	changed := next
	changed.SHA = "different"
	if _, err = l.Accept(changed); !errors.Is(err, ErrConflict) {
		t.Fatal("identity reused with different task", err)
	}
}
func TestConcurrentRuntimesShareAdmission(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.db")
	a := openTest(t, path)
	b := openTest(t, path)
	d := request("task", "attempt", 1)
	if _, err := a.Accept(d); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, l := range []*Ledger{a, b} {
		wg.Add(1)
		go func(l *Ledger) { defer wg.Done(); _, _, err := l.Start(d); results <- err }(l)
	}
	wg.Wait()
	close(results)
	success := 0
	for err := range results {
		if err == nil {
			success++
		} else if !errors.Is(err, ErrConflict) {
			t.Fatal(err)
		}
	}
	if success != 1 {
		t.Fatalf("%d owners", success)
	}
}
func TestSessionReservationSurvivesAmbiguousCreation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.db")
	l := openTest(t, path)
	_, reserved, err := l.ReserveSession("task")
	if err != nil || !reserved {
		t.Fatal(reserved, err)
	}
	l.Close()
	l = openTest(t, path)
	if _, reserved, err = l.ReserveSession("task"); !errors.Is(err, ErrUnknown) || reserved {
		t.Fatal("ambiguous allocation repeated", reserved, err)
	}
	if err = l.SetSession("task", "thread-42"); err != nil {
		t.Fatal(err)
	}
	session, reserved, err := l.ReserveSession("task")
	if err != nil || reserved || session != "thread-42" {
		t.Fatal(session, reserved, err)
	}
	if err = l.SetSession("task", "other-thread"); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
}
