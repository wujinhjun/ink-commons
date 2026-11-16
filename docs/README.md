# 项目文档

- [主动领取与业务交接](handoff.md)：cooperative 模式的启用、领取、幂等交接、结果核验和异常恢复。
- [执行生命周期与可靠执行端](execution.md)：task/attempt、Monitor 协议、执行账本与恢复边界。
- [macOS 部署与恢复](../deploy/macos/README.md)：指定版本构建、launchd 激活、校验及失败回滚。

## 流程图

[cooperative 流程图](diagrams/cooperative-workflow.png) 按纵向主线说明首次开发、审查、返工、交接核验和可选自动合入。右侧备注对应通用配置项，底部单独展示任务状态；所有配置示例均为占位值。

- 图片：[`diagrams/cooperative-workflow.png`](diagrams/cooperative-workflow.png)
- 可编辑源文件：[`diagrams/cooperative-workflow.excalidraw`](diagrams/cooperative-workflow.excalidraw)

用 Excalidraw 打开源文件编辑，保留右侧配置与主流程的对应关系；导出 PNG 时同步更新图片和源文件，检查中文字体、标签及连线。修改后确认 README 的相对图片链接可用，并保持项目说明不包含具体使用者的账号、仓库、地址或本机路径。

流程图描述 cooperative 模式；其他模式、协议细节和恢复条件以相应文档为准。
