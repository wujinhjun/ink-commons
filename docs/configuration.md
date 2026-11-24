# 中心化配置

Ink Commons 集中管理所有仓库的配置和状态。业务仓库不需要 `.ink` 文件、专用分支、GitHub 配置或 Git hook，也不需要知道同砚的存在。操作者可以在任意目录执行 `ink register owner/repository` 和 `ink config ... --repo owner/repository`。可选 pre-push hook 只是开发会话来源的一种上报方式；也可由中心 API 的 `link` 登记，不是配置分发机制。


daemon 静态配置中的 `log_level` 设置最低日志级别，默认 `info`，也可设为 `debug`、`warn` 或 `error`。`log_format` 支持 `text`（默认）和 `json`。日志记录操作、仓库、PR 编号、告警代码，以及 HTTP 状态、超时和 SQLite 错误码等诊断信息。告警明细可通过私有 Unix socket 查询。

## 存储与作用域

- JSON 的 `DaemonConfig` 管理静态启动参数：`poll_seconds`、`github_command`、`providers`（参见 [HTTP Adapter 配置](adapters.md)）。状态目录和部署 gate 沿用启动参数；凭据来自 daemon 环境。修改这些参数需要重启。
- SQLite 的 `GlobalConfig` 管理全局运行期默认值。
- SQLite 的 `RepositoryOverride` 只保存单仓库显式覆盖；注册记录即使为空对象也表示已注册。
- `EffectiveConfig` 合并内置默认值、全局配置和仓库覆盖，返回值、来源和单调递增的配置 generation。PR 的显式负责人指派位于最高层，持久化在中心工作项。

最终优先级：`内置默认值 < SQLite 全局配置 < SQLite 仓库覆盖 < PR 显式指派`。PR 层仅包含负责人、对应 GitHub 命令和路由，不支持覆盖任意执行策略。运行期不再读取业务仓库文件，也不把合并结果回写成覆盖项。

| 键 | 类型 / 内置值 | 继承与清空 |
|---|---|---|
| `reviewer_login` | string / 空；注册仓库时必须有有效值 | 全局、仓库；PR reviewer 指派覆盖 |
| `reviewer_command` | string / 空 | 全局、仓库；空字符串清空；PR 指派覆盖 |
| `human_developer` | string / 空 | 全局、仓库；PR developer 指派覆盖 |
| `reviewer_route` | Route / null | 全局、仓库；整体替换；null 选择人工；PR 指派覆盖 |
| `task_mode` | `legacy` 或 `cooperative` / legacy | 全局、仓库 |
| `review` | ReviewFeatures / null | 全局、仓库；整体替换；null 关闭 |
| `auto_merge` | AutoMergeConfig / null | 全局、仓库；整体替换；null 关闭 |
| `postApproval` | ApprovalHook / null | 全局、仓库；整体替换；null 禁用 |
| `task_blocked` | TaskBlockedHook / null | 全局、仓库；整体替换；默认关闭 |
| `execution.ack_seconds` | int / 120 | 全局、仓库，按字段覆盖 |
| `execution.heartbeat_seconds` | int / 180 | 同上 |
| `execution.execution_seconds` | int / 3600 | 同上 |
| `execution.backoff_seconds` | int / 30 | 同上 |
| `execution.result_seconds` | int / 120 | 同上 |
| `execution.max_attempts` | int / 3 | 同上；1 关闭自动重派 |

字段缺省表示继承；`unset` 删除本层覆盖。`null` 仅能清空上表的可空对象，不能代替 `unset`。`false` 保持明确关闭的含义，例如 `auto_merge: {"enabled": false}`，不会被全局 true 覆盖。路由、hook、review 和 auto_merge 的内部字段不跨作用域拼接；完整对象替换后，其缺省成员使用该对象本身的默认语义。对象内非空成员不接受 null。

运行期 execution 数值必须为正整数，超时 / 退避上限 604800 秒，max_attempts 上限 100。零值不会表示继承；请用 `unset`。旧 JSON 中零值表示默认的历史语义在导入时转换为缺省。

## GitHub CLI 适配

`github_command` 和 `github_merger_command` 指向服务用户可运行的绝对路径。可以在启动 JSON 中设置 `github_read_args` 和 `github_merge_args`，分别替换读取和合入命令的参数前缀。参数按数组逐项传入，不经过 shell。`{repo}` 会替换为当前请求的 `owner/repo`。合入参数也用于显式复用 reader App 时的合入调用。

字段缺省或为 `null` 时沿用原有参数 `--profile inspect|code -R owner/repo`。`[]` 表示直接调用 `api` 子命令，适用于采用标准 GitHub CLI 参数的客户端或包装器。它们需支持 `api --include --method METHOD ENDPOINT`，按需接受 `--header` 和 `--input -`，并在标准输出返回含状态行和响应头的 HTTP 响应。macOS 自动升级也使用读取前缀，并要求客户端支持 `api … --jq` 和 `release download`。

参数数组只放命令选项。凭据由服务用户运行的客户端或包装器提供。直接使用 `gh` 时，它会采用该进程可用的 GitHub 身份。需要仓库范围的短期 App 凭据时，使用可按 `{repo}` 申请凭据的包装器。自动合入沿用现有身份策略和摘要校验。

## CLI

先启动 daemon，再集中设置默认值和注册仓库；无需进入目标 checkout：

```sh
ink config set reviewer_login 'reviewer[bot]' --global
ink config set reviewer_command reviewer-cli --global
ink config set reviewer_route '{"provider":"cc-connect","target":"reviewer-project"}' --global
ink register owner/repository

ink config list --global
ink config list --repo owner/repository
ink config list --repo owner/repository --effective
ink config get reviewer_login --repo owner/repository --effective
ink config get reviewer_login --repo owner/repository --pr 42 --effective

ink config set execution.max_attempts 5 --repo owner/repository
ink config set postApproval null --repo owner/repository
ink config unset postApproval --repo owner/repository
```

所有操作必须显式选择 `--global` 或 `--repo`。`set` 值支持 JSON，也接受未加 JSON 双引号的普通字符串；字符串 `null`、`false` 或数字如需作为字符串，必须额外使用 JSON 双引号。`--effective` 仅用于 get/list，输出最终值和 `builtin`、`global`、`repository`、`pr` 来源。`--json` 输出含 revision 的 API JSON，便于脚本处理。

`--pr` 的 effective 视图展示当前配置与 PR 显式指派的合并结果；正在进行的任务实际使用其 `config_snapshot`，应通过工作项 / task API 查看。配置查询不触发派发。修改配置立即提交 SQLite，不用重启；下一次任务准备时重新读取，不要求等待下一轮 GitHub 同步。轮询周期仍是静态启动参数。

注册只存传入的覆盖项，不展开全局默认值。对已注册仓库不带覆盖项再次 register 返回当前配置；完全相同的显式覆盖项重复注册是幂等的。冲突不会覆盖已有配置，应通过 set/unset 管理。显式值即使与当前全局值相等也会保留，直到 unset。当前没有注销接口。

## API 与写入一致性

CLI 经仅本地 daemon 用户可访问的 Unix socket 调用：

- `GET /v1/config?repo=owner/repository`：本层覆盖；省略 repo 查询全局。
- `GET /v1/config?repo=owner/repository&effective=true`：合并结果和逐键来源。
- 添加 `key=reviewer_login` 查询单键；原始视图以 `present` 区分缺省和显式 null。
- 添加 `pr=42&effective=true` 查看 PR 显式指派来源，仅支持已观察 PR。
- `POST /v1/config?repo=owner/repository&key=postApproval`，body `{"value":null}`：设置值。
- `DELETE /v1/config?repo=owner/repository&key=postApproval`：删除覆盖项。

写入在 Service 锁和单个 SQLite 事务内完成：读取 → 修改 → 校验最终配置及所有受影响仓库 → 递增 generation → 提交。修改全局配置不能使已注册仓库失去 reviewer 或产生不合法的路由 / review 组合。非法请求、SQL 错误均不会提交部分更新。

`runtime_config` 使用专用 sqlc 查询和独立更新接口，不参与工作流的完整状态快照保存。投递和状态恢复中的旧快照不能覆盖配置。CLI 不直接打开数据库，不支持绕过 daemon 的外部 writer；不要在运行期间手工编辑 SQLite。

## 任务生效边界

尚未派发的任务在准备时读取最新配置。首次外部动作前持久化 `Task.config_snapshot`，包含有效配置、逐键来源、generation、实际负责人、来源路由和 SHA：

- legacy：首次 prepare / deliver 之前。
- cooperative：首次 prepare / wake 之前；未经 wake 直接 claim 时，在接受 claim 前冻结。
- 人工 legacy 任务：首次观察并展示给人工时冻结。

同一 task_id 的执行恢复和重试保留快照。cooperative 的显式 blocked retry 原有语义是创建新 task_id，因此新任务会读取新配置。新 SHA、下一业务任务或显式改派创建的新任务也会重新解析。已经派发的旧任务仍须按原有停止 / handoff 规则结束，改配不会释放旧执行的归属。

当前 SHA 的 reviewer 在本轮首次暴露后固定，避免改默认 reviewer 重新解释既有批准；新 SHA 使用新默认值。开发任务可采用最新执行策略，但保留前一审查轮次的 reviewer，其快照来源可标为 `round_snapshot`。分配出的 session 是运行状态，只有目标 provider、target、parent 和 reviewer 身份兼容时才跨任务复用。

历史读取在发起请求和返回结果前均核对当前任务的快照权限、来源及读取边界。当前仓库关闭历史开关可撤销访问；之后重新开启不能扩大已派发任务的快照权限，改派开发者也不会切换该 reviewer 任务的来源 thread。

轮询最多同时发现 4 个仓库、处理 8 个 PR。同一 PR 的后台工作串行执行。批准后先持久化待执行事件，合入判断完成后由独立的 2 个 worker 执行 postApproval。执行前重验 PR head、批准和配置。hook 默认超时 60 秒，可通过 `timeout_seconds` 覆盖。按仓库、PR、SHA 去重，失败或结果不明的事件不会自动重放。

## 初始化与备份

`ink state init` 显式创建 schema v6 数据库。首次启动在同一事务写入启动 JSON 中的 `repository_defaults`、`execution` 和 `repositories`。静态仓库条目按完整覆盖写入，未设置的可空字段保留为显式 `null`。之后用 `ink config` 更新 SQLite 中的运行期配置。

用 `ink state backup` 保存可恢复的 SQLite 快照。JSON 导出供诊断和人工核对。旧库切换按[单实例检查清单](migration.md)执行。
