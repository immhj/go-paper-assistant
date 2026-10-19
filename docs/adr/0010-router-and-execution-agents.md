# 意图路由 Agent 与执行 Agent 分离

## Status

Accepted. Supersedes ADR 0005 for top-level interaction routing.

> **Partially superseded：** [ADR 0021](0021-bounded-plan-execution-on-web.md) 将 Router 与执行 Agent 合并为一次规划调用，并在 Web 通道允许最多 3 步、规划后不再看结果的有界计划；本文“每轮工具预算固定为 1”只对 QQ 通道继续有效。

> **Partially superseded：** [ADR 0017](0017-research-projects-as-web-conversation-boundary.md) 为 Web Agent 增加当前项目范围的只读论文与精读历史能力，并将项目、会话和项目论文结构管理收回用户界面；本文的双 Agent、动态最小白名单、单轮单工具和严格参数校验继续有效。

## Context

把每个论文动作做成显式斜杠命令，会要求用户先理解系统内部工作流；把全部工具同时交给一个开放式 Agent，又会扩大误调用、循环调用、成本和状态不一致风险。项目还要求新会话不继承旧会话记忆、论文序号只能引用已成功展示的论文，并且 QQ 一条用户消息最多有四次被动回复。

## Decision

所有输入进入同一个意图路由模块。只有 `/new`、`/list`、`/use`、`/current`、`/help` 作为会话命令保留；命令仅提供高置信度提示，除 `/help` 外的数据库操作仍进入执行阶段。论文检索、摘要、快速/深度精读、比较、研究问答、重命名和归档只接受自然语言。

意图路由 Agent 每轮输出一个闭合结果：`respond`、`clarify` 或 `execute`。它只读取当前会话结构化状态、当前会话最近用户原话、最近展示记录、当前用户消息，以及其他 active 会话的编号和标题；不读取其他会话内容，也不把旧模型回答作为事实记忆。问候、感谢和能力说明可以直接 respond；需要数据库或论文证据的目标必须 execute；指代不唯一或模糊归档必须 clarify。

`execute` 只携带粗粒度业务目标。Go 根据目标域生成本轮工具白名单；执行 Agent 必须从白名单中调用且只调用一个深工具。深工具包括会话管理、论文检索、当前论文列表、完整摘要、两级精读、论文比较和研究问答。下载、PDF 提取、SQL、模型调用、QQ 分片等实现细节不暴露为工具。Go 负责严格参数校验、工具预算、事务、排重、序号和交付规则。

两个 Agent 都是无状态模型。每轮记忆由数据库按 `conversation_id` 重建；`/new` 创建空状态并切换，但保留旧会话，只有 `/use` 才重新加载旧会话记忆。归档是 `conversations.status='archived'` 的逻辑归档；明确目标直接执行，模糊表达先澄清；QQ 不提供恢复命令，管理员可从后台恢复。

每条用户消息的 Router 决策、业务目标、执行工具、状态和两次模型 usage 写入 `messages.agent_trace`，但 trace 不进入 Agent 记忆，不保存密钥、完整提示词、论文全文或隐藏推理。

## Consequences

用户可以用自然语言完成论文业务，不再学习 `/read` 等命令。Router 和 Executor 各增加一次模型调用，因此延迟和成本上升；换来的是职责隔离、动态最小权限工具集和可观测执行轨迹。执行 Agent 不是 ReAct 循环，每轮工具预算固定为 1；工具内部可以有界分页或固定多步骤，但不能把结果交回 Agent 再决定第二次工具调用。
