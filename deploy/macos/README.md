# macOS 部署与恢复

同砚在 Apple Silicon Mac 上由系统域的 `system/com.ink-commons.daemon` 启动。LaunchDaemon 随 macOS 启动，在退出桌面登录后继续运行，进程以安装它的普通用户身份运行。需要 Python 3.11.8+、可读取发布及受管仓库的 GitHub App CLI。执行端由同砚配置选择，可以先安装服务、之后再接入执行端。GitHub Release 发布包包含同砚及其安装脚本。

## 首次部署

同砚独立安装和运行。通过 `config.json` 或 `--github-command` 指定 GitHub 读取命令的绝对路径。安装包可通过有权读取该仓库的 GitHub 客户端取得。

```sh
cd ink-deploy &&
shasum -a 256 -c SHA256SUMS &&
python3.12 manage.py deploy --config ./config.example.json \
  --github-command /absolute/path/to/github-reader
```

已有静态配置时，用 `--config` 指向该文件。接入执行端时，配置其 endpoint 和凭据。cc-connect 使用 `cc_connect_url`，HTTP 执行端使用 `http_provider_url`。使用仅服务用户可读的 JSON 文件，通过 `--credentials` 传入相应的 `cc_connect_token`、`discord_token` 或 `http_provider_token`。旧版 cc-connect TOML 兼容入口需要显式提供 `--cc-config` 和 `--project`。

以服务用户运行部署命令。脚本通过 `sudo` 安装由 `root:wheel` 所有的 `/Library/LaunchDaemons/com.ink-commons.daemon.plist` 并管理系统域服务。构建、配置、状态和凭据操作由服务用户完成。运行 `ink init` 可为 Agent 用户安装 skill。激活会探测已配置的 provider endpoint，并验证同砚版本和 GitHub 读取权限。执行端服务及其 watchdog 由各自的运维方管理。

## 已有部署

从任意目录可运行 `~/.local/lib/ink-commons/ink upgrade`。暂存的部署文件先按 GitHub Release 校验，校验后再请求管理员授权以切换同砚。需要手动指定版本时，从对应 Release 执行 `manage.py stage --revision "$(cat REVISION)" --artifact-dir .`，然后以登录用户执行 `manage.py activate --release DIR --user "$(id -un)"`。

## 从旧版本升级

旧版 LaunchAgent 和 LaunchDaemon 都从已校验的新版发布包启动第一次升级。保留当前 `~/.local/lib/ink-commons/ink` 链接及其指向的 release。升级脚本从该版本读取部署配置，并在切换时备份和移除旧同砚 LaunchAgent 的 plist。旧 LaunchDaemon 可在原系统域更新。cc-connect 等外部服务若仍引用旧 release 的程序，清理旧 release 前先为对应服务配置独立的安装路径。

```sh
cd ink-deploy &&
shasum -a 256 -c SHA256SUMS &&
python3.12 ./manage.py upgrade
```

激活后通过 `python3.12 ./manage.py status --user "$(id -un)"` 核对系统域、运行 SHA 和已打开的 gate。后续使用已安装的 `ink upgrade`。

## 验收与恢复

- 暂存、激活和失败恢复均不复制 cc-connect，不修改其 plist，不停止或删除 watchdog。配置了 endpoint 时，切换前后会做只读探测。
- 在可输入管理员凭据的终端中以服务用户启动部署脚本。脚本取得授权后才停止旧服务。激活或回滚期间若授权过期，sudo 可再次请求验证。
- 系统级切换持有跨账户锁，覆盖原服务归属检查、激活及失败回滚。
- 停止旧同砚并取得状态锁后，保存旧 plist、入口和状态快照，再启动关闭轮询 gate 的候选版本。失败时先停止候选，确认状态锁后恢复快照及原先运行的同砚服务。若无法确认候选已停止，保留备份等待恢复。
- `manage.py status --user "$(id -un)"` 显示 release SHA、gate、同砚域和配置的执行端。验证失败切换时使用打印的备份路径。已完成切换并处理过新任务后，先保存最新 SQLite 状态再安排回滚。

隔离验证使用 `python3.12 -m unittest discover -s tests/macos -v`，其中模拟 launchd 与本机 HTTP 检查。
