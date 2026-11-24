package ink

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/wujinhjun/ink-commons/internal/ink/statedb"
)

const legacyConfigScope = "@legacy-json"

func readConfigRecords(tx *sql.Tx) ([]ConfigRecord, error) {
	rows, err := statedb.New(tx).ListRuntimeConfig(context.Background())
	if err != nil {
		return nil, err
	}
	var records []ConfigRecord
	for _, row := range rows {
		record := ConfigRecord{Scope: row.Scope, Revision: row.Revision}
		if err := json.Unmarshal([]byte(row.Payload), &record.Values); err != nil {
			return nil, err
		}
		if record.Values == nil || record.Revision < 1 {
			return nil, fmt.Errorf("invalid configuration record %q", row.Scope)
		}
		if row.Scope != legacyConfigScope {
			if row.Scope != "" && !ValidRepository(row.Scope) {
				return nil, fmt.Errorf("invalid configuration scope %q", row.Scope)
			}
			if err := validateValues(record.Values); err != nil {
				return nil, err
			}
		}
		records = append(records, record)
	}
	if len(records) > 0 {
		global, ok := recordMap(records)[""]
		if !ok {
			return nil, fmt.Errorf("runtime configuration is missing global settings")
		}
		for _, record := range records {
			if record.Revision > global.Revision {
				return nil, fmt.Errorf("configuration generation is inconsistent")
			}
		}
	}
	return records, nil
}

func putConfigRecord(tx *sql.Tx, record ConfigRecord) error {
	return statedb.New(tx).PutRuntimeConfig(context.Background(), statedb.PutRuntimeConfigParams{
		Scope: record.Scope, Payload: string(jsonValue(record.Values)), Revision: record.Revision,
	})
}

func importConfigRecords(tx *sql.Tx, records []ConfigRecord) error {
	seen := map[string]bool{}
	for _, record := range records {
		if seen[record.Scope] {
			return fmt.Errorf("duplicate configuration scope %q", record.Scope)
		}
		seen[record.Scope] = true
		if record.Scope != legacyConfigScope {
			if record.Scope != "" && !ValidRepository(record.Scope) {
				return fmt.Errorf("invalid configuration scope")
			}
			if err := validateValues(record.Values); err != nil {
				return err
			}
		}
		if record.Values == nil || record.Revision < 1 {
			return fmt.Errorf("invalid configuration revision")
		}
		if err := putConfigRecord(tx, record); err != nil {
			return err
		}
	}
	if len(records) > 0 && !seen[""] {
		return fmt.Errorf("configuration export is missing global settings")
	}
	if len(records) > 0 {
		global := recordMap(records)[""]
		for _, record := range records {
			if record.Revision > global.Revision {
				return fmt.Errorf("configuration generation is inconsistent")
			}
			if record.Scope == legacyConfigScope {
				continue
			}
			if _, err := effectiveFromRecords(records, record.Scope); err != nil {
				return err
			}
		}
	}
	return nil
}

func recordMap(records []ConfigRecord) map[string]ConfigRecord {
	result := map[string]ConfigRecord{}
	for _, record := range records {
		result[record.Scope] = record
	}
	return result
}

func effectiveFromRecords(records []ConfigRecord, name string) (EffectiveConfig, error) {
	indexed := recordMap(records)
	global := indexed[""]
	if name != "" {
		if _, ok := indexed[name]; !ok {
			return EffectiveConfig{}, apiError{404, "repository is not registered"}
		}
	}
	var override RepositoryOverride
	if name != "" {
		override = RepositoryOverride(indexed[name].Values)
	}
	return resolveConfig(name, GlobalConfig(global.Values), override, global.Revision)
}

func (s *stateStore) effectiveConfig(name string) (EffectiveConfig, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return EffectiveConfig{}, err
	}
	defer tx.Rollback()
	records, err := readConfigRecords(tx)
	if err != nil {
		return EffectiveConfig{}, err
	}
	result, err := effectiveFromRecords(records, name)
	if err != nil {
		return result, err
	}
	return result, tx.Commit()
}

// Import once, atomically. The old repositories table and a copy of the JSON
// runtime input remain as migration evidence. Neither is consulted afterwards.
func (s *stateStore) migrateConfig(config Config, st state) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	records, err := readConfigRecords(tx)
	if err != nil {
		return err
	}
	if _, exists := recordMap(records)[""]; exists {
		return tx.Commit()
	}
	global := sparseRepositoryValues(config.RepositoryDefaults)
	var reliability map[string]json.RawMessage
	_ = json.Unmarshal(jsonValue(config.Reliability), &reliability)
	for key, value := range reliability {
		global["execution."+key] = value
	}
	if err := putConfigRecord(tx, ConfigRecord{Scope: "", Values: global, Revision: 1}); err != nil {
		return err
	}
	// Static JSON won before this migration, so import it last.
	for _, repo := range append(append([]Repository{}, st.Repositories...), config.Repositories...) {
		if err := putConfigRecord(tx, ConfigRecord{Scope: repo.Repo, Values: legacyRepositoryValues(repo), Revision: 1}); err != nil {
			return err
		}
	}
	archive := ConfigValues{"repository_defaults": jsonValue(config.RepositoryDefaults), "repositories": jsonValue(config.Repositories), "execution": jsonValue(config.Reliability)}
	if err := putConfigRecord(tx, ConfigRecord{Scope: legacyConfigScope, Values: archive, Revision: 1}); err != nil {
		return err
	}
	migrated, err := readConfigRecords(tx)
	if err != nil {
		return err
	}
	for _, record := range migrated {
		if record.Scope == legacyConfigScope {
			continue
		}
		if _, err := effectiveFromRecords(migrated, record.Scope); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Called under Service.mu. Config mutations never use the workflow's full-state
// save path, so in-flight delivery snapshots cannot overwrite a new setting.
func (s *Service) changeConfig(scope, key string, value json.RawMessage, unset bool) (ConfigRecord, error) {
	tx, err := s.store.db.Begin()
	if err != nil {
		return ConfigRecord{}, err
	}
	defer tx.Rollback()
	records, err := readConfigRecords(tx)
	if err != nil {
		return ConfigRecord{}, err
	}
	indexed := recordMap(records)
	record, ok := indexed[scope]
	if !ok {
		return record, apiError{404, "repository is not registered"}
	}
	if _, ok := configFields[key]; !ok {
		return record, apiError{400, "unknown runtime configuration key"}
	}
	if unset {
		delete(record.Values, key)
	} else {
		record.Values[key] = value
	}
	indexed[scope] = record
	global := indexed[""]
	for name := range indexed {
		if name == legacyConfigScope {
			continue
		}
		effective, err := resolveConfig(name, GlobalConfig(global.Values), RepositoryOverride(indexed[name].Values), global.Revision)
		if name == "" {
			effective, err = resolveConfig("", GlobalConfig(global.Values), nil, global.Revision)
		}
		if err != nil {
			return record, apiError{400, err.Error()}
		}
		if err := s.validateEffective(effective.Repository); err != nil {
			return record, apiError{400, err.Error()}
		}
		if name != "" && name == scope && key == "auto_merge" {
			if err := s.validateAutoMergeIdentity(effective.Repository); err != nil {
				return record, apiError{400, err.Error()}
			}
		}
	}
	// Global revision is a monotonic generation for all runtime configuration.
	global.Revision++
	record.Revision = global.Revision
	if scope == "" {
		global = record
	}
	if err := putConfigRecord(tx, global); err != nil {
		return record, err
	}
	if scope != "" {
		if err := putConfigRecord(tx, record); err != nil {
			return record, err
		}
	}
	return record, tx.Commit()
}

func (s *Service) runtimeRepositories() ([]Repository, error) {
	tx, err := s.store.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	records, err := readConfigRecords(tx)
	if err != nil {
		return nil, err
	}
	result := []Repository{}
	for _, record := range records {
		if record.Scope == "" || record.Scope == legacyConfigScope {
			continue
		}
		cfg, err := effectiveFromRecords(records, record.Scope)
		if err != nil {
			return nil, err
		}
		result = append(result, cfg.Repository)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Repo < result[j].Repo })
	return result, tx.Commit()
}

func (s *Service) registerConfig(name string, values ConfigValues) (Repository, bool, error) {
	tx, err := s.store.db.Begin()
	if err != nil {
		return Repository{}, false, err
	}
	defer tx.Rollback()
	records, err := readConfigRecords(tx)
	if err != nil {
		return Repository{}, false, err
	}
	indexed := recordMap(records)
	global := indexed[""]
	if _, exists := indexed[name]; exists && len(values) == 0 {
		current, err := effectiveFromRecords(records, name)
		if err != nil {
			return Repository{}, false, err
		}
		return current.Repository, false, tx.Commit()
	}
	effective, err := resolveConfig(name, GlobalConfig(global.Values), RepositoryOverride(values), global.Revision)
	if err != nil {
		return Repository{}, false, apiError{400, err.Error()}
	}
	if err := s.validateEffective(effective.Repository); err != nil {
		return Repository{}, false, apiError{400, err.Error()}
	}
	if old, ok := indexed[name]; ok {
		if !bytes.Equal(canonicalValues(old.Values), canonicalValues(values)) {
			return Repository{}, false, apiError{409, "repository already registered with different overrides"}
		}
		return effective.Repository, false, tx.Commit()
	}
	if err := s.validateAutoMergeIdentity(effective.Repository); err != nil {
		return Repository{}, false, apiError{400, err.Error()}
	}
	global.Revision++
	if err := putConfigRecord(tx, global); err != nil {
		return Repository{}, false, err
	}
	if err := putConfigRecord(tx, ConfigRecord{Scope: name, Values: values, Revision: global.Revision}); err != nil {
		return Repository{}, false, err
	}
	return effective.Repository, true, tx.Commit()
}

func (s *stateStore) configRecords() ([]ConfigRecord, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	records, err := readConfigRecords(tx)
	if err != nil {
		return nil, err
	}
	return records, tx.Commit()
}

func canonicalValues(values ConfigValues) []byte {
	var decoded map[string]any
	_ = json.Unmarshal(jsonValue(values), &decoded)
	return jsonValue(decoded)
}
