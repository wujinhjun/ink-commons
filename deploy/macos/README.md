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

首次部署 fork 的发布包时，给 `deploy` 加上 `--release-repo owner/repo`。该仓库应在 `main` 发布与提交 SHA 对应的 `build-<SHA>` Release。服务用户的读取客户端需要能访问此发布仓库和受管仓库。省略参数时使用官方发布仓库。已有安装可按下文切换发布源。

如果读取客户端或其他子命令依赖不同的工具目录，可在 `deploy` 或 `stage` 时指定 `--service-path '/absolute/bin:/usr/bin:/bin'`。`ink upgrade` 会继承已保存的发布仓库和服务 PATH。服务依然使用 macOS LaunchDaemon 和部署账户下的版本、状态目录。

## 已有部署

从任意目录可运行 `~/.local/lib/ink-commons/ink upgrade`。暂存的部署文件先按 GitHub Release 校验，校验后再请求管理员授权以切换同砚。

手动安装另一版本时，以服务用户从目标仓库取得发布包，再执行 `stage` 和 `activate`。已有安装会拒绝 `deploy`。手动暂存要求对应的 `~/.local/lib/ink-commons/releases/<SHA>` 目录尚不存在。保留活动 release 及其入口，供当前服务和回滚使用。

手动 `stage` 时，传入已有私有配置，并按需重传 `--credentials`（或 `--cc-config` 和 `--project`）以及代理的 `--env` 选项。省略 `--release-repo` 或 `--service-path` 时，暂存会继承活动版本保存的对应设置。传入参数可以覆盖它们。活动版本链接或部署清单损坏时，继承会报错。旧式普通可执行文件入口没有版本清单，暂存会提示省略的选项将使用默认值。活动版本未保存对应设置时，也使用官方发布仓库或默认的 `~/.local/bin:/opt/homebrew/bin:/usr/bin:/bin`。

```sh
revision="$(cat REVISION)"
release="$HOME/.local/lib/ink-commons/releases/$revision"
shasum -a 256 -c SHA256SUMS &&
  python3.12 manage.py stage --revision "$revision" --artifact-dir . \
    --config /absolute/path/to/current-config.json \
    --release-repo fork-owner/ink-fork \
    --service-path '/absolute/bin:/usr/bin:/bin' &&
  python3.12 manage.py activate --release "$release" --user "$(id -un)"
```

### 保留当前 SHA 并切换发布源

目标仓库已经发布当前 SHA 的 `build-<SHA>` 时，以服务用户从支持该命令的版本运行：

```sh
python3.12 manage.py set-release-repo fork-owner/ink-fork
```

命令用当前部署的 GitHub 读取客户端确认目标发布可读，然后原子更新活动版本的发布仓库设置。运行的二进制、LaunchDaemon、服务 PATH 和状态继续沿用当前版本。将仓库参数设回原值可切回原发布源。如果活动版本自带的升级脚本早于此功能，首次升级请从支持该功能且已校验的发布包运行 `manage.py upgrade`。

## 从旧版本升级

旧版 LaunchAgent 和 LaunchDaemon 都从已校验的新版发布包启动第一次升级。若 `~/.local/lib/ink-commons/ink` 是普通可执行文件，使用上文的 `stage` 和 `activate` 手动迁移。若入口是版本链接，保留链接及其指向的 release，再运行下方的 `upgrade`。升级脚本从该版本读取部署配置，并在切换时备份和移除旧同砚 LaunchAgent 的 plist。旧 LaunchDaemon 可在原系统域更新。cc-connect 等外部服务若仍引用旧 release 的程序，清理旧 release 前先为对应服务配置独立的安装路径。

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
