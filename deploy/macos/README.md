# macOS 部署与恢复

同砚在 Apple Silicon Mac 上由当前登录用户运行。需要 Python 3.11.8+、可读取发布及受管仓库的 GitHub App CLI，以及能访问的本机 cc-connect webhook。GitHub Release 发布包包含同砚及其安装脚本。

## 首次部署

先通过 personal-infra 或独立安装器准备 cc-connect。若依赖 personal-infra Discord watchdog，将 cc-connect 保持在它配置的登录域。由 personal-infra 管理 cc-connect 和 watchdog 的安装、升级、日志及恢复。

在 `~/.config/ink-commons/config.json` 中配置 `cc_connect_url`，例如 `http://127.0.0.1:9111/hook`。使用仅服务用户可读的 JSON 凭据文件，包含 `cc_connect_token` 和 `discord_token` 两个字段。兼容已有部署时，可用 `--cc-config` 与 `--project` 指定此前的 TOML 凭据源。

```sh
yuxingyi --profile inspect -R wujinhjun/ink-commons release download --pattern '*' --dir ink-deploy
(cd ink-deploy && shasum -a 256 -c SHA256SUMS && python3.12 manage.py deploy --credentials ~/.config/ink-commons/credentials.json)
```

运行 `ink init` 可为运行 Agent 的用户安装 skill。激活只操作同砚的 LaunchAgent，启动前后检查 cc-connect endpoint，并在打开轮询之前验证同砚版本和 GitHub App 权限。cc-connect 与 watchdog 在切换期间继续运行。

## 已有部署

升级以 GitHub Release 为准。默认选择 stable channel，可显式指定 `--channel preview` 或 `--tag build-<SHA>`。默认下载、校验并暂存，切换需要 `--apply`。`--dry-run` 只打印版本计划。

```sh
ink upgrade --channel stable --dry-run
ink upgrade --tag build-<SHA>
ink upgrade --tag build-<SHA> --apply
ink rollback --tag build-<previous-SHA> --dry-run
```

明确确认目标版本兼容现有 SQLite 后，可省略 rollback 的 `--dry-run` 切换。回滚保留最新状态，激活自检失败时会恢复原版本和切换前快照。回滚前保存最新状态。旧二进制没有新命令时，从发布包调用 `manage.py upgrade` 或 `manage.py rollback` 并传相同参数。

发布包包含 SHA-256 校验清单。为 CI 配置 `INK_RELEASE_SIGNING_KEY` 可发布 OpenSSH 签名。首次安装用 `--allowed-signers` 指向通过独立渠道取得的信任文件，签名身份为 `ink-release`，命名空间为 `ink-commons`。部署会沿用信任文件；选择签名校验后，缺签名或签名错误都会终止。已发布的早期无签名版本仍可按摘要验证。

若已有同砚 LaunchDaemon，升级和激活会在写入前停止。迁移前由管理员安排维护窗口，确认 cc-connect 与 watchdog 已由 personal-infra 管理，独立保留旧 plist、当前 state 和仍被引用的旧 release，再将旧同砚系统服务移出系统域。已有的系统服务与其数据留作回退依据。迁移激活使用登录域，需维持登录会话。任何仍引用旧 release 的 cc-connect 可执行文件须先迁到其独立安装目录。

## 配置权威与 drift

`~/.config/ink-commons/config.json` 是静态启动配置的默认来源。发布目录的 `config.json` 是上次暂存时保存的启动快照。修改静态来源后，下次暂存读取新内容。

运行期仓库、reviewer、合入策略和执行参数以 SQLite 为准。JSON 中的 `repositories`、`repository_defaults` 和 `execution` 是旧数据库首次导入字段，暂存会逐项标记。SQLite 初始化后，通过 `ink config` 维护这些策略。

执行 `(umask 077; ink config export > runtime-policy.json)` 可导出声明。部署时传 `--runtime-declaration runtime-policy.json`，暂存、升级及 status 会比对并报告有变化的范围。声明仅用于比对，首次安装尚无运行中 daemon 时报告 unavailable。声明可能包含本机路由和命令配置，应按私有文件保存。

## 关联开发会话

在每个开发 checkout 运行 `ink hook install`，或用 `--checkout DIR` 指定目录。安装器遵循 Git 的 hook 路径。已有自定义 hook 时，保留原脚本，可将 `"$HOME/.local/lib/ink-commons/ink" pre-push "$2"` 接入现有 hook，或在同砚用工作项 `link` 明确关联 PR。

`status --checkout OWNER/REPO=DIR` 可多次传入。每个运行期仓库显示指定 checkout 的受管 hook 状态和是否已有已关联工作项。未提供 checkout 或无法匹配 remote 时显示 unknown。已关联工作项只表示既有证据。

## 验收与恢复

- 暂存、激活和失败恢复均不复制 cc-connect，不修改其 plist，不停止或删除 watchdog。对本机 endpoint 的前后检查均为只读。
- root 调用激活或发现旧同砚 LaunchDaemon 时，在修改前退出。启用前应核对当前身份、服务模式及 GitHub self-check。
- 停止旧同砚并取得状态锁后，保存旧 plist、入口和状态快照，再启动关闭轮询 gate 的候选版本。失败时先停止候选，确认状态锁后恢复快照及旧登录服务。若无法确认候选已停止，保留备份等待恢复。
- `manage.py status --user "$(id -un)"` 显示 release SHA、gate、同砚域和外部服务状态。验证失败切换时使用打印的备份路径。已完成切换并处理过新任务后，先保存最新 SQLite 状态再安排回滚。

隔离验证使用 `python3.12 -m unittest discover -s tests/macos -v`，其中模拟 launchd 与本机 HTTP 检查。
