# Ink 主动领取与交接

本模式让 ink 持久化任务、控制领取和业务交接。cc-connect 继续承载会话和唤醒通知；不要求修改 cc-connect，不要求切换 Codex app-server，也不依赖 Pi/Codex 原生队列。默认维持原有执行模式。

## 启用范围

用 `ink config set task_mode cooperative --repo owner/repository` 设置中心仓库策略，或注册时使用：

```sh
ink register --task-mode cooperative --github-login REVIEWER --target REVIEW_PROJECT owner/repository
```

也可用 `ink config set task_mode cooperative --global` 设置动态继承的全局默认值。显式 `legacy` 覆盖全局值，unset 恢复继承。运行期改配无需重启；旧 JSON 仅用于首次迁移，详见 [中心化配置](configuration.md)。

开关影响尚未首次派发的新任务；已派发任务的模式及负责人快照不被改写。旧任务和历史 `unknown` 仍按原执行确认规则处理，不能通过打开开关或普通 `retry` 绕过。首次启用应在工作空闲、旧任务已完成核验/恢复后进行。关闭开关也不会撤销已经领取的 cooperative 任务，仍需交接。旧交接完成后，新一轮 legacy 审批恢复 legacy 行为，不会被已完成的 cooperative 历史永久阻塞。

当前只有 cc-connect 实现 `execution.TaskWaker`。其他 provider 不会退回直接执行，会在 API 中显示不支持唤醒；也可通过本机 CLI 人工领取。Agent 必须能访问同一个私有 Unix socket。

## 三种信息分别保存

- `phase` 继续表示 GitHub 事实：`review_pending`、`changes_requested`、`approved`、`merged` 等。
- `task.handoff.state` 管领取与交接：`pending -> claimed -> submitted -> completed`；核验期间保持 `submitted`。另有主动 `blocked` 和已过期 `superseded`。
- `workflow` 展示 `waiting_claim`、`waiting_previous_handoff`、`waiting_handoff`、`verifying_handoff` 及告警。不能只看 PR 是否仍然 `changes_requested` 就重做代码任务。

任务 UUID 是该次领取的 generation。主动 blocked 后显式 retry 生成新 UUID；旧 credential 不能完成新任务。此模式不创建执行 `runs`，也不把业务交接伪装成 executor 的 `succeeded/stopped` 回执。

## Agent 初始化与回合边界

在 Agent 所属用户下执行 `ink init` 和 `ink init --check`，安装完整协作 skill；具体位置、自定义目录及冲突处理见 [初始化说明](../README.md#初始化-agent-协作指令)。升级 CLI 后重新初始化。安装和链接不会刷新活跃会话，首次送审应明确要求 Agent 读取 `ink-commons-workflow`。

首次送审尚未领取 task：确认 PR 已关联、reviewer 已配置并标记 Ready 后，报告送审状态并结束回合，不伪造 task handoff。收到后续 wake 再领取；不能在原回合通过循环查询自行返工。任务交接后也应报告并结束回合；结果核验和下一轮唤醒由同砚负责，不用 sleep、后台轮询或马上领取后继任务来维持长回合。单次状态查询用于诊断，不作为等待循环。

## Agent 操作

每个 task wake 只表示“有任务”，不授权修改或提交正式 review。先领取：

```sh
ink task claim --task TASK_UUID
```

wake 会给出带路由的完整命令，例如 `--provider cc-connect --target PROJECT --session discord:THREAD`。ACP 等未注入 `CC_PROJECT` / `CC_SESSION_KEY` 的运行时直接使用这些参数；显式路由必须三项齐全，claim 成功后保存到私有 credential，后续 submit 无需重复。旧格式 wake 或兼容环境仍可使用进程环境中的 `CC_PROJECT` / `CC_SESSION_KEY`，通用路由可通过 `INK_PROVIDER` / `INK_TARGET` / `INK_SESSION` 提供。没有执行路由的人类任务使用 `--github-login ASSIGNED_LOGIN`。

CLI 在发送请求前，先生成私有随机 credential，保存到状态目录 `claims/TASK_UUID.json`（文件 `0600`，目录 `0700`）；daemon 只保存其哈希。可用 `--claim-file /private/path/credential.json` 指定另一个私有文件。不要把这些文件提交到 Git、打印或放入唤醒消息；更换状态目录时应保留仍有未决任务的 credential。

响应包含任务、`delivery`、`first` 和 `eligible`。cooperative 任务的 GitHub 登录名/命令始终来自领取任务时保存的负责人快照，改派不会改写旧领取者的指令。**只有领取成功且 `first:true` 才开始执行。** `first:false` 表示这个 credential 已领取过，或任务已交接：不要因重复唤醒再次执行。其他 credential 争抢返回 409。首次领取响应丢失时，相同文件重试会返回 `first:false`；先核对当前 owner/工作记录，由原领取者恢复，不能将其视作一份新授权。

领取后读取 `delivery` 中的精确 SHA、角色、review URL、指定 GitHub 身份和命令。reviewer 若启用了 thread history，还需使用返回的 SHA / context cutoff 调用 `ink context` 读取原开发会话。

开发者修改、检查、推送之后，等待本任务所有工具与写操作收尾，再提交交接：

```sh
ink task submit --task TASK_UUID --handoff --request-id dev-result-1 --new-sha PUSHED_SHA
```

审查者对领取的精确 SHA 提交指定身份的 GitHub 正式 review，收尾后：

```sh
ink task submit --task TASK_UUID --handoff --request-id review-result-1 \
  --review-id REVIEW_ID --outcome CHANGES_REQUESTED
# 通过时使用 --outcome APPROVED
```

`--handoff` 明确表示本任务工作已结束、交回权限。提交后不得继续该任务的工具、文件/GitHub 写操作或延迟工作；需要新增工作时重新领取适用的新任务。

`request-id` 在一次提交的所有重试中保持一致，证据也必须相同。提交响应丢失可以原样重试，重复调用返回原任务的已记录结果；不同证据或不同请求 ID 会得到 409。即使 PR 已进入下一轮，旧提交也不会创建第二个后继任务或更改新任务。

```sh
ink task status --task TASK_UUID
```

ink 在后续轮询中核验 PR / review。GitHub 暂不可用、head 尚未显示推送的 SHA、review 尚未显示正确作者/结论时，保留 `submitted` 并重试核验。Agent 不必 sleep 等待，也不能因此重做修改。状态还会返回当前的 `next_task_id`（如已生成）。

## GitHub 事实先到与过期结果

GitHub 先显示 Request changes 时，ink 可以先建 developer 待办，但旧 reviewer 领取未交接时不能领取它。GitHub 先显示新 SHA 时，同样可建 reviewer 待办，但要等 developer 交接。完成记录、后继任务及其中的待唤醒记录在同一 SQLite 事务保存。

reviewer 交接会核验 review ID、作者、精确 SHA 和结论；developer 交接核验当前 head 是否等于新的不同 SHA。核验发生在数据库锁之外；期间的观察/改派有版本检查，冲突时下轮重核验。

PR 关闭或进入 Draft 后，不再发放新任务，旧领取也不会被静默释放。领取者仍可交回旧任务，结果记为 `superseded`，不声称完成新一轮。旧 SHA 已被其他 head 替代时，也仅记录旧任务的交接；迟到回执不能完成新任务。

cooperative 审查的自动合入和 `postApproval` 脚本还需等待对应批准 review 的已验证交接。人类直接在 GitHub 批准、而没有对应 cooperative reviewer 交接时，不自动合入。审批通知仍独立报告 GitHub 批准事实，不代表已经合并。

## 阻塞和重试

不能继续时，先收尾并主动交回：

```sh
ink task submit --task TASK_UUID --handoff --request-id blocked-1 \
  --outcome blocked --reason '需要补充验收条件'
```

任务进入 `blocked`，保持调度暂停并显示告警。问题解决、确认该交接声明有效后，原领取者/本机操作者使用原 credential 显式恢复：

```sh
ink task retry --task TASK_UUID
```

若仍为当前任务，这会把旧任务归档为 superseded 并生成全新待办；若已有后继任务，则解除该条主动阻塞。未收到交接的 claimed/submitted 不能通过此入口重新执行。

- 未领取：唤醒可重发。首次立即发送；仍未领取时，后续间隔依次为 1、1、1、2、3、4…分钟，上限 60 分钟。次数和下次重试时间按任务持久化，重启不重置，新任务重新计数；领取成功后停止唤醒。该间隔独立于执行失败重试的 `execution.backoff_seconds`；无需依赖 webhook 的 accepted 证明接单。
- 已领取：不再重复唤醒代码任务。超过 `execution.execution_seconds`（默认一小时）显示 overdue，但不自动释放、不自动重做。
- 已提交：重试证据核验；不是重新执行开发。
- 会话创建结果不明：持久化 `preparing` 并停止自动创建，避免重启后复制审查 thread。核查现存 thread 后可通过现有 `assign` 指定确定的会话，使新待办使用该会话；不要重试模糊的创建请求。

## 信任与实际保证

与现有 `assign` / `confirm-stopped` 相同，私有 Unix socket 和服务用户权限是信任边界。路由/登录名是匹配断言，不是对同一 UID 下恶意进程的独立身份认证。credential 限制无意串用、重复执行和迟到请求；不应把 daemon API 暴露到网络。

这是合作式业务调度，不是进程强制终止协议。领取者须等待自己的工具结束并停止后续写操作；UUID/credential 不能撤销一个已在运行的 shell/git 命令。不同 PR 共享工作目录的互斥仍需外部串行或独立 worktree，本 PR 不实现跨 PR 目录锁。需要强制终止保证的执行端继续使用原 Monitor 协议。

## 升级与回滚

升级前用 `ink state backup` / `ink state export` 备份。状态数据库使用 schema version 4，保护任务领取/交接记录和中心配置；旧二进制拒绝打开该版本。任务及 outbox 仍在 work payload 内事务保存，运行期配置存入独立 runtime_config 表。

不要把数据库 user_version 手工改回 2。若需降级，先交回/核查所有未决 cooperative 和旧执行任务，并保留最新完整备份；显式转换和核对状态之后才可使用旧版。恢复升级前快照会丢失之后发生的任务交接。

## 本地 API

- `GET /v1/tasks/{task_id}`：任务、交接状态、领取资格及当前后继任务。
- `POST /v1/tasks/{task_id}/claim`：`{token, route? , github_login?}`。
- `POST /v1/tasks/{task_id}/submit`：`{token, submission:{request_id,handoff:true,new_sha? ,review_id? ,outcome? ,reason?}}`。
- `POST /v1/tasks/{task_id}/retry`：`{token}`，仅用于明确 blocked 并交回的任务。

`token` 是调用方生成的 32-256 字节私有随机字符串；CLI 使用 32 字节随机数的十六进制表示。GET 不返回 token，数据库/API 只包含 claim 哈希。全局旧 `work/.../retry` 不用于 cooperative 任务。

## 阻塞通知

可为仓库启用同砚的阻塞通知。空对象将通知发到任务领取时记录的 Discord 开发会话。之后改派开发者不会改变这个默认目标。也可以指定 Discord 频道或线程 ID。

~~~sh
ink config set task_blocked '{}' --repo owner/repository
ink config set task_blocked '{"discord_channel_id":"123456789012345678"}' --repo owner/repository
ink config set task_blocked null --repo owner/repository
~~~

守护进程通过已有的 INK_DISCORD_TOKEN 直接发消息，Bot 需要在目标会话拥有发送权限。消息包含 PR、提交 SHA、任务 ID、阻塞原因和手动重试命令。通知不唤醒 Agent，也不重派任务。

同砚在首次接受 blocked 交接时持久化通知和目标。重复提交不会创建新通知，启用配置也不会回填历史阻塞。投递失败由 daemon 在后续轮询退避重试，任务状态中的 blocked_notice 显示尝试次数、下次重试时间或发送结果。Discord 通过任务级固定 nonce 在其去重窗口内合并请求结果不明时的重复发送。如果响应丢失且重试超出 Discord 的去重窗口，仍可能收到重复消息。
