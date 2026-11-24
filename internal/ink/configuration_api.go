//go:build linux || darwin

package ink

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"strconv"
	"strings"

	"github.com/wujinhjun/ink-commons/internal/execution"
)

// Export reads a single SQLite snapshot and retains only declared policy.
func (s *Service) exportConfiguration(w http.ResponseWriter, r *http.Request) {
	records, err := s.store.configRecords()
	if err != nil {
		fail(w, err)
		return
	}
	global := ConfigValues{}
	repositories := map[string]ConfigValues{}
	for _, record := range records {
		if record.Values == nil {
			record.Values = ConfigValues{}
		}
		if record.Scope == "" {
			global = record.Values
		} else {
			repositories[record.Scope] = record.Values
		}
	}
	reply(w, http.StatusOK, map[string]any{"schema": 1, "global": global, "repositories": repositories})
}

func (s *Service) validateEffective(repo Repository) error {
	if repo.ReviewerRoute == nil {
		return nil
	}
	if provider, ok := s.providers[repo.ReviewerRoute.Provider].(interface{ ValidateDelivery() error }); ok {
		if err := provider.ValidateDelivery(); err != nil {
			return err
		}
	}
	return execution.ValidateDestination(s.providers, *repo.ReviewerRoute, true)
}

func (s *Service) validateAutoMergeIdentity(repo Repository) error {
	if repo.AutoMerge == nil || !repo.AutoMerge.Enabled {
		return nil
	}
	merger, err := s.up.merger(repo.AutoMerge.ReuseReaderIdentity)
	if err != nil {
		return err
	}
	if merger.command == "" {
		return fmt.Errorf("automatic merge requires a dedicated GitHub merger App")
	}
	if _, err := exec.LookPath(merger.command); err != nil {
		return fmt.Errorf("configured GitHub merge command is unavailable")
	}
	if err := checkCommandDigest(merger.command, merger.commandDigest); err != nil {
		return err
	}
	if sameAppCommand(repo.ReviewerCommand, merger.command) {
		return fmt.Errorf("reviewer and merger must use separate GitHub Apps")
	}
	return nil
}

// Configuration is a central daemon API. It never opens a business checkout.
func (s *Service) configuration(w http.ResponseWriter, r *http.Request) {
	scope := strings.ToLower(r.URL.Query().Get("repo"))
	if scope != "" && !ValidRepository(scope) {
		fail(w, apiError{400, "expected owner/repo"})
		return
	}
	configKey := r.URL.Query().Get("key")
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.Method != http.MethodGet && r.URL.Query().Get("pr") != "" {
		fail(w, apiError{400, "PR assignments use the work assign API"})
		return
	}
	if r.Method != http.MethodGet {
		if configKey == "" {
			fail(w, apiError{400, "key is required"})
			return
		}
		var input struct {
			Value json.RawMessage `json:"value"`
		}
		if r.Method == http.MethodPost {
			if err := decode(w, r, &input); err != nil {
				fail(w, err)
				return
			}
			if input.Value == nil {
				fail(w, apiError{400, "value is required (null is an explicit clear)"})
				return
			}
		}
		record, err := s.changeConfig(scope, configKey, input.Value, r.Method == http.MethodDelete)
		if err != nil {
			fail(w, err)
			return
		}
		reply(w, 200, record)
		return
	}
	effective, err := s.store.effectiveConfig(scope)
	if err != nil {
		fail(w, err)
		return
	}
	if pr := r.URL.Query().Get("pr"); pr != "" {
		if scope == "" || r.Method != http.MethodGet || r.URL.Query().Get("effective") != "true" {
			fail(w, apiError{400, "pr requires repo and effective=true"})
			return
		}
		number, err := strconv.Atoi(pr)
		if err != nil || number < 1 {
			fail(w, apiError{400, "invalid PR number"})
			return
		}
		work, err := s.store.loadWork(key(scope, number))
		if err != nil {
			fail(w, err)
			return
		}
		if work == nil {
			fail(w, apiError{404, "PR not observed yet"})
			return
		}
		applyPRValues(&effective, work)
		effective.Repository.ReviewerLogin = stringConfigValue(effective.Values["reviewer_login"])
		effective.Repository.ReviewerCommand = stringConfigValue(effective.Values["reviewer_command"])
		effective.Repository.HumanDeveloper = stringConfigValue(effective.Values["human_developer"])
		_ = json.Unmarshal(effective.Values["reviewer_route"], &effective.Repository.ReviewerRoute)
	}
	if r.URL.Query().Get("effective") == "true" {
		if configKey != "" {
			value, ok := effective.Values[configKey]
			if !ok {
				fail(w, apiError{400, "unknown runtime configuration key"})
				return
			}
			reply(w, 200, map[string]any{"value": value, "source": effective.Sources[configKey], "revision": effective.Revision})
		} else {
			reply(w, 200, effective)
		}
		return
	}
	records, err := s.store.configRecords()
	if err != nil {
		fail(w, err)
		return
	}
	record := recordMap(records)[scope]
	if configKey != "" {
		if _, ok := configFields[configKey]; !ok {
			fail(w, apiError{400, "unknown runtime configuration key"})
			return
		}
		value, present := record.Values[configKey]
		reply(w, 200, map[string]any{"value": value, "present": present, "revision": record.Revision})
	} else {
		reply(w, 200, record)
	}
}

func stringConfigValue(raw json.RawMessage) string {
	var value string
	_ = json.Unmarshal(raw, &value)
	return value
}
