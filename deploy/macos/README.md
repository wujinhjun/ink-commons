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

`stage` 从指定提交的 `git archive` 构建，不包含工作区修改；将 SHA 写入二进制。产物保存到 `~/.local/lib/ink-commons/releases/<SHA>/`，已存在时拒绝覆盖。release 内保存生成的非敏感同砚配置和本机路径，不复制 cc-connect token；启动时才从本机 `~/.cc-connect/config.toml` 读取 webhook / Discord token。可用 `--config`、`--cc-config`、`--project`、`--github-command` 覆盖来源，GitHub 默认使用 `~/.local/bin/yuxingyi`。

生成配置的 `github_command` 是绝对 App CLI 路径。每个 GitHub 请求执行 `yuxingyi --profile inspect -R OWNER/REPO api ...`，由已有 broker 获取单仓库短期 token；不会读取个人 `gh auth token`，也不缓存启动时的一小时 token。多个受管仓库分别认证，失败不回退个人身份。原有 `INK_GITHUB_TOKEN` 仍供其他部署方式使用；配置了 `github_command` 时以 App CLI 为准。

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

脚本保留 cc-connect 已安装的二进制和配置，写入 `/Library/LaunchDaemons/`；备份并移除用户目录中的两个旧 plist，避免登录后启动第二份。显式设置 HOME、可执行文件路径、RunAtLoad 和 KeepAlive。之后升级同砚仍用 `--system`，可省略 `--cc-connect-agent`，会复用已安装的 cc-connect 系统 plist。脚本会短暂停止并重启这两个服务；不要在有 Agent 正在执行时切换。

FileVault 启用时，断电后的冷启动仍须先解锁磁盘。这是磁盘加密边界，LaunchDaemon 不能绕过。本脚本不关闭 FileVault，也不承诺无人解锁的冷启动恢复。

cc-connect 的网络代理、Agent 运行时和模型凭据也必须能在无 GUI 会话时使用。本脚本只迁移这两个服务，不改代理或其他凭据服务，不提供 cc-connect 新执行协议。首次迁移之后必须实际验证无 GUI 环境中的网络连接与 Agent 执行，才能认定整条链路可在退出登录后工作。

## 激活验证与失败恢复

切换先停止旧服务并确认释放状态锁，再复制私有状态快照（排除 socket、锁和日志）。新版通过 `-poll-gate` 启动：可初始化 / 迁移状态并提供本地 API，但 gate 文件出现之前不轮询 GitHub、不派发任务。脚本核对 `/healthz` 的 SHA、cc-connect 本机监听端口，并用 App 身份读取所有配置仓库；全部通过才写 gate 文件并更新 hook 使用的 `~/.local/lib/ink-commons/ink` 入口。重启后 gate 保留，服务继续正常轮询。

验证期间失败会先停新版，再恢复状态快照、旧二进制和 plist 并重新加载原服务。若无法停下新版，拒绝覆盖数据库，保留备份等待人工恢复。不会在已开始派发后自动回滚数据库。

备份位于 `~/.local/lib/ink-commons/backups/<timestamp>/`，包含旧状态、旧二进制及原 plist；系统模式的备份由 root 保护。备份含 PR / thread 关联和原服务环境，不应提交 Git 或上传。可在激活成功后执行：

```sh
"$PYTHON" deploy/macos/manage.py status --user "$(id -un)"
curl --unix-socket "$HOME/.local/state/ink-commons/ink.sock" http://ink/healthz
```

`status` 分别报告两个服务在 system / GUI domain 中是否加载，以及本地 API 的运行 SHA。健康和端口检查不能证明审查闭环成功；需另用测试 PR 验证派审、正式 review 同步及回流。真实重启测试应在工作空闲时进行。

激活成功且已开始处理工作之后，如需回滚旧版，先停止服务，用当前版本的 `ink state backup` / `ink state export` 保存**最新**状态，再按根 README 的 SQLite 回滚步骤操作。不能直接用激活前的快照覆盖这期间产生的新任务。恢复旧 plist 后要卸载对应 system 服务、还原原 domain，避免两个进程并存。部署脚本只自动恢复验证失败的切换，不提供运行一段时间之后的一键数据降级。

## 离线验证

```sh
go test -race ./...
go vet ./...
"$PYTHON" -m unittest discover -s tests/macos -v
```

Python 测试使用隔离目录和假的 launchctl、App CLI、socket，不重启本机服务、不发送消息。覆盖系统迁移去重、运行版本 gate、App 检查失败后的数据库与 plist 恢复，以及运行时不继承个人 GitHub token。
