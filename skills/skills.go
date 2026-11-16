// Package skills 将协作规则随 CLI 发布，初始化无需访问源码 checkout 或网络。
package skills

import _ "embed"

const Name = "ink-commons-workflow"

//go:embed ink-commons-workflow/SKILL.md
var Workflow []byte
