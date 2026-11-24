//go:build linux || darwin

package ink

import (
	"fmt"
)

func sameStack(a, b *PullStack) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

type stackMergeResult struct {
	Status  string `json:"status"`
	Details struct {
		ID           string `json:"uuid"`
		ExpectedHead string `json:"expected_head_sha"`
		Action       string `json:"merge_action"`
		Method       string `json:"merge_method"`
		BypassRules  bool   `json:"bypass_rules"`
		SHA          string `json:"sha"`
	} `json:"details"`
}

const stackMergeAction = "direct_merge"

func stackMergePath(repo Repository, number int) string {
	return fmt.Sprintf("/repos/%s/pulls/%d/merge-async", repo.Repo, number)
}
