//go:build linux || darwin

package ink

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"sort"

	"github.com/wujinhjun/ink-commons/internal/ink/statedb"
)

func readConfigRecords(tx *sql.Tx) ([]ConfigRecord, error) {
	rows, err := statedb.New(tx).ListRuntimeConfig(context.Background())
	if err != nil {
		// database/sql scan errors can quote the stored value when a row has an
		// invalid column type. Preserve the read stage and safe error details.
		return nil, diagnosticError("cannot read runtime configuration rows: " + redactedError(err))
	}
	var records []ConfigRecord
	for _, row := range rows {
		record := ConfigRecord{Scope: row.Scope, Revision: row.Revision}
		if err := json.Unmarshal([]byte(row.Payload), &record.Values); err != nil {
			return nil, err
		}
		if record.Values == nil || record.Revision < 1 {
			return nil, diagnosticError("invalid runtime configuration record")
		}
		if row.Scope != "" && !ValidRepository(row.Scope) {
			return nil, diagnosticError("invalid runtime configuration scope")
		}
		if err := validateValues(record.Values); err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	if len(records) > 0 {
		global, ok := recordMap(records)[""]
		if !ok {
			return nil, diagnosticError("runtime configuration is missing global settings")
		}
		for _, record := range records {
			if record.Revision > global.Revision {
				return nil, diagnosticError("runtime configuration generation is inconsistent")
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

// Seed runtime settings only in an explicitly initialized database.
func (s *stateStore) initializeConfig(config Config) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	records, err := readConfigRecords(tx)
	if err != nil {
		return err
	}
	if len(records) == 0 {
		if err := putConfigRecord(tx, ConfigRecord{Scope: "", Values: ConfigValues(staticGlobal(config)), Revision: 1}); err != nil {
			return err
		}
		for _, repo := range config.Repositories {
			if err := putConfigRecord(tx, ConfigRecord{Scope: repo.Repo, Values: repositoryValues(repo), Revision: 1}); err != nil {
				return err
			}
		}
		records, err = readConfigRecords(tx)
		if err != nil {
			return err
		}
	}
	for _, record := range records {
		if _, err := effectiveFromRecords(records, record.Scope); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Called under Service.mu. Configuration revisions belong to runtime_config.
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
		var overrides ConfigValues
		if name != "" {
			overrides = indexed[name].Values
		}
		effective, err := s.resolveRepositoryPolicy(name, global, overrides)
		if err != nil {
			return record, err
		}
		inheritsCommand := scope == "" && key == "reviewer_command" && effective.Sources[key] != "repository"
		if name != "" && (name == scope || inheritsCommand) && mergeIdentityInput(key) {
			if err := s.validateRuntimeMergeIdentity(effective.Repository); err != nil {
				return record, err
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
		if record.Scope == "" {
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
	effective, err := s.resolveRepositoryPolicy(name, global, values)
	if err != nil {
		return Repository{}, false, err
	}
	if old, ok := indexed[name]; ok {
		if !bytes.Equal(canonicalValues(old.Values), canonicalValues(values)) {
			return Repository{}, false, apiError{409, "repository already registered with different overrides"}
		}
		return effective.Repository, false, tx.Commit()
	}
	if err := s.validateRuntimeMergeIdentity(effective.Repository); err != nil {
		return Repository{}, false, err
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
