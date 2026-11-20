# Ink Commons 同砚

同砚跟踪 GitHub PR 的开发和审查交接。第一版提供 Go daemon、Unix socket HTTP API 和 Git hook。人和 agent 都可以在 GitHub 上开发、送审和提交正式 review。

## 开发与审查流程

下图说明 cooperative 模式下的任务领取、审查、返工交接与可选自动合入；右侧按环节列出配置项及作用范围，配置值均使用占位示例。

![同砚 cooperative 开发与审查流程及配置说明](docs/diagrams/cooperative-workflow.png)

[查看原图](docs/diagrams/cooperative-workflow.png) · [Excalidraw 可编辑源文件](docs/diagrams/cooperative-workflow.excalidraw) · [文档索引](docs/README.md)

## 开始

需要 Go 1.23+ 和 Git。daemon 跟踪中心注册的仓库，业务仓库无需配置文件或 hook。私有仓库使用具备 Pull requests 读取权限的 GitHub token，通过 `INK_GITHUB_TOKEN` 注入。配置中的 `reviewer_login` 要和 GitHub 上实际提交 review 的身份一致。可以把任务留给人工处理，或配置执行端自动接单。

```sh
go build -o ink ./cmd/ink
./ink init
./ink init --check
cp config.example.json config.json
./ink serve -config config.json
```

默认状态目录是 `$XDG_STATE_HOME/ink-commons` 或 `~/.local/state/ink-commons`。daemon 与 hook 可以通过同一个 `INK_DATA_DIR` 覆盖。HTTP API 通过仅当前用户可访问的 `ink.sock` 提供。配置文件使用 `config.example.json` 的结构，凭据从环境变量读取。

如果需要通过可选 hook 自动记录开发会话来源，受管仓库从 `git rev-parse --git-path hooks/pre-push` 确认默认 hook 位置。使用 `core.hooksPath` 的工作区按该设置安装。没有现有 hook 时，把 `scripts/ink-pre-push` 安装为可执行的 `pre-push`。已有 hook 时，从现有 hook 为同砚和原检查分别传入同一份标准输入。让执行 Git 命令的进程能在 PATH 找到 `ink`，或通过 `INK_BIN` 指定绝对路径。

## 初始化 Agent 协作指令

构建或升级 CLI 后，以实际运行 Agent 的用户执行：

```sh
ink init
ink init --check
```

`init` 将随二进制携带的 `ink-commons-workflow` skill 写入 `$XDG_DATA_HOME/ink-commons/skills/<内容哈希>/`（默认 `~/.local/share/ink-commons/skills/`），并在 `~/.agents/skills/ink-commons-workflow` 建立目录链接。Pi 与 Codex 共用这个发现位置；不需要源码 checkout，不调用网络、不启动服务、不注册仓库，也不修改已有任务。旧版只有审查 skill，完整协作 skill 现在包含首次送审、领取 review/返工任务、交接和结束回合的约定。

重复执行会检查资源并更新同砚管理的旧版本链接，缺失资源可重新安装。用户已有的同名文件、目录或其他来源链接会报冲突并保留；同砚资源被手动修改时也会报告，不覆盖修改。保留冲突内容后再处理。旧内容哈希版本会保留，初始化不自动清理，便于回滚；它们占用磁盘空间，清理前需确认没有任何 Agent 发现目录的链接仍在引用。升级 CLI 后重新运行这两条命令；daemon 启动和 `ink register` 不隐式修改用户 skill 目录。

使用自定义发现目录时，初始化与检查必须传入同一位置，例如：

```sh
ink init --skills-dir /absolute/path/to/agent/skills
ink init --check --skills-dir /absolute/path/to/agent/skills
```

`--check` 只读核对当前二进制对应的资源内容和链接；缺失、失效、版本落后或冲突时返回非零。它不能证明 Agent 已加载指令。初始化后启动新的 Agent 会话；已有会话需要显式读取链接内的 `SKILL.md` 再开展协作。Pi 可显式调用 `/skill:ink-commons-workflow`，Codex 可显式使用 `$ink-commons-workflow`。运行时禁用技能或使用其他资源目录时，需按对应运行时配置启用或使用 `--skills-dir`。

安装路径依据：[Codex 用户级技能目录](https://developers.openai.com/api/reference/cli)；Pi 1.4.0 的 `docs/skills.md` 同样支持用户级 `~/.agents/skills/`。安装不保证模型每次自动选择 skill；首次送审应显式使用它，task wake 中也保留领取、交接和结束回合的关键规则。

## 全局注册命令

把新版 `ink` 放入 PATH 后，在任意 GitHub checkout 执行：

```sh
ink register
```

命令读取 `origin`，自动识别 `owner/repo`，通过本地 Unix socket 向 daemon 注册。可用 `--remote upstream` 选择其他 remote，或在任意目录用 `ink register owner/repo` 指定仓库。daemon 使用自身 GitHub 身份验证 PR 读取权限；不会读取调用者的个人 gh 凭据。注册持久化到 SQLite，下一轮轮询生效，不需要修改配置文件或重启。相同设置重复注册是幂等的；设置冲突返回错误，不覆盖已有设置。仓库配置统一保存在同砚 SQLite，通过 `ink config get/set/unset/list` 动态管理；当前不提供注销接口。

为了让 `ink register` 无需重复填写 reviewer，在 daemon 启动后设置中心默认值：

```sh
ink config set reviewer_login 'your-reviewer[bot]' --global
ink config set reviewer_command your-reviewer-cli --global
ink config set reviewer_route '{"provider":"cc-connect","target":"reviewer-project"}' --global
```

使用 bot 路由前，在静态 JSON 中配置对应 provider endpoint 及 daemon 环境凭据；人工 reviewer 可省略路由。

JSON 管理 daemon 静态启动参数，SQLite 管理运行期可变配置和状态。新注册仓库只存显式覆盖，未设置字段动态继承。旧 JSON 的运行期字段仅在首次迁移时导入；完整字段规则、CLI、任务快照和迁移说明见 [中心化配置](docs/configuration.md)。也可以显式指定参数：

```sh
ink register --github-login 'your-reviewer[bot]' --github-command your-reviewer-cli \
  --target reviewer-project owner/repository
```

注册管理的是 GitHub 仓库，不安装 Git hook。自动关联开发会话仍需安装前述 pre-push hook，或通过 `link` 登记。只注册仓库不会把未认领 PR 自动指派给某个开发会话。

## 审查通过后的 postApproval

用 `ink config set postApproval '{"command":["/absolute/path/to/after-approval"],"timeout_seconds":60}' --repo owner/repository` 设置中心 hook，也可用 `ink register --post-approval /absolute/path/to/script` 为新仓库指定脚本：

```json
{
  "postApproval": {
    "command": ["/absolute/path/to/after-approval", "--notify"],
    "timeout_seconds": 60
  }
}
```

`command` 是 argv 数组，首项必须是绝对路径。不经过 shell 展开，也不执行 PR 中声明的命令。脚本由 daemon 用户运行，默认超时 60 秒，上限 3600 秒；标准输出和标准错误不写入状态或 daemon 日志。脚本需自行记录业务结果。

仅当已关联、非 Draft、未关闭 PR 被指定 reviewer 对当前 head SHA 正式 Approve，工作项进入 `approved` 时触发。其他人或旧 SHA 的 review 不触发。脚本从 stdin 接收一行 JSON：

```json
{"event_id":"owner/repo#42:HEAD_SHA","event":"postApproval","repository":"owner/repo","pull_number":42,"pull_url":"https://github.com/owner/repo/pull/42","sha":"HEAD_SHA","review_id":123,"review_url":"https://github.com/owner/repo/pull/42#pullrequestreview-123","reviewer":"your-reviewer[bot]"}
```

以仓库、PR、SHA 去重，同一 SHA 的再次 Approve、重复轮询和重启不会重复触发。新 SHA 审查通过后会产生新事件。首次启用 hook 时，已观察到的有效批准也会触发。`GET /v1/work` 和单条工作项的 `postApproval` 数组记录 `unknown`、`succeeded` 或 `failed`、时间及错误。

触发记录在启动脚本前持久化；失败、超时或 daemon 中断后不自动重放，也没有自动重试接口。这样避免脚本已经产生外部副作用后再次运行，但中断可能导致事件未执行完成，需要人工核查。脚本应使用 `event_id` 去重；如果它要执行 merge 等对当前版本敏感的操作，还需核对 GitHub 最新 head 和批准状态。hook 本身不自动 merge PR。

## 批准与自动合入通知

指定 reviewer 对当前 head SHA 正式 Approve 后，同砚在下一轮轮询中向当前 developer 路由发送 `review_approved` 通知，包含 PR、批准的 SHA、reviewer 和正式 review 链接。cc-connect 通过 webhook 将这些信息送入原开发 Agent 上下文，并要求简短告知用户。通知包含发送时的 `merge` 观测（自动合入开关、方法、状态、原因及可用的合并提交），供 Agent 参考。通知独立于代码修改任务，不授权修改、推送、合并或部署。

按仓库、PR、SHA 生成稳定事件 ID；新 SHA 获得批准后产生新事件。只有当前指定 reviewer 对当前 SHA 的有效正式批准触发；Draft、未关联或已关闭 PR、旧 SHA 和其他作者不触发。人工 developer 不会回退到历史 bot 会话。

cc-connect 的 Webhook `accepted` 仅表示 HTTP 请求被接受，忙碌会话仍可能丢弃通知。因此新通知启用 Agent 收件确认：发送前持久化 `unknown`、尝试次数和下次重试时间；HTTP 成功后记作 `awaiting_ack`，收到 Agent 确认后才记作 `acknowledged`。未确认或网络结果不明时，在后续成功轮询中退避重试（2、4、8…分钟，上限一小时），重启保留进度；不设自动放弃次数。等待确认时，批准通知显示 `approval_notification_unresolved`，合入完成通知显示 `merge_notification_unresolved` 诊断。此机制无需修改 cc-connect；投递至少一次，可能重复进入 Agent 上下文。

通知要求 Agent 在回应用户前运行：

```sh
ink notification ack --event '通知中的事件 ID'
# 仅 first:true 时，回复前再只读查询一次：
ink notification status --event '通知中的事件 ID'
# ink 不在 PATH 时可使用 "$HOME/.local/lib/ink-commons/ink"
```

命令调用本机私有 Unix socket 的 `POST /v1/notifications/ack`（JSON：`{"event_id":"..."}`）。原子持久化收件确认并返回 `{"status":"acknowledged","first":true}`；重复确认返回 `first:false`，Agent 应跳过重复回应。PR head、批准状态或 developer 路由已变化，或 PR 未合并即关闭时，确认返回 `superseded`、`first:false`。重试保留原投递路由，路由变更后停止向旧会话重发。合并前已预留的通知在 PR 合并后仍会继续轮询、重试和接收确认（需 head 保持一致），不让自动合并吞掉待收通知；不会为已合并 PR 新建审批通知。收件确认只证明 Agent 已读到通知，不证明用户可见回复已成功发送；确认后 Agent 崩溃可能遗漏回复。Agent 必须能访问同一个 daemon，并遵循确认指令；未确认时会继续重试并保留诊断。

`status` 调用只读 `GET /v1/notifications/status?event_id=...`，不会确认收件、调用 GitHub、触发合并或修改配置。返回事件 SHA、当前 head SHA、phase、通知状态及当前 `merge` 观测；CLI 会编码事件 ID 中的 `#` 等字符。观测来自当前仓库配置和持久化账本，可能落后于 GitHub；成功的合入账本已足以返回 `merged`，无需等待下一次 PR phase 同步。状态包括 `merged`、`waiting`、`blocked`、`unknown`、`disabled`、`superseded`、`not_approved`、`closed`。Agent 按状态简短报告，查询失败则说明不可读，不把失败解释为尚未合并；不循环等待，也不重复固定的“批准不代表已合并”提示来替代已确认结果。

自动合入成功时，`pr_merged` 事件与合入账本在同一事务预留，之后使用相同的发送、重试、去重和回执机制。发送前退出或 PR 已从 open 列表移除，仍恢复该事件的初次发送/回执；旧合入请求结果不明时，只在 GitHub 确认最终 head 匹配后创建完成事件，其他 head 或未合并关闭不触发。升级不回填已经完成的历史合并，也不为纯手工合并创建新的完成通知。`review_approved` 与 `pr_merged` 各有稳定事件 ID，按 PR、SHA、事件去重；完成事件首次收到回执后，尚未接收的同 SHA 批准通知记作 superseded，避免通知乱序后再补旧提示。合入不表示已部署。

工作项 API 的 `notifications` 保存 `require_ack`、`attempts`、`retry_at` 及状态。人工负责人记录为 `manual`，不支持通知的执行端为 `unsupported`，无效路由为 `failed`。通用执行端需显式实现 `execution.Notifier`；只有额外声明 `NotificationACKProvider` 的执行端启用收件确认和重试，其余保留原有 `accepted`/`unknown` 一次投递语义，不扩展现有 HTTP provider 协议。通知不替换代码任务，也不授权修改、推送、合并或部署。

升级前已记录的通知保留原有语义，不自动重放，以免重复通知已处理的历史审批。此前被 cc-connect 丢弃的旧通知仍需人工补充上下文；新建的 cc-connect 通知自动使用确认协议。

通知与 `postApproval` 脚本相互独立；通知尝试发生在脚本之前，脚本无需配置，也不会因通知已发送而跳过。

升级前备份 SQLite 状态；通知记录保存在现有工作项 JSON 中。旧版程序可能丢弃 `notifications` 字段，导致再次升级后重复通知，回滚前应保留包含通知记录的备份。

## AP 后自动合入（默认关闭）

在单个仓库配置中设置：

```json
"auto_merge": {"enabled": true, "method": "merge"}
```

`method` 可选 `merge`（默认）、`squash` 或 `rebase`。也可用 `ink config set auto_merge '{"enabled":true,"method":"squash"}' --global` 设置动态继承的全局默认值，或通过 `ink register --auto-merge-after-approval --merge-method squash owner/repository` 注册。显式 `"auto_merge": {"enabled": false}` 或注册参数 `--auto-merge-after-approval=false` 覆盖注册默认值；缺省动态继承，迁移来的旧覆盖项需 unset 后才恢复继承。

开启后，同砚仅处理已关联、打开、非 Draft 且由指定 reviewer 正式批准当前 head SHA 的 PR。每次尝试前重新读取 PR、review 和检查结果；需要 GitHub 报告 `mergeable: true`、`mergeable_state: clean`，并要求 head 和可用的 test-merge SHA 上当前已发现的 check runs 全部结束且为 success、neutral 或 skipped。检查未完成、失败、无权限读取、合并冲突或状态未知时保留 `waiting` 和原因，后续轮询重新检查。cooperative 模式下，指定 reviewer 已完成批准交接后，同一 base 和 head 间隔至少 30 秒仍被 GitHub 报告为冲突时，同砚给开发会话派一个可领取的核查任务。开发者本地试合，处理实际冲突并推送后由同砚重新送审。若本地试合成功，用 blocked handoff 记录 GitHub 与本地结果的差异。**需要保证等待特定 CI 时，应在 GitHub 分支规则中将其设为必需检查**；当前尚未出现的非必需检查不能由同砚保证等待。

实际调用 GitHub merge API 时携带批准的 `sha`，避免 head 变化后误合入新提交；不使用管理员绕过。GitHub 分支保护、必需检查、额外 review 和仓库允许的合并方式仍由服务端执行。批准与 merge 之间无法实现跨 API 的原子校验；对必须严格维持到合并时的审批条件，应使用 GitHub 分支规则约束。没有 CI 的仓库可以在 GitHub 报告 clean 后合入。

GitHub 原生 Stack PR 使用异步合入。只有当前目标分支已经是 stack 目标分支的栈底 PR 才能发起请求。GitHub 将下一条推进到栈底后，同砚会按它当时的 head SHA 重新核验批准与检查。合入请求 ID 持久化在 `auto_merges` 的 `request_id` 中，服务重启后继续查询。GitHub 确认合入后才发合入通知。遇到没有可核验请求 ID 的历史 `unknown` 记录，仍按下述对账规则处理。

读取使用配置的 GitHub App `inspect` profile；实际 merge 请求和异步合入结果查询使用同一 CLI 的 `code` profile，并以 JSON stdin 传入 SHA 和方法。App 需要相应仓库的 contents 写权限、PR/checks 读取权限，且 broker 允许 code profile；使用 `INK_GITHUB_TOKEN` 部署时同样需要相应权限。不会回退个人 gh 凭据，不自动扩大 App 权限。

工作项 API 的 `auto_merges` 保存 SHA、reviewer、review ID、方式、状态、merge SHA、更新时间及原因。merge 发出前持久化 `unknown` 预留；同一 PR 的并发轮询和重启不会重复发请求。响应丢失、中断或结果无法判定时保留 unknown 并产生诊断，不自动重放。GitHub 对 Stack 合入返回明确的 HTTP 400 拒绝时，后续轮询会从预检开始；**任何历史 SHA 仍有 unknown 时，都继续告警并阻止该 PR 的新 head 自动合入**，避免后续 push 掩盖未决结果。仍然打开且结果不明的 PR 需人工核查和处理，当前没有自动重放/重置接口。

GitHub 观察到 PR 关闭后会一次对账所有 unknown，即使开关已关闭也可完成只读对账：未合并关闭时记为 failed；已合并且最终 head 匹配该记录时记为 merged；已合并但 head 不同时记为 superseded 并保留原因，不声称旧 SHA 已合入，也不把其他 head 的 merge SHA 归给旧请求。

开启会处理已有的有效批准，不只处理开启后新产生的 review。它与批准通知、`postApproval` 脚本独立：通知和脚本先运行，随后检查自动合入；脚本本身不构成合入前置检查。如脚本已合并或改变 PR，自动流程会重新读取后停止当前合入尝试。关闭开关后不再发起新 merge，已经发出的请求无法撤回。合入不等于部署，此配置不会部署服务。

升级前备份 SQLite。`auto_merges` 保存在工作项 JSON 中，旧版可能丢弃这些记录；回滚前保留含最新合入记录的完整备份。

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

`target` 是 cc-connect project。省略 `parent` 时，从工作项关联的公开开发 thread 或文本频道解析 Discord 父频道，在同一频道下创建审查 thread。名称包含 PR 编号和开发 thread 的标题。单条 PR 指派中的显式 `parent` 可以覆盖创建位置；仓库及 `repository_defaults` 路由中的 `parent` 不再用于新建 cc-connect 审查会话，遗留配置无需清理即可默认跟随开发频道。已有工作项保存的路由和审查会话保持原位；需要迁移时，应先确认旧执行结束，再按 PR 重新指派。自动推导只支持普通文本父频道，不支持论坛、媒体或公告父频道，也不支持公告 thread 和私有 thread，会报明确错误；显式指定文本频道 `parent` 代表选择独立的公开审查位置，仍创建公开 thread。来源无法读取或不适用于自动推导时，显式位置仍可使用，名称回退为 `Review owner/repository #42`，不复制私有来源标题。需要注入能读取开发会话并在目标频道创建、访问公共 thread 的 `INK_DISCORD_TOKEN`。也可以直接传 `session: "discord:<thread-id>"` 复用已有会话。Git hook 识别 cc-connect 注入的 `CC_PROJECT` 和 `CC_SESSION_KEY`。旧版的 `reviewer_project`、`review_channel_id` 和 `project` / `session_key` 路由输入也能读取。查询 API 统一返回新路由；原 `review_thread_id` 可从 `reviewer.route.session` 中取得。

### 开发 thread 复用与原始交流上下文（可选）

在仓库配置中设置 `review`，两个开关默认均关闭，可以分别启用：

```json
{
  "repo": "owner/repository",
  "reviewer_login": "your-reviewer[bot]",
  "reviewer_route": {"provider": "cc-connect", "target": "reviewer-project"},
  "review": {
    "reuse_development_thread": true,
    "context": {"thread_history": true}
  }
}
```

`reuse_development_thread` 让**尚未分配会话**的 reviewer 复用开发 thread 的 Discord ID，同时保留 reviewer 的 project 和 GitHub 身份；自动复用要求 reviewer project 与 developer project 不同，避免共享同一个 Agent 上下文。已有 reviewer 会话保持原位。`ink review --session` 仍选择指定的已有会话；单条 PR 的显式 `--parent` 仍创建独立审查 thread。关闭此开关时保留原来的独立 thread 行为。

`context.thread_history` 允许读取工作项绑定的开发 thread 中的原始用户和 bot 消息，不依赖 developer 的总结，也不共享其他 Agent 的内部思考或会话文件。派审 prompt 会要求 reviewer 先运行：

```sh
ink context --sha HEAD_SHA --before REVIEW_CUTOFF PR_URL
```

`HEAD_SHA` 和 `REVIEW_CUTOFF` 由同砚填入 prompt，后者固定为该 reviewer task 创建时点的 Discord snowflake 边界。CLI 通过私有 Unix socket 调用 `GET /v1/work/{owner}/{repo}/{number}/context`；响应含 `task_id`、`sha`、`review_before` 和按时间正序排列的消息。每页最多 50 条、消息内容合计最多 64 KiB；`truncated: true` 和 `next_before` 表示还有更早消息，可再次用 `--before NEXT_BEFORE` 查询。每次查询都重新核对开关、任务、head SHA 和来源；旧 SHA、已关闭 PR、改派中失效的任务、超过送审边界的游标会被拒绝。当前 head 的正式 review 仍由 GitHub 判定，聊天中的“通过”不替代它。

消息保留作者、时间、正文、编辑时间、常见 embed 文本及附件名称/URL；不会自动下载或理解图片、附件和外部链接。时间边界限制**消息创建时间**，不是不可变聊天快照：历史编辑与删除仍反映 Discord 当前状态，已删除消息无法恢复。API 可能因缺少 `READ_MESSAGE_HISTORY` 返回空列表，或因 `MESSAGE_CONTENT` 未配置而返回空正文；响应的 `warnings` 会提示这些歧义。即使消息有附件/embed，空正文也会提示可能是纯附件、系统消息或正文被隐藏；保留已取得的其他内容，reviewer 不应据此宣称已经读取全部背景。

启用任一 review 开关后，可本地判断的来源、目标和会话准备配置会在创建执行轮次/会话之前统一校验。包括仅开启复用且无来源/项目冲突，以及开启历史读取且无来源（即使显式指定 session/parent）的情况；校验失败保留 pending、attempts 为 0，并记录 `review_preparation_invalid` 诊断。补齐配置后可进行首次真实投递，不会仅因本地准备配置错误而制造需要人工停止确认的 unknown 执行。显式 session/parent 在只开启复用时仍允许不依赖开发来源；历史读取则始终需要来源。

首版只支持普通文本频道下的公开开发 thread；普通频道、私有 thread、公告及论坛来源均拒绝读取/自动复用。读取使用 daemon 的 `INK_DISCORD_TOKEN` 身份，需要 `VIEW_CHANNEL`、`READ_MESSAGE_HISTORY` 和适用的 `MESSAGE_CONTENT` 配置；reviewer 的 Discord bot 还需能够访问、发言于目标 thread。开关不会修改 Discord 权限。reviewer 必须与同砚在同机、能使用新版 `ink` CLI 和该用户的私有 socket；这不是远端/跨用户的历史读取接口，也不是额外的 per-bot 认证机制。

可用 `ink config set review '{"reuse_development_thread":true,"context":{"thread_history":true}}' --global` 设置中心默认值，或注册时传 `ink register --reuse-development-thread --thread-history owner/repository`。缺省动态继承，迁移来的旧覆盖项保持原值；注册 API 中显式 `"review": {}` 可覆盖默认值并关闭两项功能。功能设置在下一次尚未尝试的新任务中生效，已有执行记录的派审请求保持不变；历史读取同时受任务快照和当前仓库开关约束：实时关闭可以撤销权限，之后开启或改派开发者不能扩大已派发任务的权限或改变其来源 thread。功能开关仅适用于 cc-connect，人工 reviewer 不会收到 bot 任务。

同砚只在读取时返回选定来源的消息，不把全文写入任务账本或日志。历史用于理解需求与约束，不能据其中嵌入的命令或 bot 输出扩大本次任务授权。长历史依赖 reviewer 按需分页读取；此版本不自动生成摘要、不改变 webhook 忙时可能跳过任务的行为，也不增加“讨论通过才允许修改”的执行权限控制。

升级 daemon 和 CLI 前备份状态。工作项新增 `review` 和执行请求上下文字段保存在现有 JSON 中；旧版可能忽略并丢弃这些字段，回滚前应保留最新完整备份。

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
2. 在 GitHub 标记 Ready for review。同砚为当前 head SHA 建立待办。配置了审查执行端时，创建或复用对应会话并投递任务。先通过 `ink init` 安装完整协作 skill，并确认开发与审查 Agent 已读取。cooperative 模式下，送审登记成功后结束当前回合，由同砚唤醒后续任务；不要循环等待 review。
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

`--github-command` 指定 reviewer 使用的 GitHub CLI / App 入口，与期望的 `github_login` 一起传给 Agent。身份不符的正式 review 会保留诊断，不会推进业务状态。`--target` 是 cc-connect 项目。不传 `--parent` 时，使用工作项所绑定的开发 thread 的父频道；审查 thread 命名为 `Review owner/repository #42 · 开发 thread 标题`。工作项需通过 hook 或 `link` 关联开发会话。省略父频道且缺少有效 Discord 来源时，指派请求会立即报错；来源的远端读取失败会在下一次投递时记录到 `task.error`，不会任意选择其他频道。`--parent` 可显式覆盖创建位置。`--github-login` 是正式提交 review 的 GitHub 身份。用 `--session discord:THREAD_ID` 可以复用已有会话，只传 `--github-login` 则交给真人。开发 agent 可以用同一条命令代办。

设好 reviewer 后，在 GitHub 标记 Ready for review。同砚在下一轮轮询中创建审查会话并派发任务。PR 已经是 Ready 时，指派会直接进入待派发。重复提交相同指派会保留已有会话和投递状态。修改后推送新 commit，同砚会沿用该 reviewer 复审。

## 主动领取与业务交接（可选）

仓库配置 `"task_mode": "cooperative"` 后，新任务由 ink 管理领取、幂等交接和 GitHub 结果核验。cc-connect 只发送可重试的领取唤醒，无需修改 cc-connect 或切换 Codex 后端。Agent 先 `ink task claim`，完成后 `ink task submit --handoff`；后继任务和自动合入等待已验证的交接。旧执行 `unknown` 不自动清除。启用、凭据、异常恢复和信任边界见 [主动领取与交接](docs/handoff.md)。

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
| `GET /healthz` | 查看运行 revision、gate、last_poll 和 last_github_ok。 |
| `GET /v1/repositories` | 列出配置及动态注册的仓库。 |
| `POST /v1/repositories` | 注册仓库，接受 Repository JSON，仅存显式覆盖，省略字段动态继承 SQLite 全局配置。 |
| `GET /v1/self-check` | 由 daemon 用自身环境验证所有配置仓库的 PR 读取权限，不派发任务。 |
| `GET / POST / DELETE /v1/config` | 查询 / 设置 / 删除中心配置覆盖；支持 effective 和逐键来源，详见配置文档。 |
| `GET /v1/work` | 列出已观察的 PR 和当前待办。 |
| `GET /v1/work/{owner}/{repo}/{number}` | 查看一条 PR 的来源、负责人、审查版本和投递状态。 |
| `GET /v1/work/{owner}/{repo}/{number}/context?sha=HEAD_SHA&before=MESSAGE_ID` | 开启历史开关后，分页读取当前 reviewer task 对应开发 thread 在送审边界之前的交流；省略 before 使用该任务边界。 |
| `POST /v1/work/{owner}/{repo}/{number}/link` | 手动关联未认领 PR。请求体提供 `github_login` 或 `route`。路由包含 `provider`、`target`、`session`。 |
| `POST /v1/work/{owner}/{repo}/{number}/assign` | 为 `developer` 或 `reviewer` 更新负责人。传 `github_login` 即可转给人。传 `route` 可指定已有 bot 会话。指定 reviewer 时也可以省略 `session`，让执行端按 `provider`、`target` 和适用的 `parent` 创建会话。 |
| `POST /v1/work/{owner}/{repo}/{number}/retry` | 在下次轮询重试当前 bot 待办。所有旧轮次已确认结束才允许重试；保留 task_id，生成新的 attempt_id。 |
| `POST /v1/work/{owner}/{repo}/{number}/confirm-stopped` | 人工确认指定轮次已停止且延迟投递已被禁止。必须提供 task_id、attempt_id、stopped: true 和 reason，保留确认记录。 |
| `POST /v1/pushes` | Git hook 提交候选关联记录。 |

`assign` 修改未来任务的投递。新 SHA、改派、Draft 和 PR 关闭都会保留旧轮次记录；旧执行未确认结束时，不会投递后续 bot 任务。接手之前如果 bot 正在工作，先在原执行端停止它并禁止延迟投递，再接管目录或处理未提交改动。`accepted` 表示执行端接收请求，正式 GitHub review 或新的 PR 版本才表示工作状态推进。cc-connect 忙时可能跳过任务，待办仍可在 API 中查看、重试或转给人。

开发计划与完成记录统一维护在 [TODO.md](TODO.md)。

## 执行可靠性

工作项的 `phase` 继续由 GitHub review 和 head SHA 决定；`task.runs` 单独记录投递、开始 ACK、心跳、结束、停止请求和重试计划。`execution_history` 保存已被业务状态替换的任务。执行成功本身不会把 PR 标成审查通过。

HTTP 执行端可以通过 `http_provider_execution: true` 显式启用 `status` / `stop` 协议。默认关闭，旧 HTTP 服务不会收到额外操作。执行端必须持久化去重和运行归属，并保证终态表示 Agent 及所有工具已停止、延迟投递不能重新启动旧轮次。仓库提供 SQLite 执行端账本及恢复测试，接入约定见 [执行协议](docs/execution.md)。

通过 `ink config set execution.<字段> <值> --global` 或 `--repo owner/repository` 可设置 `ack_seconds`（默认 120）、`heartbeat_seconds`（180）、`execution_seconds`（3600）、`backoff_seconds`（30）、`max_attempts`（3，含首次投递）。运行期数值必须为正，unset 恢复继承；`max_attempts: 1` 关闭自动重派。超时先查询、再请求停止；只有终止确认后才按指数退避重试，退避上限一小时。耗尽次数或无法确认停止时需要人工处理。每轮 daemon 轮询先恢复执行状态，即使 GitHub 暂时不可用也会核查执行端。

cc-connect 当前 webhook 不支持执行级状态查询、停止或持久化去重，仍按保守模式处理：`accepted` 仅表示接收，失联后保留 `unknown`，不自动重派。历史版本投递的任务也需要人工确认。人工停止并禁止旧请求重新启动后，调用 `confirm-stopped`，再用 `retry` 重试；若未耗尽自动重试次数，当前任务的停止确认也会触发退避重派。人工确认只是可信本地操作者的声明，daemon 不会据此停止外部进程。

## 状态、备份与回滚

daemon 将状态写入状态目录中的 `state.sqlite3`，使用 schema version 4（兼容迁移 version 1 / 2 / 3，并新增中心配置表）。PR 和 push 候选分别存入表，PR 记录保留完整的负责人、路由、审查历史及当前投递状态。每次读取和更新使用 SQLite 事务，进程内串行修改；仍使用 `state.json.lock` 排除其他 daemon，包括旧版进程。数据库和备份文件权限为 `0600`。

状态读写与执行账本的日常查询由 sqlc 生成类型化 Go 方法。状态保存仍使用完整快照接口，在同一事务中比较已有记录，仅插入新增记录、更新 payload 有变化的记录，并按主键删除已移除记录；未变化的记录不写入。push 保留原有 position 顺序，删除或重排会更新受影响的位置。读取和比较仍为全量，仍依赖进程内串行修改与 daemon 文件锁，不提供多 writer 并发合并。任务、交接和配置快照保存在 JSON payload 内；version 4 的 runtime_config 表通过专用查询更新，不参与工作流快照保存。

修改查询时编辑 `internal/ink/sql/` 或 `internal/execution/ledger/sql/`，运行 `./scripts/generate-sqlc.sh`（固定 sqlc v1.30.0），提交生成的 Go 文件，并运行 `go test -race ./...` 和 `go vet ./...`。`schema.sql` 仅供代码生成使用；运行时建表和版本迁移仍由存储层显式管理，变更表结构时须同步更新两处。普通构建不需要 sqlc。

首次启动时，若存在 `state.json`，daemon 校验数据并生成 `state.json.pre-sqlite.bak`，然后在单个事务中导入并记录 schema 版本。原 JSON 不会被修改。导入失败时不会留下部分记录，修复 JSON 后可以重新启动；备份已存在但内容不同时会拒绝导入，需先另行保存两份文件再处理。数据库初始化完成后只读取 SQLite，不会再次导入遗留 JSON。初始化成功后、接单前持久化 `state.sqlite3.initialized` 标记。已有该标记而数据库缺失、为空或变成无版本空库时，启动会拒绝导入陈旧 JSON；迁移备份存在但数据库缺失时同样拒绝。应恢复 SQLite 备份；若确实要从 JSON 重新迁移，须先归档初始化标记和旧迁移备份。未提交完成的首次迁移（无标记且空库仍在）可正常重试。已知旧格式的 `project` / `session_key` 路由和 `review_thread_id` 会映射到新路由；重复字段冲突时拒绝迁移。其他未知 JSON 字段也会拒绝导入：保留原文件和备份，先确认写入它的版本并用兼容版本转换，或核实字段用途后人工修正，避免直接删除可能承载路由的字段。遇到未知 schema 版本或损坏数据时启动失败，不会重置数据库。

以下操作均先停止 daemon，`-data` 可省略以使用默认状态目录。输出文件必须尚不存在。备份通过 SQLite `VACUUM INTO` 生成独立快照；目标文件系统不支持硬链接时，使用独占创建、复制并同步的方式，仍不覆盖已有文件。这种回退在进程异常中断时可能留下不完整的输出文件，恢复前应验证或重新生成；目录同步不受文件系统支持时只能尽力保证目录项持久化。导出包含迁移之后的最新记录，保持旧版 JSON 结构。

```sh
ink state backup -data /path/to/ink-state -output /safe/path/ink-backup.sqlite3
ink state export -data /path/to/ink-state -output /safe/path/ink-rollback.json
```

恢复 SQLite 备份：停止 daemon，将当前 `state.sqlite3` 移到另一个位置留存，再将备份复制为状态目录中的 `state.sqlite3`，确认权限为 `0600`，然后启动 daemon。不要在运行期间直接复制数据库文件。

回滚到旧版程序：旧版可能不支持 schema version 3、新增的任务交接/注册数据及 `postApproval` 字段，不能直接读取新数据库或 JSON；需先在兼容版本中显式转换并核查新增数据。以下旧版 JSON 恢复流程仅适用于已完成转换的快照。停止 daemon，先执行上面的 backup 和 export；将当前数据库、`state.sqlite3.initialized` 标记、旧 `state.json` 及 `state.json.pre-sqlite.bak` 一起移到独立归档目录，再将最新导出的 JSON 复制为状态目录中的 `state.json`（权限 `0600`），最后使用旧版程序启动。以后重新升级时，保持状态目录中没有旧 SQLite 数据库、初始化标记及旧迁移备份，让新版重新导入旧版运行期间更新过的 JSON。直接使用迁移前的备份只会恢复迁移时刻的数据。

## 审查等待与回流诊断

`GET /v1/work` 和单条工作项 API 返回持久化的 `workflow` 与 `alerts`。Agent 结束或投递返回 `accepted` 后，工作项仍等待指定 reviewer 对当前 SHA 的正式 review，或等待开发推送新的 SHA；不会因此标记业务完成。`waiting_start` 仅表示投递已接收、尚未确认开始，`executing` 需要已验证的运行回执。

`alerts` 记录身份不符、新提交的旧 SHA review、旧执行结果不明、缺少正式结论或新 SHA、重试耗尽等原因。每个诊断有稳定 ID，重复轮询和重启不会重复创建；条件解除后保留历史并标记为非活动。首次出现也写入 daemon 日志。合法的 Request changes 与新 SHA 继续使用现有派发与复审逻辑，reviewer / SHA 过滤保持不变。

配置仓库的 `reviewer_command`，或用 `ink review --github-command`，可把正确的 App 命令一并交给 reviewer。任务还会带上当前活动诊断，便于人工重试时了解上次未推进的原因。`execution.result_seconds`（默认 120）仅控制已确认结束但缺少业务结果时的诊断等待时间；不会自动重派成功的执行。

本功能兼容现有 cc-connect webhook，不需要 cc-connect 配套改动，也不调用新的执行或通知接口。诊断通过 API 和日志查看；可靠队列、Discord 自动诊断通知及缺少结论时自动唤醒 reviewer 留待后续接入。现有 webhook 的忙时丢弃与执行无法确认限制仍按前述保守方式处理。

## macOS 服务与版本部署

安装、更新和从登录启动迁移到开机启动，见 [macOS 部署与恢复](deploy/macos/README.md)。Apple Silicon 部署可使用 GitHub Actions 为 `main` 预编译的产物，也可按指定提交本机编译。通过 `/healthz` 返回的 `revision` 核对实际运行版本，使用 App CLI 按仓库刷新短期凭据。系统迁移同时处理 cc-connect 登录依赖，不需要 cc-connect 新协议。FileVault 冷启动仍需先解锁磁盘。
