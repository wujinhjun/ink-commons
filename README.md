# Ink Commons 同砚

同砚跟踪 GitHub PR 的开发和审查交接。第一版提供 Go daemon、Unix socket HTTP API 和 Git hook。人和 agent 都可以在 GitHub 上开发、送审和提交正式 review。

## 开始

需要 Go 1.23+ 和 Git。daemon 跟踪配置中的仓库。私有仓库使用具备 Pull requests 读取权限的 GitHub token，通过 `INK_GITHUB_TOKEN` 注入。配置中的 `reviewer_login` 要和 GitHub 上实际提交 review 的身份一致。可以把任务留给人工处理，或配置执行端自动接单。

```sh
go build -o ink ./cmd/ink
cp config.example.json config.json
./ink serve -config config.json
```

默认状态目录是 `$XDG_STATE_HOME/ink-commons` 或 `~/.local/state/ink-commons`。daemon 与 hook 可以通过同一个 `INK_DATA_DIR` 覆盖。HTTP API 通过仅当前用户可访问的 `ink.sock` 提供。配置文件使用 `config.example.json` 的结构，凭据从环境变量读取。

受管仓库从 `git rev-parse --git-path hooks/pre-push` 确认默认 hook 位置。使用 `core.hooksPath` 的工作区按该设置安装。没有现有 hook 时，把 `scripts/ink-pre-push` 安装为可执行的 `pre-push`。已有 hook 时，从现有 hook 为同砚和原检查分别传入同一份标准输入。让执行 Git 命令的进程能在 PATH 找到 `ink`，或通过 `INK_BIN` 指定绝对路径。

## 代码结构

| 目录 | 职责 |
|---|---|
| `cmd/ink/` | CLI 入口，读取配置和凭据，注册执行端并启动 daemon。 |
| `internal/ink/` | GitHub 同步、PR 工作流、本地 API、Git hook 和状态存储。 |
| `internal/execution/` | 定义 `Route`、`Delivery` 和 `Executor`，供工作流和执行端共同使用。 |
| `internal/execution/ccconnect/` | 创建 Discord thread，并通过 cc-connect webhook 投递任务。 |
| `internal/execution/httpagent/` | 通过版本化 HTTP 协议接入其他 agent runtime。 |

同砚核心从命令入口接收已注册的执行端，通过 `Executor` 验证路由、准备会话和投递任务。兼容现有 HTTP 协议的服务可以直接配置接入。新增原生执行端时，实现这个接口并在 `cmd/ink` 注册即可。

## 执行端

每个 PR 的开发和审查任务可以交给人或不同的执行端。配置 `reviewer_route` 指定默认审查执行端。路由包含 `provider`、`target`，以及可选的 `session` 和 `parent`。已有会话可以直接指定 `session`。需要新建会话时，由执行端根据目标和上级位置创建，随后同砚保存会话标识供复审使用。

### cc-connect

配置 `cc_connect_url` 为本机 webhook 完整地址，例如 `http://127.0.0.1:9111/hook`，并注入 `INK_CC_CONNECT_TOKEN`。例如：

```json
{
  "provider": "cc-connect",
  "target": "reviewer-project",
  "parent": "123456789012345678"
}
```

`target` 是 cc-connect project。省略 `parent` 时，从工作项关联的公开开发 thread 或文本频道解析 Discord 父频道，在同一频道下创建审查 thread。名称包含 PR 编号和开发 thread 的标题。显式 `parent` 可以覆盖创建位置；已有仓库默认路由中的 `parent` 继续生效。自动推导只支持普通文本父频道，不支持论坛、媒体或公告父频道，也不支持公告 thread 和私有 thread，会报明确错误；显式指定文本频道 `parent` 代表选择独立的公开审查位置，仍创建公开 thread。来源无法读取或不适用于自动推导时，显式位置仍可使用，名称回退为 `Review owner/repository #42`，不复制私有来源标题。需要注入能读取开发会话并在目标频道创建、访问公共 thread 的 `INK_DISCORD_TOKEN`。也可以直接传 `session: "discord:<thread-id>"` 复用已有会话。Git hook 识别 cc-connect 注入的 `CC_PROJECT` 和 `CC_SESSION_KEY`。旧版的 `reviewer_project`、`review_channel_id` 和 `project` / `session_key` 路由输入也能读取。查询 API 统一返回新路由；原 `review_thread_id` 可从 `reviewer.route.session` 中取得。

### 通用 HTTP

配置 `http_provider_url` 为执行端的接收地址，再用 `{"provider":"http","target":"reviewer"}` 作为仓库的 `reviewer_route`。执行端可以是一个本地服务，也可以连接其他 agent runtime。远端地址需要 HTTPS，并通过 `INK_HTTP_PROVIDER_TOKEN` 提供 Bearer 认证。

同砚向该地址发送 JSON POST，包含 `version: 1`、`operation`、`route` 和 `task`。任务含 `repository`、`pull_number`、`pull_url`、`role`、`sha`，以及适用的 `review_id`、`review_url` 和 `source`（开发会话的路由）。执行端按操作返回 HTTP 2xx 和 JSON：

| 操作 | 返回值 |
|---|---|
| `prepare` | `{"session":"your-session-id"}`。当路由缺少会话时调用。 |
| `deliver` | `{"status":"accepted"}`。执行端已接收任务。 |

其他运行时可在启动开发会话时设置 `INK_PROVIDER=http`、`INK_TARGET` 和 `INK_SESSION`，Git hook 就能记录它们的来源。

## 走完一轮

1. 开发 agent 在自己的会话里建分支、推送并开 Draft PR。hook 从当前会话环境读取路由，并登记 Git 准备推送的源仓库、目标分支和 SHA。daemon 轮询 GitHub 找到匹配的 PR 后确认关联。同一版本有两个候选会话时，PR 等待认领。
2. 在 GitHub 标记 Ready for review。同砚为当前 head SHA 建立待办。配置了审查执行端时，创建或复用对应会话并投递任务。可以将随附的 review skill 提供给对应 agent。
3. reviewer 在 GitHub 提交 Approve 或 Request changes。机器人和人在 GitHub 上提交的 review 使用同一同步逻辑。只有指定 reviewer 对当前 SHA 的正式结论才会推进状态。需要修改时把任务投递给开发会话，或留给当前人工负责人。
4. 新 commit 将进入下一轮，沿用已有审查会话。历史 review 按原 SHA 保留。工作项合并或关闭后结束待办。

GitHub 的当前 PR 快照是状态来源。轮询间隔默认 60 秒。hook 路由候选保留七天，仅精确匹配 PR 已观察到的 head SHA。漏装 hook、首次观察之前 head 已变化或归属含糊时，可以用 `link` 补登记。

## 为单条 PR 指定 reviewer

GitHub 上开 Draft PR，等同砚发现并关联后，在 Mini 上执行：

```sh
ink review --github-login 'yanshi-reviewer[bot]' --github-command yanshi-reviewer \
  --target mo-cangli-pi \
  https://github.com/owner/repository/pull/42
```

`--github-command` 指定 reviewer 使用的 GitHub CLI / App 入口，随期望的 `github_login` 一起传给 Agent；身份不符的 review 会告警并保留原审查状态。`--target` 是 cc-connect 项目。不传 `--parent` 时，使用工作项所绑定的开发 thread 的父频道；审查 thread 命名为 `Review owner/repository #42 · 开发 thread 标题`。工作项需通过 hook 或 `link` 关联开发会话。省略父频道且缺少有效 Discord 来源时，指派请求会立即报错；来源的远端读取失败会在下一次投递时记录到 `task.error`，不会任意选择其他频道。`--parent` 可显式覆盖创建位置。`--github-login` 是正式提交 review 的 GitHub 身份。用 `--session discord:THREAD_ID` 可以复用已有会话，只传 `--github-login` 则交给真人。开发 agent 可以用同一条命令代办。

设好 reviewer 后，在 GitHub 标记 Ready for review。同砚在下一轮轮询中创建审查会话并派发任务。PR 已经是 Ready 时，指派会直接进入待派发。重复提交相同指派会保留已有会话和投递状态。修改后推送新 commit，同砚会沿用该 reviewer 复审。

## 本地 API

```sh
socket="${INK_DATA_DIR:-${XDG_STATE_HOME:-$HOME/.local/state}/ink-commons}/ink.sock"
curl --unix-socket "$socket" http://ink/v1/work
curl --unix-socket "$socket" http://ink/v1/work/owner/repository/42
curl --unix-socket "$socket" http://ink/v1/work/owner/repository/42/assign \
  -H 'Content-Type: application/json' \
  -d '{"role":"developer","github_login":"your-login"}'
```

| 接口 | 行为 |
|---|---|
| `GET /healthz` | 检查本地 API 可达。 |
| `GET /v1/work` | 列出已观察的 PR 和当前待办。 |
| `GET /v1/work/{owner}/{repo}/{number}` | 查看一条 PR 的来源、负责人、审查版本和投递状态。 |
| `POST /v1/work/{owner}/{repo}/{number}/link` | 手动关联未认领 PR。请求体提供 `github_login` 或 `route`。路由包含 `provider`、`target`、`session`。 |
| `POST /v1/work/{owner}/{repo}/{number}/assign` | 为 `developer` 或 `reviewer` 更新负责人。传 `github_login` 即可转给人。传 `route` 可指定已有 bot 会话。指定 reviewer 时也可以省略 `session`，让执行端按 `provider`、`target` 和适用的 `parent` 创建会话。 |
| `POST /v1/work/{owner}/{repo}/{number}/retry` | 在下次轮询重试当前 bot 待办。所有旧轮次已确认结束才允许重试；保留 task_id，生成新的 attempt_id。 |
| `POST /v1/work/{owner}/{repo}/{number}/confirm-prepared` | 人工关联创建结果不明的审查 thread。提供当前 task_id、包含已有 session 的 route 和 reason。 |
| `POST /v1/work/{owner}/{repo}/{number}/confirm-stopped` | 人工确认指定轮次已停止且延迟投递已被禁止。必须提供 task_id、attempt_id、stopped: true 和 reason，保留确认记录。 |
| `POST /v1/pushes` | Git hook 提交候选关联记录。 |

`assign` 修改未来任务的投递。新 SHA、改派、Draft 和 PR 关闭都会保留旧轮次记录；旧执行未确认结束时，不会投递后续 bot 任务。接手之前如果 bot 正在工作，先在原执行端停止它并禁止延迟投递，再接管目录或处理未提交改动。`accepted` 表示执行端接收请求，正式 GitHub review 或新的 PR 版本才表示工作状态推进。未启用受管入口的旧 cc-connect webhook 忙时仍可能跳过任务，待办可在 API 中查看并人工处理。受管入口会持久化排队。

开发计划与完成记录统一维护在 [TODO.md](TODO.md)。

## 执行可靠性

工作项的 `phase` 继续由 GitHub review 和 head SHA 决定；`task.runs` 单独记录投递、开始 ACK、心跳、结束、停止请求和重试计划。`execution_history` 保存已被业务状态替换的任务。`workflow` 表示等待 Agent、等待审查结论、等待新 SHA 或需要处理；`alerts` 保留身份 / SHA 不符、执行不明、缺少结果和重试耗尽的诊断及通知状态。执行成功本身不会把 PR 标成审查通过。

HTTP 执行端可以通过 `http_provider_execution: true` 显式启用 `status` / `stop` 协议。默认关闭，旧 HTTP 服务不会收到额外操作。执行端必须持久化去重和运行归属，并保证终态表示 Agent 及所有工具已停止、延迟投递不能重新启动旧轮次。仓库提供 SQLite 执行端账本及恢复测试，接入约定见 [执行协议](docs/execution.md)。

cc-connect 的配套受管入口启用后，设置 `cc_connect_execution: true`。服务必须支持 `cc_connect_url + "/execution"`，并在自己的 `[webhook]` 中配置私有 `execution_state` 文件和 token。未升级服务会明确报错，不会退回不可靠的旧投递。受管入口在回复成功前保存任务、忙时持久化排队，并返回开始 ACK、心跳和确认终止后的回执。目前支持 Codex 和 Pi RPC；其他 Agent 须实现终止能力接口。接入示例和两个仓库的联合测试见 [执行协议](docs/execution.md#cc-connect-managed-integration)。

配置 `execution` 可设置 `ack_seconds`（默认 120）、`queue_seconds`（3600）、`result_seconds`（120）、`heartbeat_seconds`（180）、`execution_seconds`（3600）、`backoff_seconds`（30）、`max_attempts`（3，含首次投递）。零值使用默认值，`max_attempts: 1` 关闭自动重派。超时先查询、再请求停止；只有终止确认后才按指数退避重试，退避上限一小时。耗尽次数或无法确认停止时需要人工处理。每轮 daemon 轮询先恢复执行状态，即使 GitHub 暂时不可用也会核查执行端。

Agent 结束后，同砚仍持久化等待正式审查结论或新 SHA。缺少正式审查结论超过 `result_seconds` 时，只在上一轮已确认结束后自动跟进 reviewer，受同一 `max_attempts` 和退避约束。开发完成却没有新 SHA 会明确告警。合法 Request changes 会派发到开发会话；新 SHA 会沿用 reviewer 复审。通知通过受管 cc-connect 入口持久化排队，重启后继续发送；API 中也能看到诊断，不依赖发起 Agent 一直在线。

旧 webhook 和升级前的历史投递仍保持保守模式：无法证明旧执行停止时不重派。人工停止并禁止延迟投递后，调用 `confirm-stopped`；受管 cc-connect 会先在执行端记录该确认，再更新同砚。创建 thread 的预留结果不明时，还需通过 `confirm-prepared` 关联已有 thread，确认前不会再次创建。人工声明不会自行停止外部进程。

## 状态、备份与回滚

daemon 将状态写入状态目录中的 `state.sqlite3`，使用 schema version 1。PR 和 push 候选分别存入表，PR 记录保留完整的负责人、路由、审查历史及当前投递状态。每次读取和更新使用 SQLite 事务，进程内串行修改；仍使用 `state.json.lock` 排除其他 daemon，包括旧版进程。数据库和备份文件权限为 `0600`。

首次启动时，若存在 `state.json`，daemon 校验数据并生成 `state.json.pre-sqlite.bak`，然后在单个事务中导入并记录 schema 版本。原 JSON 不会被修改。导入失败时不会留下部分记录，修复 JSON 后可以重新启动；备份已存在但内容不同时会拒绝导入，需先另行保存两份文件再处理。数据库初始化完成后只读取 SQLite，不会再次导入遗留 JSON。初始化成功后、接单前持久化 `state.sqlite3.initialized` 标记。已有该标记而数据库缺失、为空或变成无版本空库时，启动会拒绝导入陈旧 JSON；迁移备份存在但数据库缺失时同样拒绝。应恢复 SQLite 备份；若确实要从 JSON 重新迁移，须先归档初始化标记和旧迁移备份。未提交完成的首次迁移（无标记且空库仍在）可正常重试。已知旧格式的 `project` / `session_key` 路由和 `review_thread_id` 会映射到新路由；重复字段冲突时拒绝迁移。其他未知 JSON 字段也会拒绝导入：保留原文件和备份，先确认写入它的版本并用兼容版本转换，或核实字段用途后人工修正，避免直接删除可能承载路由的字段。遇到未知 schema 版本或损坏数据时启动失败，不会重置数据库。

以下操作均先停止 daemon，`-data` 可省略以使用默认状态目录。输出文件必须尚不存在。备份通过 SQLite `VACUUM INTO` 生成独立快照；目标文件系统不支持硬链接时，使用独占创建、复制并同步的方式，仍不覆盖已有文件。这种回退在进程异常中断时可能留下不完整的输出文件，恢复前应验证或重新生成；目录同步不受文件系统支持时只能尽力保证目录项持久化。导出包含迁移之后的最新记录，保持旧版 JSON 结构。

```sh
ink state backup -data /path/to/ink-state -output /safe/path/ink-backup.sqlite3
ink state export -data /path/to/ink-state -output /safe/path/ink-rollback.json
```

恢复 SQLite 备份：停止 daemon，将当前 `state.sqlite3` 移到另一个位置留存，再将备份复制为状态目录中的 `state.sqlite3`，确认权限为 `0600`，然后启动 daemon。不要在运行期间直接复制数据库文件。

回滚到旧版程序：停止 daemon，先执行上面的 backup 和 export；将当前数据库、`state.sqlite3.initialized` 标记、旧 `state.json` 及 `state.json.pre-sqlite.bak` 一起移到独立归档目录，再将最新导出的 JSON 复制为状态目录中的 `state.json`（权限 `0600`），最后使用旧版程序启动。以后重新升级时，保持状态目录中没有旧 SQLite 数据库、初始化标记及旧迁移备份，让新版重新导入旧版运行期间更新过的 JSON。直接使用迁移前的备份只会恢复迁移时刻的数据。
