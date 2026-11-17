package ink

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"ink-commons/internal/execution"
)

// ConfigValues contains only explicit overrides. A missing key inherits; a JSON
// null is a deliberate clear. RawMessage preserves false, zero and null.
type ConfigValues map[string]json.RawMessage
type GlobalConfig ConfigValues
type RepositoryOverride ConfigValues

type ConfigRecord struct {
	Scope    string       `json:"scope"`
	Values   ConfigValues `json:"values"`
	Revision int64        `json:"revision"`
}

type EffectiveConfig struct {
	Repository Repository        `json:"repository"`
	Execution  Reliability       `json:"execution"`
	Values     ConfigValues      `json:"values"`
	Sources    map[string]string `json:"sources"`
	Revision   int64             `json:"revision"`
}

type configField struct {
	Default  json.RawMessage
	Nullable bool
	Check    func(json.RawMessage) error
}

func checked[T any](value json.RawMessage) error {
	var target T
	d := json.NewDecoder(bytes.NewReader(value))
	d.DisallowUnknownFields()
	if err := d.Decode(&target); err != nil {
		return err
	}
	if err := rejectNestedNull(value); err != nil {
		return err
	}
	if d.Decode(new(any)) != io.EOF {
		return fmt.Errorf("expected one JSON value")
	}
	return nil
}

func jsonValue(v any) json.RawMessage { b, _ := json.Marshal(v); return b }

// One registry defines the public keys, types, clear semantics and defaults.
// Objects are atomic; only execution exposes independently inheritable leaves.
var configFields = func() map[string]configField {
	fields := map[string]configField{
		"reviewer_login":   {jsonValue(""), false, checked[string]},
		"reviewer_command": {jsonValue(""), false, checked[string]},
		"human_developer":  {jsonValue(""), false, checked[string]},
		"task_mode": {jsonValue("legacy"), false, func(raw json.RawMessage) error {
			var mode string
			if err := json.Unmarshal(raw, &mode); err != nil {
				return err
			}
			if mode != "legacy" && mode != "cooperative" {
				return fmt.Errorf("must be legacy or cooperative")
			}
			return nil
		}},
		"reviewer_route": {jsonValue(nil), true, checked[execution.Route]},
		"postApproval":   {jsonValue(nil), true, checked[ApprovalHook]},
		"review":         {jsonValue(nil), true, checked[ReviewFeatures]},
		"auto_merge":     {jsonValue(nil), true, checked[AutoMergeConfig]},
	}
	var defaults map[string]json.RawMessage
	b := jsonValue((Reliability{}).defaults())
	_ = json.Unmarshal(b, &defaults)
	for key, value := range defaults {
		fields["execution."+key] = configField{value, false, func(raw json.RawMessage) error {
			var n int
			if err := json.Unmarshal(raw, &n); err != nil {
				return err
			}
			if n < 1 {
				return fmt.Errorf("must be a positive integer; unset restores the default")
			}
			return nil
		}}
	}
	return fields
}()

func validateValues(values ConfigValues) error {
	for key, value := range values {
		field, ok := configFields[key]
		if !ok {
			return fmt.Errorf("unknown runtime configuration key %q", key)
		}
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			if !field.Nullable {
				return fmt.Errorf("%s cannot be null; use unset to inherit", key)
			}
		} else if err := field.Check(value); err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
	}
	return nil
}

func resolveConfig(name string, global GlobalConfig, override RepositoryOverride, revision int64) (EffectiveConfig, error) {
	effective := EffectiveConfig{Values: ConfigValues{}, Sources: map[string]string{}, Revision: revision}
	for key, field := range configFields {
		effective.Values[key] = append(json.RawMessage(nil), field.Default...)
		effective.Sources[key] = "builtin"
	}
	for _, layer := range []struct {
		values ConfigValues
		source string
	}{{ConfigValues(global), "global"}, {ConfigValues(override), "repository"}} {
		if err := validateValues(layer.values); err != nil {
			return effective, err
		}
		for key, value := range layer.values {
			effective.Values[key] = append(json.RawMessage(nil), value...)
			effective.Sources[key] = layer.source
		}
	}
	repoJSON, executionJSON := map[string]json.RawMessage{}, map[string]json.RawMessage{}
	for key, value := range effective.Values {
		if strings.HasPrefix(key, "execution.") {
			executionJSON[strings.TrimPrefix(key, "execution.")] = value
		} else {
			repoJSON[key] = value
		}
	}
	if err := json.Unmarshal(jsonValue(repoJSON), &effective.Repository); err != nil {
		return effective, err
	}
	effective.Repository.Repo = name
	if err := json.Unmarshal(jsonValue(executionJSON), &effective.Execution); err != nil {
		return effective, err
	}
	if err := effective.Execution.Validate(); err != nil {
		return effective, err
	}
	if err := effective.Repository.PostApproval.Validate(); err != nil {
		return effective, err
	}
	if err := effective.Repository.AutoMerge.Validate(); err != nil {
		return effective, err
	}
	if err := effective.Repository.ValidateReviewFeatures(); err != nil {
		return effective, err
	}
	if name != "" && strings.TrimSpace(effective.Repository.ReviewerLogin) == "" {
		return effective, fmt.Errorf("reviewer_login is required for %s", name)
	}
	return effective, nil
}

// Historical repository objects were complete settings, not patches. Include
// their empty and nil fields so migration cannot unexpectedly enable inheritance.
func legacyRepositoryValues(repo Repository) ConfigValues {
	return ConfigValues{
		"reviewer_login": jsonValue(repo.ReviewerLogin), "reviewer_command": jsonValue(repo.ReviewerCommand),
		"human_developer": jsonValue(repo.HumanDeveloper), "reviewer_route": jsonValue(repo.ReviewerRoute),
		"postApproval": jsonValue(repo.PostApproval), "review": jsonValue(repo.Review), "auto_merge": jsonValue(repo.AutoMerge),
		"task_mode": jsonValue(normalTaskMode(repo.TaskMode)),
	}
}

func normalTaskMode(mode string) string {
	if mode == "" {
		return "legacy"
	}
	return mode
}

func sparseRepositoryValues(repo Repository) ConfigValues {
	values := ConfigValues{}
	_ = json.Unmarshal(jsonValue(repo), &values)
	delete(values, "repo")
	if repo.ReviewerLogin == "" {
		delete(values, "reviewer_login")
	}
	return values
}

// Atomic objects may be cleared at the top level. Their non-nullable members
// cannot silently coerce null into Go's false, zero or empty string.
func rejectNestedNull(raw json.RawMessage) error {
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return err
	}
	var walk func(any) error
	walk = func(v any) error {
		if v == nil {
			return fmt.Errorf("nested null is not supported; clear the whole object or omit the member")
		}
		switch node := v.(type) {
		case map[string]any:
			for _, child := range node {
				if err := walk(child); err != nil {
					return err
				}
			}
		case []any:
			for _, child := range node {
				if err := walk(child); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return walk(value)
}
