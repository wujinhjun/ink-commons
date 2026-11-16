# macOS 部署与恢复

本目录管理同砚的运行版本，以及从登录服务迁移到开机服务。`stage` 只构建一个指定 Git 提交，不切换服务；`activate` 才停止旧进程、备份状态和 plist、切换二进制并验证实际运行 SHA。不会执行 `git pull` 后偷偷更新线上版本。

需要 Go 1.23+、Python **3.11.8+**（系统自带 Python 3.9 不适用），以及已安装且可用的 Yuxingyi App CLI。以下 `PYTHON` 应替换为本机 Python 的绝对路径，`REVISION` 是待部署的完整提交 SHA，`USER` 是已有服务用户。路径包含空格时保持引号。

## 构建与版本核对

以服务用户在仓库内执行，不使用 sudo：

```sh
PYTHON=/absolute/path/to/python3.12
REVISION=<full-commit-sha>
"$PYTHON" deploy/macos/manage.py stage --revision "$REVISION"
"$PYTHON" deploy/macos/manage.py status --user "$(id -un)"
```

`stage` 从指定提交的 `git archive` 构建，不包含工作区修改；将 SHA 写入二进制。产物保存到 `~/.local/lib/ink-commons/releases/<SHA>/`，已存在时拒绝覆盖。release 内保存生成的非敏感同砚配置和本机路径，不复制 cc-connect token；启动时才从本机 `~/.cc-connect/config.toml` 读取 webhook / Discord token。本入口面向已有 cc-connect 的部署，没有 cc-connect 时需先安装或指定二进制来源。stage 会输出 cc-connect 来源路径及 SHA256 前缀；已迁移后若来源在旧 release 内，必须显式传 `--cc-connect-binary` 选择复用该二进制或升级到新二进制，避免误以为自动升级。release 还会复制现有 cc-connect 二进制并记录 SHA256，系统服务引用此稳定副本，避免依赖会话构建目录；也可通过 `--cc-connect-binary` 指定来源。可用 `--config`、`--cc-config`、`--project`、`--github-command` 覆盖来源，GitHub 默认使用 `~/.local/bin/yuxingyi`。

生成配置的 `github_command` 是绝对 App CLI 路径。每个 GitHub 请求执行 `yuxingyi --profile inspect -R OWNER/REPO api ...`，由已有 broker 获取单仓库短期 token；不会读取个人 `gh auth token`，也不缓存启动时的一小时 token。多个受管仓库分别认证，daemon 的读取失败不回退个人身份。这不改变 reviewer **提交** review 的身份；后者仍需配置仓库的 `reviewer_command`，或使用 `ink review --github-command yanshi-reviewer`。原有 `INK_GITHUB_TOKEN` 仍供其他部署方式使用；配置了 `github_command` 时以 App CLI 为准。

需要代理时，stage 可重复传非敏感环境：`--env HTTPS_PROXY=http://127.0.0.1:7897 --env NODE_OPTIONS=--use-env-proxy`。只允许 `HTTP_PROXY`、`HTTPS_PROXY`、`ALL_PROXY`、`NO_PROXY` 和上述 Node 开关；代理 URL 不允许凭据、query 或 fragment；NO_PROXY 仅允许有长度限制、无空白或凭据的逗号分隔 host 列表。环境写入私有部署清单及 plist，不能填入 token。该代理本身也需要能在无 GUI 时运行；不要把临时审查代理写入长期部署。

## 安装 Agent skill

首次安装和每次升级都应在激活前，以实际运行 Agent 的用户执行 staged CLI（不要通过 sudo 安装到 root 的 HOME）：

```sh
"$HOME/.local/lib/ink-commons/releases/$REVISION/ink" init
"$HOME/.local/lib/ink-commons/releases/$REVISION/ink" init --check
```

skill 已嵌入该版本二进制，不依赖构建工作目录。默认链接到 Pi/Codex 共用的 `~/.agents/skills`；自定义资源目录的初始化和检查均传 `--skills-dir DIR`。这一步是用户侧初始化，不会重启 cc-connect 或刷新现有 Agent 上下文，也不进入服务切换的数据库回滚流程。若回滚 CLI，应使用回滚后的 CLI 再次执行 `init`，将链接切回对应 skill 版本。初始化后新建 Agent 会话，或在原会话明确读取 skill；只读检查只能证明安装正确，不能证明当前会话已加载。

## 激活登录服务

只升级现有登录服务：

```sh
"$PYTHON" deploy/macos/manage.py activate \
  --release "$HOME/.local/lib/ink-commons/releases/$REVISION" --user "$(id -un)"
```

## 开机服务迁移

macOS 的 LaunchAgent 在用户登录后启动；LaunchDaemon 在系统启动后运行，可用 `UserName` 让进程仍以普通用户运行。参见 [Apple 的 launchd 说明](https://developer.apple.com/library/archive/documentation/MacOSX/Conceptual/BPSystemStartup/Chapters/CreatingLaunchdJobs.html)。本脚本不会开启自动登录，也不会让同砚或 cc-connect 以 root 运行。

首次系统迁移必须同时迁移已有 cc-connect LaunchAgent：

```sh
sudo "$PYTHON" deploy/macos/manage.py activate --system \
  --user "$(id -un)" \
  --release "$HOME/.local/lib/ink-commons/releases/$REVISION" \
  --cc-connect-agent "$HOME/Library/LaunchAgents/com.cc-connect.service.plist"
```

脚本使用 release 中固定的 cc-connect 二进制和既有配置，写入 `/Library/LaunchDaemons/`；备份并移除用户目录中的两个旧 plist，避免登录后启动第二份。先备份、停用并摘除旧 `com.personal-infra.discord-watchdog` GUI plist，防止它重新拉起旧 cc-connect；随后摘除 GUI 服务入口并停服务。不会安装新的 Discord 连接 watchdog，system 服务只依赖 launchd 进程退出重启，Discord 断线恢复仍需另行验证。显式设置 HOME、可执行文件路径、RunAtLoad 和 KeepAlive。之后升级同砚仍用 `--system`，可省略 `--cc-connect-agent`，会复用已安装的 cc-connect 系统 plist。脚本会短暂停止并重启这两个服务；不要在有 Agent 正在执行时切换。

FileVault 启用时，断电后的冷启动仍须先解锁磁盘。这是磁盘加密边界，LaunchDaemon 不能绕过。本脚本不关闭 FileVault，也不承诺无人解锁的冷启动恢复。

cc-connect 的网络代理、Agent 运行时和模型凭据也必须能在无 GUI 会话时使用。本脚本只迁移这两个服务，不改代理或其他凭据服务，不提供 cc-connect 新执行协议。首次迁移之后必须实际验证无 GUI 环境中的网络连接与 Agent 执行，才能认定整条链路可在退出登录后工作。

## 激活验证与失败恢复

切换先停止旧服务；`bootout` 后最多等待 45 秒确认 job 已移除，再确认释放状态锁，最多等待状态锁释放五秒，再复制私有状态快照（排除 socket、锁和日志）。旧 `~/.local/lib/ink-commons/start` 不再被 launchd 引用，它仍会读取个人 gh token，不要再用它启动服务。新版通过 `-poll-gate` 启动：可初始化 / 迁移状态并提供本地 API，但 gate 文件出现之前不轮询 GitHub、不派发任务。脚本核对 `/healthz` 的 SHA 和关闭的 gate、cc-connect 本机端口唯一监听者与 system 服务 PID 一致，并调用 daemon 自身的只读 `GET /v1/self-check` 验证所有配置仓库的 PR 读取权限。self-check 具有总请求超时，并最多重试三次；失败只返回仓库和脱敏的超时/退出码分类，不转发 CLI stderr 或配置。全部通过才写 gate 文件并更新 hook 使用的 `~/.local/lib/ink-commons/ink` 入口。重启后 gate 保留，服务继续正常轮询。

验证期间失败会先停新版，再恢复状态快照、旧二进制和 plist 并重新加载原服务。若无法停下新版，或等待五秒后仍无法取得状态锁，拒绝覆盖数据库，保留备份等待人工恢复，并输出阶段、SHA、备份目录、gate 状态和各部分恢复结果。回滚恢复原 plist 权限/属主、入口符号链接形态与 launchd enabled/disabled 状态；子目录也恢复服务用户属主。不会在已开始派发后自动回滚数据库。

停服务前会输出备份位置。备份位于 `~/.local/lib/ink-commons/backups/<timestamp>/`，包含旧状态、旧二进制及原 plist；系统模式的备份由 root 保护。备份含 PR / thread 关联和原服务环境，不应提交 Git 或上传。可在激活成功后执行：

```sh
"$PYTHON" deploy/macos/manage.py status --user "$(id -un)"
curl --unix-socket "$HOME/.local/state/ink-commons/ink.sock" http://ink/healthz
```

`status` 分别报告两个服务及旧 watchdog 在 system / GUI domain 中是否加载、cc-connect 进程 PID 和配置端口的监听 PID及 plist 二进制存在状态，以及本地 API 的运行 SHA。状态目录优先从当前 plist 对应的部署清单解析。`/healthz` 同时显示 `gate: open|closed`、`last_poll`（最近完成轮询）及 `last_github_ok`（最近成功读取，包含 self-check），便于发现 gate 被误删或持续上游故障；这些时间是当前进程内的观察值，重启后重新记录。健康和端口检查不能证明审查闭环成功；需另用测试 PR 验证派审、正式 review 同步及回流。真实重启测试应在工作空闲时进行。

激活成功且已开始处理工作之后，如需回滚旧版，先停止服务，用当前版本的 `ink state backup` / `ink state export` 保存**最新**状态，再按根 README 的 SQLite 回滚步骤操作。不能直接用激活前的快照覆盖这期间产生的新任务。恢复旧 plist 后要卸载对应 system 服务、还原原 domain，避免两个进程并存。部署脚本只自动恢复验证失败的切换，不提供运行一段时间之后的一键数据降级。

## 离线验证

```sh
go test -race ./...
go vet ./...
"$PYTHON" -m unittest discover -s tests/macos -v
```

Python 测试使用隔离目录和假的 launchctl、daemon self-check、socket，不重启本机服务、不发送消息。覆盖系统迁移去重、运行版本 gate、App 检查失败后的数据库与 plist 恢复，以及运行时不继承个人 GitHub token。
