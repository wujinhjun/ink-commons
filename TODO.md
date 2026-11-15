# Ink Commons TODO

## 待办

- [ ] 配置化。
- [ ] 在 Discord 开发 thread 提供原生 `/review` 命令，校验改派权限，并在同一个 thread 对应多条 PR 时让操作者选择 PR。命令复用同砚指派 API。
- [ ] 从同砚工作项启动 Discord 开发 thread 时，按需分配独立目录、Git worktree 和分支。启动请求携带仓库和 base，分配成功后登记 thread 与源分支的绑定。现有工作项继续支持关联已有 PR 或分支。
- [ ] 通过受管执行入口交接本地未提交改动，确认 agent 已停下后再移交工作目录。
- [ ] 接入 GitHub Webhook。

### 执行可靠性

- [ ] 执行确认（ACK）：`accepted` 只表示执行端接收请求；Agent 真正开始执行后回传带 `task_id`、`attempt_id` 的 ACK，并持久化开始、心跳和结束状态。校验回执来源与执行轮次，拒绝旧轮次或重复回执；执行状态与 GitHub review / 新 SHA 驱动的业务状态分开记录。
- [ ] 超时恢复：明确 ACK、心跳和执行超时的配置与恢复策略。Agent 崩溃或 Session 失联后，先核查旧执行并确认其已停止或失效，再重派；无法确认时保留 `unknown` 并允许人工接管，避免超时后盲目重派导致并发执行。重试次数、退避、耗尽后的人工处理和 daemon 重启后的恢复都需可追踪。
- [ ] 幂等保障：为逻辑任务分配稳定的 `task_id`，重派使用新的 `attempt_id`。按仓库、PR、SHA、角色及适用的 `review_id` 区分任务，并明确显式改派如何产生新任务版本；同一逻辑任务重试沿用 `task_id`。执行端持久化去重与运行归属，覆盖重复投递、ACK 丢失和进程重启，阻止旧轮次继续写入；超时恢复与幂等机制一起验证，不能只依赖 daemon 内存去重。

## 已完成

- [x] 将 daemon 状态从 `state.json` 迁移到 SQLite，保留现有 PR、会话绑定和投递记录，并提供备份与回滚方式。见 [PR #3](https://github.com/wujinhjun/ink-commons/pull/3) 和 [状态维护说明](README.md#状态备份与回滚)。
- [x] 审查 thread 与开发 thread 位于同一父频道，名称关联开发 thread 标题和 PR 编号，并支持显式指定审查位置。见 [PR #4](https://github.com/wujinhjun/ink-commons/pull/4)。
