# macOS 部署与恢复

同砚在 Apple Silicon Mac 上由当前登录用户运行。需要 Python 3.11.8+、可读取发布及受管仓库的 GitHub App CLI。执行端由同砚配置选择，可以先安装服务、之后再接入执行端。GitHub Release 发布包包含同砚及其安装脚本。

## 首次部署

同砚独立安装和运行。通过 `config.json` 或 `--github-command` 指定 GitHub 读取命令的绝对路径。安装包可通过有权读取该仓库的 GitHub 客户端取得。

```sh
cd ink-deploy
shasum -a 256 -c SHA256SUMS
python3.12 manage.py deploy --github-command /absolute/path/to/github-reader
```

接入执行端时，配置其 endpoint 和凭据。cc-connect 使用 `cc_connect_url`，HTTP 执行端使用 `http_provider_url`。使用仅服务用户可读的 JSON 文件，通过 `--credentials` 传入相应的 `cc_connect_token`、`discord_token` 或 `http_provider_token`。旧版 cc-connect TOML 兼容入口需要显式提供 `--cc-config` 和 `--project`。

运行 `ink init` 可为 Agent 用户安装 skill。激活只管理同砚的 LaunchAgent，探测显式配置的 provider endpoint，并验证同砚版本和 GitHub 读取权限。执行端服务及其 watchdog 由各自的运维方管理。


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

若已有同砚 LaunchDaemon，升级和激活会在写入前停止。迁移前由管理员安排维护窗口，确认外部服务的独立运行条件，独立保留旧 plist、当前 state 和仍被引用的旧 release，再将旧同砚系统服务移出系统域。已有的系统服务与其数据留作回退依据。迁移激活使用登录域，需维持登录会话。任何仍引用旧 release 的 cc-connect 可执行文件须先迁到其独立安装目录。

## 配置权威与 drift

`~/.config/ink-commons/config.json` 是静态启动配置的默认来源。发布目录的 `config.json` 是上次暂存时保存的启动快照。修改静态来源后，下次暂存读取新内容。

运行期仓库、reviewer、合入策略和执行参数以 SQLite 为准。JSON 中的 `repositories`、`repository_defaults` 和 `execution` 是旧数据库首次导入字段，暂存会逐项标记。SQLite 初始化后，通过 `ink config` 维护这些策略。

执行 `(umask 077; ink config export > runtime-policy.json)` 可导出声明。部署时传 `--runtime-declaration runtime-policy.json`，暂存、升级及 status 会比对并报告有变化的范围。声明仅用于比对，首次安装尚无运行中 daemon 时报告 unavailable。声明可能包含本机路由和命令配置，应按私有文件保存。

## 关联开发会话

在每个开发 checkout 运行 `ink hook install`，或用 `--checkout DIR` 指定目录。安装器遵循 Git 的 hook 路径。已有自定义 hook 时，保留原脚本，可将 `"$HOME/.local/lib/ink-commons/ink" pre-push "$2"` 接入现有 hook，或在同砚用工作项 `link` 明确关联 PR。

`status --checkout OWNER/REPO=DIR` 可多次传入。每个运行期仓库显示指定 checkout 的受管 hook 状态和是否已有已关联工作项。未提供 checkout 或无法匹配 remote 时显示 unknown。已关联工作项只表示既有证据。

## 设备 profile 与目录保留

在新设备上复制 `deployment.example.json`，填写部署用户、发布仓库、Python、GitHub App CLI和 PATH。用 `deploy --profile FILE` 或 `stage --profile FILE` 暂存。LaunchAgent 默认使用系统 PATH。需要 Node 或其他工具时，在本机 profile 的 PATH 中提供其目录。GitHub App 安装器内置绝对 Node 路径时，由其独立管理。profile 保存获准的代理环境及凭据文件路径。旧安装仍识别历史默认值。使用独立的非 root 登录服务用户可隔离日常账户；跨账户直接访问当前同砚私有 Unix socket 需要单独的执行端设计。

`status` 显示生效静态配置 SHA-256、凭据读取时间、配置漂移和运行期仓库。凭据读取时间来自 runtime 成功装载文件的时间，GitHub 连接验证时间仍使用 `last_github_ok`。外部 cc-connect 管理器若提供含 `version` 与 `commit` 的 JSON 文件，可在 profile 设置 `cc_version_file`。

```sh
python3.12 manage.py maintain
python3.12 manage.py maintain --include-legacy
```

默认列出将归档的路径。明确加 `--apply` 才归档到安装目录下的 `archive/`。保留最近的 release、备份和轮转日志，以及所有仍被入口、活动部署配置或 launchd 引用的版本。遇到无法检查的 plist 时停止归档。历史 `start`、`activate-*.command`、`staging-archive.*` 和旧 `deploy/<SHA>` 需用 `--include-legacy` 纳入计划。当前 stderr 在同砚停机切换期间按阈值轮转。归档不占用活跃保留额度，可在恢复窗口过后另外处置。

## 验收与恢复

- 暂存、激活和失败恢复均不复制 cc-connect，不修改其 plist，不停止或删除 watchdog。对本机 已配置 endpoint 的前后检查均为只读。
- root 调用激活或发现旧同砚 LaunchDaemon 时，在修改前退出。启用前应核对当前身份、服务模式及 GitHub self-check。
- 停止旧同砚并取得状态锁后，保存旧 plist、入口和状态快照，再启动关闭轮询 gate 的候选版本。失败时先停止候选，确认状态锁后恢复快照及旧登录服务。若无法确认候选已停止，保留备份等待恢复。
- `manage.py status --user "$(id -un)"` 显示 release SHA、gate、同砚域和配置的执行端。验证失败切换时使用打印的备份路径。已完成切换并处理过新任务后，先保存最新 SQLite 状态再安排回滚。

隔离验证使用 `python3.12 -m unittest discover -s tests/macos -v`，其中模拟 launchd 与本机 HTTP 检查。
