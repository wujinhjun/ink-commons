# Ink Commons TODO

## 待办

- [ ] 配置化。
- [ ] 在 Discord 开发 thread 提供原生 `/review` 命令，校验改派权限，并在同一个 thread 对应多条 PR 时让操作者选择 PR。命令复用同砚指派 API。
- [ ] 从同砚工作项启动 Discord 开发 thread 时，按需分配独立目录、Git worktree 和分支。启动请求携带仓库和 base，分配成功后登记 thread 与源分支的绑定。现有工作项继续支持关联已有 PR 或分支。
- [ ] 通过受管执行入口交接本地未提交改动，确认 agent 已停下后再移交工作目录。
- [ ] 接入 GitHub Webhook。

### 执行可靠性

已实现任务 / 轮次记录、执行 ACK 与心跳核查、安全停止后退避重试、重启恢复、人工停止确认，以及可供 HTTP 执行端接入的 SQLite 去重账本。见 [执行协议与接入边界](docs/execution.md)。

- [ ] cc-connect 原生接入执行协议：持久化 task_id / attempt_id、Agent 开始与心跳回执、执行查询及停止；终态必须确认所有工具停止且延迟投递不能恢复旧执行。当前 webhook 保持保守模式，不自动重派。
- [ ] cc-connect 会话创建接入持久化预留与结果核查；无法确认创建结果时由人工关联已有 thread，避免重复创建。

## 已完成

- [x] 持久化审查等待状态与回流诊断，传递 reviewer 的 GitHub 身份 / 命令；身份或 SHA 不符、执行不明及缺少业务结果通过 API / 日志可见。不依赖 cc-connect 新协议，不自动重派成功的执行。见 [PR #6](https://github.com/wujinhjun/ink-commons/pull/6)。

- [x] 将 daemon 状态从 `state.json` 迁移到 SQLite，保留现有 PR、会话绑定和投递记录，并提供备份与回滚方式。见 [PR #3](https://github.com/wujinhjun/ink-commons/pull/3) 和 [状态维护说明](README.md#状态备份与回滚)。
- [x] 审查 thread 与开发 thread 位于同一父频道，名称关联开发 thread 标题和 PR 编号，并支持显式指定审查位置。见 [PR #4](https://github.com/wujinhjun/ink-commons/pull/4)。
