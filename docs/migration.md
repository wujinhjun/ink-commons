# 迁移 Ink Commons 部署

## 确定是否接续原工作项

新环境独立接管新仓库时，从 [config.example.json](../config.example.json) 新建启动配置，再按需设置全局默认值、注册仓库和分配路由。公开示例使用虚构的仓库、身份和路径。

需要接续既有 PR 时，在源环境停止 daemon，并确认执行端是否仍有已投递或正在运行的任务。迁移窗口内只允许一套 daemon 处理这些 PR。使用 [状态备份流程](../README.md#状态备份与回滚) 生成完整备份，并在目标服务用户下恢复。将 SQLite 备份和 JSON 导出当作私有运行数据保存。里面可能含有真实账号、执行路由、文件路径、会话标识和历史配置，即使当前全局配置已经替换。

## 重建启动环境

- 为目标服务用户安装 GitHub 客户端或包装器，更新 `github_command` 和需要的 `github_merger_command` 绝对路径。配置了 reader 摘要时，用目标可执行文件的已批准摘要更新 `github_command_sha256`。专用 merger 必须设置其已批准的 `github_merger_command_sha256`。使用 GitHub App 时，在目标安装中核对 reader、reviewer 和适用的 merger 身份及对受管仓库的权限。使用自动升级时也验证 reader 能读取发布仓库。参数与身份模式见 [GitHub CLI 适配](configuration.md#github-cli-适配) 和 [自动合入身份](../README.md#自动合入身份)。
- 在私有启动配置中填入目标 `cc_connect_url` 或 `http_provider_url`。为目标服务用户单独配置所需的环境凭据，验证实际服务可达并能访问配置中的项目、会话或频道。macOS 版通过 `--credentials` 读取仅服务用户可读的凭据文件，其他部署从 daemon 环境注入。
- 按目标平台安装服务。macOS 部署可指定发布仓库和 LaunchDaemon 的工具搜索路径。迁移到其他服务管理器时，用同一目标用户运行 `ink serve`，明确传入启动配置和状态目录，并让本地 CLI 通过 `INK_DATA_DIR` 找到同一个 socket。参见 [macOS 部署与恢复](../deploy/macos/README.md)。

## 核对运行期配置和在途任务

用以下命令查看全局配置、仓库显式覆盖和最终配置。将 `example/project` 和 PR 编号替换成待迁移的对象。

```sh
ink config list --global
ink config list --repo example/project
ink config list --repo example/project --effective
ink config list --repo example/project --pr 42 --effective
```

核对 `reviewer_login`、`reviewer_command`、`human_developer` 和 `reviewer_route`。PR 的显式指派可覆盖仓库设置。将 `postApproval.command` 指向目标主机上可执行的绝对路径，并在恢复批准处理前部署该文件。另行检查 `task_blocked.discord_channel_id` 或它回退使用的开发会话。

已冻结的任务配置、投递回执、PR 负责人、会话绑定和已准备的批准事件会随备份恢复。逐项对照当前工作项的 `config_snapshot` 和运行状态。仍可访问原执行端时，按原路由等待或确认终止。在原执行停止且无法延迟重新启动后，再按 [交接和重试规则](../README.md#执行可靠性) 处理，并通过显式改派或创建新任务使用新配置。改全局默认值只会影响后续配置解析。已保留的批准事件继续沿用其已记录的命令和去重记录。参见 [任务生效边界](configuration.md#任务生效边界)。

## 重建用户级集成

以实际运行 Agent 的用户执行 `ink init` 和 `ink init --check`。使用自定义发现目录时，两条命令都传相同的 `--skills-dir`。启动新的 Agent 会话加载指令。

需要从 Git 推送登记开发来源时，在目标 checkout 检查 `git config --get core.hooksPath`。未设置时，使用 `git rev-parse --git-path hooks/pre-push` 确定安装位置。按 [pre-push 说明](../README.md#开始) 安装或接入现有 hook，再确认从实际运行 Git 的环境能通过 `PATH` 或设置的 `INK_BIN` 找到 `ink`，并能访问目标 daemon 的 socket。也可通过 [工作项 API](../README.md#本地-api) 显式登记来源。
