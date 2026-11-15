# Ink Commons TODO

## 待办

- [ ] 配置化。
- [ ] 在 Discord 开发 thread 提供原生 `/review` 命令，校验改派权限，并在同一个 thread 对应多条 PR 时让操作者选择 PR。命令复用同砚指派 API。
- [ ] 从同砚工作项启动 Discord 开发 thread 时，按需分配独立目录、Git worktree 和分支。启动请求携带仓库和 base，分配成功后登记 thread 与源分支的绑定。现有工作项继续支持关联已有 PR 或分支。
- [ ] 通过受管执行入口交接本地未提交改动，确认 agent 已停下后再移交工作目录。
- [ ] 接入 GitHub Webhook。

### 执行可靠性

已实现任务 / 轮次记录、执行 ACK 与心跳核查、安全停止后退避重试、重启恢复、人工停止确认，以及可供 HTTP 执行端接入的 SQLite 去重账本。见 [执行协议与接入边界](docs/execution.md)。

- [x] cc-connect 受管入口配套接入：持久化接单、忙时排队、开始 ACK、心跳、按轮次查询与停止。终态等待运行环境确认退出；重启遗留运行保持不明并要求人工核查。Codex / Pi RPC 适配由 cc-connect 配套 PR 提供，需同时升级并显式启用。
- [x] 审查 thread 创建前持久化预留；结果不明时禁止再次创建，通过 confirm-prepared 人工关联已有 thread。
- [x] 持久化审查 / 修复等待状态，传递 reviewer 身份及命令；身份、SHA 不符及缺少结果时告警，安全跟进 reviewer，合法修改请求唤醒开发，新 SHA 自动复审。

## 已完成

- [x] 将 daemon 状态从 `state.json` 迁移到 SQLite，保留现有 PR、会话绑定和投递记录，并提供备份与回滚方式。见 [PR #3](https://github.com/wujinhjun/ink-commons/pull/3) 和 [状态维护说明](README.md#状态备份与回滚)。
- [x] 审查 thread 与开发 thread 位于同一父频道，名称关联开发 thread 标题和 PR 编号，并支持显式指定审查位置。见 [PR #4](https://github.com/wujinhjun/ink-commons/pull/4)。
