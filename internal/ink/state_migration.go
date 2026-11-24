//go:build linux || darwin

package ink

import (
	"encoding/json"
	"fmt"
	"strings"
)

// The sole v6 compatibility boundary is an atomic rewrite before serving.
// Receipts and frozen task policies must survive the spelling change.
func migrateStateV6(path string) error {
	db, err := openSQLite(path)
	if err != nil {
		return err
	}
	defer db.Close()
	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 6 {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := verifyStateTables(tx); err != nil {
		return err
	}
	for _, table := range []struct{ name, id string }{
		{"runtime_config", "scope"}, {"work_items", "id"},
	} {
		rows, err := tx.Query("SELECT "+table.id+", payload FROM "+table.name+" WHERE instr(payload, ?) > 0", oldApprovalKey)
		if err != nil {
			return err
		}
		type update struct{ id, payload string }
		var changes []update
		for rows.Next() {
			var row update
			if err := rows.Scan(&row.id, &row.payload); err != nil {
				rows.Close()
				return err
			}
			var value any
			decoder := json.NewDecoder(strings.NewReader(row.payload))
			decoder.UseNumber()
			if err := decoder.Decode(&value); err != nil {
				rows.Close()
				return err
			}
			if err := renameApprovalKey(value); err != nil {
				rows.Close()
				return err
			}
			row.payload = string(jsonValue(value))
			changes = append(changes, row)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, row := range changes {
			if _, err := tx.Exec("UPDATE "+table.name+" SET payload = ? WHERE "+table.id+" = ?", row.payload, row.id); err != nil {
				return err
			}
		}
	}
	if _, err := readConfigRecords(tx); err != nil {
		return err
	}
	if _, err := tx.Exec("ALTER TABLE workspace_allocations ADD COLUMN repository TEXT"); err != nil {
		return err
	}
	if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", schemaVersion)); err != nil {
		return err
	}
	return tx.Commit()
}

const oldApprovalKey = "postApproval"

func renameApprovalKey(value any) error {
	switch node := value.(type) {
	case map[string]any:
		if old, found := node[oldApprovalKey]; found {
			if _, duplicate := node["post_approval"]; duplicate {
				return diagnosticError("conflicting approval keys in schema v6 state")
			}
			node["post_approval"] = old
			delete(node, oldApprovalKey)
		}
		if event, _ := node["event"].(string); event == oldApprovalKey {
			node["event"] = "post_approval"
		}
		for _, child := range node {
			if err := renameApprovalKey(child); err != nil {
				return err
			}
		}
	case []any:
		for _, child := range node {
			if err := renameApprovalKey(child); err != nil {
				return err
			}
		}
	}
	return nil
}
