# 迁移 Ink Commons 部署

## 确定是否接续原工作项

新环境独立接管新仓库时，从 [config.example.json](../config.example.json) 新建启动配置，再按需设置全局默认值、注册仓库和分配路由。公开示例使用虚构的仓库、身份和路径。

需要接续既有 PR 时，在停机前盘点已投递、正在运行和待交接的任务。让原领取者完成能结清的交接。对仍需提交交接或显式重试的 cooperative 任务，安全保留执行侧的原领取凭据。SQLite 备份和 JSON 导出只含领取凭据的哈希，原凭据位于领取侧状态目录的 `claims/TASK_UUID.json`，或当时通过 `--claim-file` 指定的路径。保持文件归原领取者所有、权限为 `0600`，默认 `claims` 目录权限为 `0700`。更换领取侧路径时，将原文件安全移到新默认位置，或在后续命令中通过 `--claim-file` 指向保留的文件。之后用原凭据提交交接或重试已阻塞的任务。参见 [领取与交接](handoff.md#agent-操作)。

停止源 daemon 时，核对是否有新增领取凭据需要保留。迁移窗口内只允许一套 daemon 处理这些 PR。使用 [状态备份流程](../README.md#状态备份与回滚) 生成完整备份，并在目标服务用户下恢复。将 SQLite 备份和 JSON 导出当作私有运行数据保存。里面可能含有真实账号、执行路由、文件路径、会话标识和历史配置，即使当前全局配置已经替换。

## 重建启动环境

- 为目标服务用户安装 GitHub 客户端或包装器，更新 `github_command` 和需要的 `github_merger_command` 绝对路径。配置了 reader 摘要时，用目标可执行文件的已批准摘要更新 `github_command_sha256`。专用 merger 必须设置其已批准的 `github_merger_command_sha256`。使用 GitHub App 时，在目标安装中核对 reader、reviewer 和适用的 merger 身份及对受管仓库的权限。使用自动升级时也验证 reader 能读取发布仓库。参数与身份模式见 [GitHub CLI 适配](configuration.md#github-cli-适配) 和 [自动合入身份](../README.md#自动合入身份)。
- 在私有启动配置中填入目标 `cc_connect_url` 或 `http_provider_url`。为目标服务用户单独配置所需的环境凭据，验证实际服务可达并能访问配置中的项目、会话或频道。macOS 版通过 `--credentials` 读取仅服务用户可读的凭据文件，其他部署从 daemon 环境注入。
- 按目标平台准备服务。macOS 部署可指定发布仓库和 LaunchDaemon 的工具搜索路径。接续既有 PR 时，先暂存发布包，按下节核查后再激活。参见 [macOS 部署与恢复](../deploy/macos/README.md)。

## 核对运行期配置和在途任务

恢复后先以目标服务用户启动配置核查模式。该模式暂停轮询和派发。本地 socket 支持修改运行期配置、改派工作项、记录旧执行停止确认，以及使用原凭据交接或重试 cooperative 任务。领取新任务仍然关闭。以下命令中的路径应替换为目标环境的实际路径。

```sh
ink serve -config /private/ink/config.json -data /private/ink/state -config-only
```

macOS 可以用 `manage.py stage` 暂存发布包，确保 LaunchDaemon 已停止，再以服务用户用暂存时的 Python 在前台运行该版本目录中的 `runtime.py --config-only`。这个入口会加载部署凭据。下方的 `INK_DATA_DIR` 应设为暂存版本 `deployment.json` 中的 `data`。

在另一个终端用同一服务用户操作，并让本地 CLI 指向目标状态目录。以下命令依次查看全局配置、仓库显式覆盖、仓库最终配置和 PR 最终配置。将 `example/project` 和 PR 编号替换成待迁移的对象。

```sh
export INK_DATA_DIR=/private/ink/state
ink config list --global
ink config list --repo example/project
ink config list --repo example/project --effective
ink config list --repo example/project --pr 42 --effective
```

核对 `reviewer_login`、`reviewer_command`、`human_developer` 和 `reviewer_route`，用 [`ink config set` 或 `unset`](configuration.md#cli) 修正相应作用域。PR 的显式指派可覆盖仓库设置。将 `postApproval.command` 指向目标主机上可执行的绝对路径，并在恢复批准处理前部署该文件。另行检查 `task_blocked.discord_channel_id` 或它回退使用的开发会话。

已冻结的任务配置、投递回执、PR 负责人、会话绑定和已准备的批准事件会随备份恢复。用 [工作项 API](../README.md#本地-api) 逐项核对 `config_snapshot`、显式指派和运行状态。改全局或仓库默认值只会影响后续配置解析。已保留的批准事件继续沿用其已记录的命令和去重记录。参见 [任务生效边界](configuration.md#任务生效边界)。

发现旧路由时，可在目标端保持核查模式，按下面的顺序处理：

1. 确认旧执行已停止且延迟请求无法重启。已领取的 cooperative 任务由原领取者使用原凭据提交交接。对需要迁移路由且无法继续的任务，可提交 `blocked`。旧版执行使用工作项 API 的 `confirm-stopped` 记录已核实的停止。
2. 用 `ink review` 改派 reviewer，或用工作项 API 的 `assign` 改派 developer。改派会替换该角色的当前待办并保留旧记录，仍在等待交接的任务会继续阻塞后继待办。
3. 对主动交回为 `blocked` 的旧任务，在核对新指派后，用原凭据执行 `ink task retry` 解除阻塞。已经提交的 GitHub 结果由正式服务恢复轮询后核验。参见 [阻塞和重试](handoff.md#阻塞和重试)。

例如，把 reviewer 改派到已验证的新 cc-connect 项目。替换示例值，并按需用 `--github-command` 指定 reviewer 的客户端。

```sh
ink review --github-login 'reviewer[bot]' --target new-review-project \
  https://github.com/example/project/pull/42
```

**放行条件**。确认每个恢复的工作项可以继续使用已核对的路由，或已经改派到可用的新路由。只要首轮轮询仍可能向待停用或未经核对的旧项目发出唤醒或重试，就保持轮询关闭。

满足放行条件后先停止核查进程，再省略 `-config-only` 启动正式服务。macOS 停止前台核查进程后，用 `manage.py activate` 激活暂存版本。正常激活会在验证后自动创建 `enabled` 并开始轮询。

## 重建用户级集成

以实际运行 Agent 的用户执行 `ink init` 和 `ink init --check`。使用自定义发现目录时，两条命令都传相同的 `--skills-dir`。启动新的 Agent 会话加载指令。

需要从 Git 推送登记开发来源时，在目标 checkout 检查 `git config --get core.hooksPath`。未设置时，使用 `git rev-parse --git-path hooks/pre-push` 确定安装位置。按 [pre-push 说明](../README.md#开始) 安装或接入现有 hook，再确认从实际运行 Git 的环境能通过 `PATH` 或设置的 `INK_BIN` 找到 `ink`，并能访问目标 daemon 的 socket。也可通过 [工作项 API](../README.md#本地-api) 显式登记来源。
