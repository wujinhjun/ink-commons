# Ink Commons 同砚

同砚跟踪 GitHub PR 的开发和审查交接。第一版提供 Go daemon、Unix socket HTTP API 和 Git hook。人和 agent 都可以在 GitHub 上开发、送审和提交正式 review。

## 开始

需要 Go 1.23+ 和 Git。daemon 跟踪配置中的仓库。私有仓库使用具备 Pull requests 读取权限的 GitHub token，通过 `INK_GITHUB_TOKEN` 注入。bot 模式另外需要 `INK_DISCORD_TOKEN`、`INK_CC_CONNECT_TOKEN`，以及启用 Discord thread 隔离的 cc-connect reviewer project。Discord bot 需要能创建和访问目标频道的公共 thread。配置中的 `reviewer_login` 要和 GitHub 上实际提交 review 的身份一致。

```sh
go build -o ink ./cmd/ink
cp config.example.json config.json
./ink serve -config config.json
```

默认状态目录是 `$XDG_STATE_HOME/ink-commons` 或 `~/.local/state/ink-commons`。daemon 与 hook 可以通过同一个 `INK_DATA_DIR` 覆盖。HTTP API 通过仅当前用户可访问的 `ink.sock` 提供。配置文件使用 `config.example.json` 的结构，凭据从环境变量读取。若启用 cc-connect，`cc_connect_url` 填其完整的本机 webhook 地址，例如 `http://127.0.0.1:9111/hook`。

受管仓库从 `git rev-parse --git-path hooks/pre-push` 确认默认 hook 位置。使用 `core.hooksPath` 的工作区按该设置安装。没有现有 hook 时，把 `scripts/ink-pre-push` 安装为可执行的 `pre-push`。已有 hook 时，从现有 hook 为同砚和原检查分别传入同一份标准输入。让 cc-connect agent 的 PATH 能找到 `ink`，或通过 `INK_BIN` 指定绝对路径。

## 走完一轮

1. 开发 agent 在自己的 Discord thread 里建分支、推送并开 Draft PR。hook 从 cc-connect 环境读取路由，并登记 Git 准备推送的源仓库、目标分支和 SHA。daemon 轮询 GitHub 找到匹配的 PR 后确认关联。同一版本有两个候选会话时，PR 等待认领。
2. 在 GitHub 标记 Ready for review。同砚为当前 head SHA 建立待办。配置了 reviewer bot 时，创建一个 Discord thread 并唤醒它。可以将随附的 review skill 提供给对应 agent。
3. reviewer 在 GitHub 提交 Approve 或 Request changes。机器人和人在 GitHub 上提交的 review 使用同一同步逻辑。只有指定 reviewer 对当前 SHA 的正式结论才会推进状态。需要修改时把任务投递给开发会话，或留给当前人工负责人。
4. 新 commit 将进入下一轮，沿用已有 review thread。历史 review 按原 SHA 保留。工作项合并或关闭后结束待办。

GitHub 的当前 PR 快照是状态来源。轮询间隔默认 60 秒。hook 路由候选保留七天，仅精确匹配 PR 已观察到的 head SHA。漏装 hook、首次观察之前 head 已变化或归属含糊时，可以用 `link` 补登记。

## 本地 API

```sh
socket="${INK_DATA_DIR:-${XDG_STATE_HOME:-$HOME/.local/state}/ink-commons}/ink.sock"
curl --unix-socket "$socket" http://ink/v1/work
curl --unix-socket "$socket" http://ink/v1/work/owner/repository/42
curl --unix-socket "$socket" http://ink/v1/work/owner/repository/42/assign \
  -H 'Content-Type: application/json' \
  -d '{"role":"developer","github_login":"your-login"}'
```

| 接口 | 行为 |
|---|---|
| `GET /healthz` | 检查本地 API 可达。 |
| `GET /v1/work` | 列出已观察的 PR 和当前待办。 |
| `GET /v1/work/{owner}/{repo}/{number}` | 查看一条 PR 的来源、负责人、审查版本和投递状态。 |
| `POST /v1/work/{owner}/{repo}/{number}/link` | 手动关联未认领 PR。请求体提供 `github_login` 或 `route`。路由包含 `project`、`session_key`。 |
| `POST /v1/work/{owner}/{repo}/{number}/assign` | 为 `developer` 或 `reviewer` 更新负责人。传 `github_login` 即可转给人。传 `route` 可指定已有 bot 会话。 |
| `POST /v1/work/{owner}/{repo}/{number}/retry` | 在下次轮询重试当前 bot 待办。先检查 Discord 是否已经开始，避免重复执行。 |
| `POST /v1/pushes` | Git hook 提交候选关联记录。 |

`assign` 修改未来任务的投递。接手之前如果 bot 正在工作，先在 cc-connect 停止它，再接管目录或处理未提交改动。`accepted` 表示 cc-connect 接收请求，正式 GitHub review 或新的 PR 版本才表示工作状态推进。cc-connect 忙时可能跳过任务，待办仍可在 API 中查看、重试或转给人。

未来可从 Discord 开发 thread 按需分配 worktree 和分支，见 [TODO](TODO.md)。
