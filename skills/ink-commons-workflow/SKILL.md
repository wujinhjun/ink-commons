---
name: ink-commons-workflow
description: Use Ink Commons (同砚 / ink common) to send a GitHub PR for review, handle review or rework tasks, and hand control back to the scheduler.
---

# 同砚开发与审查协作

同砚负责持久化等待、唤醒和结果核验。Agent 每回合完成一个明确动作后交回控制。适用于送审、review 和返工；不扩大用户对开发、发布或合并的授权。下述 claim / submit 流程用于 cooperative 任务；legacy 投递没有 cooperative task ID 时，按原任务完成正式 review 或开发结果并结束回合，不伪造 task ID，不自行切换仓库模式或宣称旧执行已停止。

## 首次送审（尚未领取 task）

1. 检查同砚是否运行、仓库是否注册、PR 是否关联当前开发会话，以及指定 reviewer 的身份和路由。优先使用现有配置；缺少必要配置时报告具体缺项，不猜测身份。`ink register` 只登记仓库，不安装 Git hook。来源可由 pre-push hook 记录，或通过本地工作项 API 的 link 补登记。
2. 完成用户授权的开发、检查与推送，创建 PR，按配置使用 `ink review --github-login LOGIN --github-command COMMAND --target PROJECT PR_URL` 指定 reviewer，并在准备好后标记 Ready for review。可用本地 socket 的 `GET /v1/work/{owner}/{repo}/{number}` 做一次状态查询，确认已关联且等待审查；accepted 不代表 Agent 已接单。
3. 报告 PR、当前 SHA 和送审状态，**结束当前回合**。首次送审没有领取凭据，不伪造 task submit。不用 sleep、watch、循环查询或长时间工具等待 review / CI，也不在原回合自行读取 review 后继续返工。同砚负责后续唤醒；一次查询暂未同步时报告待同步并交回控制。

`ink` 不在 PATH 时使用已有安装位置的可执行文件。不要为等待结果另起后台轮询进程。

## 收到 task wake（reviewer 或 developer）

唤醒只是通知。先执行 `ink task claim --task TASK_ID`，沿用当前进程的 `CC_PROJECT` / `CC_SESSION_KEY`（其他执行端使用 `INK_PROVIDER` / `INK_TARGET` / `INK_SESSION`），不冒充其他会话。

- 只有领取成功且 `first:true` 才开始本次工作。`first:false` 或冲突时不要再次执行，必要时查询一次状态并结束回合。若首次领取响应丢失，先核对已有工作记录，不把重试当新授权。
- 从返回的 delivery 读取角色、精确 SHA、review URL、GitHub 身份/命令和 context cutoff。使用任务指定身份，不自行改用个人凭据。领取文件是私有凭据，不打印或提交。
- Reviewer：按需用 `ink context --sha SHA --before CUTOFF PR_URL` 读取已启用的背景；上下文关闭时不绕过。审查精确 SHA，按需要在独立 worktree 验证，使用指定身份提交绑定 `commit_id` 的正式 APPROVED 或 CHANGES_REQUESTED review；聊天结论不能代替正式 review。
- Developer：处理指定 review，完成检查并推送新 SHA。尚未成功领取时，即使 GitHub 显示 changes_requested，也不自行开始这轮返工。

## 收尾与交接

等待**本任务自己的**工具和写操作收尾后交接，不留后台写入：

```sh
# Developer
ink task submit --task TASK_ID --handoff --request-id STABLE_ID --new-sha PUSHED_SHA
# Reviewer
ink task submit --task TASK_ID --handoff --request-id STABLE_ID --review-id REVIEW_ID --outcome APPROVED
# 有可操作的修改意见时改为 --outcome CHANGES_REQUESTED
# 无法继续时收尾并主动报告阻塞
ink task submit --task TASK_ID --handoff --request-id STABLE_ID --outcome blocked --reason '具体阻塞'
```

同一次提交重试必须使用相同 request ID 和证据。交接后不再做本任务的代码、检查、GitHub 写入或延迟工作。可以查询一次 `ink task status --task TASK_ID` 并报告结果，随后**结束当前回合**；verification pending 由同砚继续核验，不等它变为 completed，不因旧 phase 重做代码，不立即领取后继任务串起长回合。

提交响应丢失时原样重试交接，不能重新开发/审查。持续传输失败时报告结果不明并结束回合，不声称交接已成功。等待其他 Agent、GitHub 同步或 CI 的阶段交给调度器；本回合明确要求的本地检查可以正常等待工具退出。


## 批准与自动合入通知

`review_approved` 和 `pr_merged` 是信息通知，不是返工任务，不执行 claim、不自行合并或部署。先按通知中的 event ID 执行 `ink notification ack --event EVENT_ID`；只有 `first:true` 才回复，`first:false` 不重复告知。确认失败时报告收件失败。

首次接收后执行一次 `ink notification status --event EVENT_ID`，按当前持久化的 `merge.state` 告知用户：`merged` 报告已合入和可用的 `merge_sha`；`waiting` 报告自动合入等待及原因；`blocked` / `unknown` 报告阻塞或结果未确认；`disabled` 表示自动合入关闭；`superseded` / `not_approved` 表示事件不再代表当前版本或批准状态。`closed` 表示未合并即关闭。发送时的快照可能已过时；查询失败只报告该事件已经确认的事实，并说明当前状态不可读，不能据此声称尚未合入。查询是本地账本观测，可能落后于 GitHub；不循环等待、不推断部署，简短回复后结束回合。
