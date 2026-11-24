//go:build linux || darwin

package ink

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestApprovalKeyMigrationPreservesFrozenPolicyAndReceipts(t *testing.T) {
	s, dir := initTestStore(t)
	path := filepath.Join(dir, stateDatabase)
	if _, err := s.db.Exec("ALTER TABLE workspace_allocations DROP COLUMN repository; PRAGMA user_version = 6"); err != nil {
		t.Fatal(err)
	}
	hook := `{"command":["/bin/true"]}`
	global := `{"` + oldApprovalKey + `":` + hook + `,"reviewer_login":"reviewer"}`
	if _, err := s.db.Exec("INSERT INTO runtime_config VALUES ('', ?, 1)", global); err != nil {
		t.Fatal(err)
	}
	work := `{"repo":"acme/project","pull_request":{"number":42,"title":"` + oldApprovalKey + `"},
		"` + oldApprovalKey + `":[{"state":"succeeded","event":"` + oldApprovalKey + `","sha":"head","review_id":9007199254740993}],
		"task":{"config_snapshot":{"values":{"` + oldApprovalKey + `":` + hook + `},"sources":{"` + oldApprovalKey + `":"global"}}}}`
	if _, err := s.db.Exec("INSERT INTO work_items VALUES ('acme/project#42', 'acme/project', 42, 3, 1, 0, 0, 0, ?)", work); err != nil {
		t.Fatal(err)
	}
	s.db.Close()
	if err := migrateStateV6(path); err != nil {
		t.Fatal(err)
	}
	if err := migrateStateV6(path); err != nil {
		t.Fatal(err)
	}
	store, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.db.Close()
	config, err := store.effectiveConfig("")
	if err != nil || config.Repository.PostApproval == nil {
		t.Fatalf("global hook missing: %v", err)
	}
	w, err := store.loadWork("acme/project#42")
	if err != nil {
		t.Fatal(err)
	}
	if len(w.Approvals) != 1 || !w.hasApprovalReceipt("head", 9007199254740993) || w.Approvals[0].Event != "post_approval" || w.Pull.Title != oldApprovalKey ||
		w.Task.ConfigSnapshot.Sources["post_approval"] != "global" || !strings.Contains(string(w.Task.ConfigSnapshot.Values["post_approval"]), "/bin/true") {
		t.Fatalf("migration lost receipt or snapshot: %+v", w)
	}
}
