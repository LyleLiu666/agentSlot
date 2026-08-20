# Agent 设计的架构讨论

## 1. 文档定位

本文是 AgentSlot 中 Session、固定 `AgentRuntime` 和标准组件 Slot 的架构决策账本，
不是聊天记录，也不是已经完成的代码说明。确定的结论直接约束后续接口和实现；
标记为“待商榷”的事项必须在编码触及对应边界前再次评审。

本文不把候选 Go 接口写成已经实现的合同。`AgentRuntime` 和标准 `Gateway` 是框架
固定对象，不参与 Slot 成熟度计分；新增或改名的标准组件仍保持 `Mapped`，直到满足
组件地图的证据要求。

相关实施步骤见
[AgentRuntime 与标准 Slot 实施计划](agent-runtime-standard-slots-implementation-plan.zh-CN.md)。
完整对象所有权、Gateway 主链路、包依赖和部署边界见
[AgentSlot 标准 Agent 框架全景架构](agent-framework-architecture.zh-CN.md)。

## 2. 总体对象关系

```mermaid
flowchart TD
    START["启动程序"] --> AR["启动后的 Application Runtime"]
    AR --> REG["RuntimeRegistry"]
    AR --> G["固定 Gateway"]
    AR --> RC["框架 Runtime 协调器"]
    AR --> A["Application Assembly"]
    A --> SM["SessionManager"]
    A --> IC["InteractionCommand 组件"]
    SM --> SS["SessionStore"]
    A --> ME["ModelExecutor"]
    W["Workspace"] --> S1["Session A"]
    W --> S2["Session B"]
    SM --> S1
    SM --> S2
    RC --> SM
    RC --> REG
    REG -->|"Create / Resume 成功"| RT1["AgentRuntime A"]
    REG -->|"Create / Resume 成功"| RT2["AgentRuntime B"]
    RT1 --> S1
    RT2 --> S2
    S1 --> MC1["SessionModelConfig A"]
    S2 --> MC2["SessionModelConfig B"]
    RT1 --> ME
    RT2 --> ME
    S1 --> R1["同一时刻最多一个活跃 Run"]
    S2 --> R2["同一时刻最多一个活跃 Run"]
    E["Entrypoint / 传输适配器"] --> G
    IC --> G
    G --> RC
    RT1 -. "事件" .-> G
    RT2 -. "事件" .-> G
```

基本包含关系如下：一个 Application Assembly 可以服务多个 Workspace；一个 Workspace
可以有多个 Session；一个 Session 可以先后产生多个 Run，但同一时刻最多有一个
活跃 Run；一个 Run 由若干 Step 组成；完整 Message 属于 Session，并记录其来源
Run/Step。sub-agent 是独立执行参与者，必须拥有独立 Session，并通过父子关系与
发起方关联。`Application.Start` 创建的应用级 Runtime 是有意确定的单进程执行边界：
它持有进程内 RuntimeRegistry，该注册表中的全部 AgentRuntime 都运行在该进程；
RuntimeCoordinator 只操作注册表，不拥有它。

## 3. 装配、身份与生命周期

### A-001 一个 Application Assembly 服务多个 Workspace 和 Session

- **问题 / 背景：** Assembly 是 Build 产生的应用装配结果，不应与一次会话的短生命周期绑定；`Plan` 在 Agent 领域又容易被误解为任务计划或 Plan-Act 策略。
- **最终决定：** 目标公共名称使用 `Assembly`（应用装配结果）。一个已构建并启动的 Application Assembly 可以同时服务多个 Workspace 和 Session。
- **必须满足的不变量：** Assembly 内组件选择和启动顺序固定；不同 Session 的运行状态相互隔离；运行时不能把 Assembly 当作服务定位器。
- **否决的方案及原因：** 每个 Session 创建独立 Assembly，会重复启动应用级组件，并把会话隔离错误地变成重复装配；继续使用 `Plan` 会与 Agent 自己的任务计划混淆。
- **对接口、存储、Gateway 和实现的影响：** 公共 API 为 `Application.Build() -> *Assembly`、`Assembly.Start() -> *Runtime`、`Assembly.Describe()` 和 `Runtime.Assembly()`，描述格式为 `agentslot.assembly/v0`。不保留旧 `Plan`、`PlanDescription` 名称的长期兼容别名。Assembly 固定 SessionManager、SessionStore、ModelExecutor、InteractionCommand 等共享组件选择；启动后的 Application Runtime 持有 Gateway、Registry 和 AgentRuntime。
- **状态：** 已确定。

### A-002 `AgentRuntime` 是框架固定对象，不是 Slot

- **问题 / 背景：** Slot 表示开发者能够独立实现和替换的边界；而标准循环的事务顺序和状态控制必须只有一个权威。
- **最终决定：** `AgentRuntime` 及其内部循环由框架固定，不定义标准 `agent.loop` Slot，也不定义公共 `AgentLoopFactory` 或独立 `AgentLoop` 对象。
- **必须满足的不变量：** 标准 Profile 只有一套循环不变量；Hook、Provider 或 Entrypoint 都不能替代 Runtime 控制状态。
- **否决的方案及原因：** 一边禁止替换循环、一边保留 `agent.loop` Slot 是自相矛盾的 API；共享全局 Loop 又会混合多个 Session 的状态。
- **对接口、存储、Gateway 和实现的影响：** 组件地图删除 `agent.loop`。需要完全不同循环的项目可用通用装配核心定义本地 Slot，但不属于标准 LLM Agent Profile。
- **状态：** 已确定。

### A-003 启动后的 Application Runtime 持有进程内 RuntimeRegistry

- **问题 / 背景：** 并行 Session 必须在内存状态、取消和等待关系上隔离，同时注册表的所有者必须与应用启动和停止边界一致。
- **最终决定：** `Application.Start` 创建的应用级 Runtime 持有进程内 `RuntimeRegistry`。浏览或列出 Session 不进入注册表；`CreateSession` 或 `ResumeSession` 成功时创建绑定该 Session 的 `AgentRuntime` 并登记，idle 时继续驻留，直到显式 `Close` 或应用停止。
- **必须满足的不变量：** 一个启动后的 Application Runtime 是单进程执行边界；它管理的全部 AgentRuntime 都位于同一进程。同一 SessionID 在该注册表中只有一个 Runtime；并发 resume 汇合到同一实例；初始化失败不登记半成品；不同 Session 的 Runtime 可以并行。持久化但未 create/resume 的 Session 不占用内存 Runtime。
- **否决的方案及原因：** 每个 Run 创建临时 Loop 会反复重建命令、取消和配置边界；对只读浏览也创建 Runtime 会把查询行为变成资源占用；让 Gateway、SessionManager、Entrypoint 或全局变量持有注册表会使生命周期和关闭责任分裂；把单进程边界描述为第一版临时妥协会为未设计的分布式所有权留下隐式分支。
- **对接口、存储、Gateway 和实现的影响：** RuntimeCoordinator 只操作注册表，不拥有它；`Runtime.Stop` 负责关闭并清空。`Close` 只移除并释放内存 AgentRuntime，不删除 Session。再次 resume 使用最新 Runtime 固定配置，但恢复该 Session 已持久化的模型配置。标准架构不把一个 Application Runtime 的 Session 拆到多个进程；未来若引入跨进程 Session 所有权、租约或迁移，必须作为新的架构决策重新评审。
- **状态：** 已确定。

### A-004 SessionManager 与 SessionStore 分离，Runtime 负责命令和执行

- **问题 / 背景：** Session 创建、恢复、fork、持久化和 Agent 命令具有不同变化原因。
- **最终决定：** `session.manager` 负责 create/resume/fork/摘要启动；`session.store` 负责 History、Context、Queue、RunJournal、SessionModelConfig 和 revision/CAS 原子事务；固定 Runtime 接收 `Send`、`Steer`、`RunPending`、Queue 操作、`ModelConfig`、`UpdateModelConfig`、`Cancel`、`WhenIdle` 和 `Close`。
- **必须满足的不变量：** Manager 依赖 Store；Runtime 只接收已恢复完成的 Session；任何组件都不能绕过 SessionStore 的核心事务。
- **否决的方案及原因：** Manager 与 Store 合并会让生命周期服务和存储机制无法独立替换；把这些职责塞进循环会让 fork、恢复和存储迁移污染执行代码。
- **对接口、存储、Gateway 和实现的影响：** `session.manager` 与 `session.store` 都是标准 `One` Slot；`CreateSession`/`ResumeSession` 是生命周期入口，不是 Runtime 实例命令。
- **状态：** 已确定。

### A-005 身份和包含关系

- **问题 / 背景：** Agent、sub-agent、Session、Run、Step、Message 容易被混成同一层对象。
- **最终决定：** `AgentID` 标识配置和能力集合；`SessionID` 标识隔离会话；`RunID` 标识一次从运行到停止的执行；`StepID` 标识一次模型调用或工具批次边界；`MessageID` 标识持久化完整消息。sub-agent 另有身份，但执行时同样绑定独立 Session。
- **必须满足的不变量：** Message 可追溯到 Session，并在适用时追溯到 Run/Step；Run 不能跨 Session；Step 不能跨 Run。
- **否决的方案及原因：** 用内存对象地址或数组下标充当身份，无法跨进程重启、RPC 或持久化引用。
- **对接口、存储、Gateway 和实现的影响：** 所有持久化记录和事件使用稳定 ID；RPC 不传递内存对象句柄。
- **状态：** 已确定。

### A-006 Session 内串行、Session 间并行

- **问题 / 背景：** 同一上下文上的多个并行 Run 会争夺队列和 Context 尾部。
- **最终决定：** 同一 Session 同一时刻只允许一个活跃 Run；不同 Session 可以并行运行。
- **必须满足的不变量：** 活跃 Run 的认领必须原子化；重复启动返回冲突或同一结果，不能产生第二个活跃 Run。
- **否决的方案及原因：** 同一 Session 并行 Run 会破坏消息顺序、工具调用配对和 Context 版本。
- **对接口、存储、Gateway 和实现的影响：** SessionStore 需要活跃 Run 执行权 CAS；共享组件必须支持多个 Session Runtime 并发调用。
- **状态：** 已确定。

### A-007 sub-agent 使用独立 Session

- **问题 / 背景：** sub-agent 需要隔离上下文和失败边界，但仍要继承必要配置。
- **最终决定：** 每个 sub-agent 必须使用独立 Session；完整历史 fork 与摘要启动是两个显式操作。
- **必须满足的不变量：** 子 Session 有独立 ID、Queue、Context、History 和 Run；父子关系可追踪；两种创建方式不得悄悄互换。
- **否决的方案及原因：** 让 sub-agent 共用父 Session 会混淆权限、上下文和并发；把摘要伪装成 fork 会丢失审计语义。
- **对接口、存储、Gateway 和实现的影响：** SessionManager 分别提供 fork 和基于摘要创建。完整 fork 复制指定 revision 的完整 History，并为子 Session 重写消息、工具调用、Run 和 Step 身份；Context 按子 Session 最终模型重新派生，Queue 与 RunJournal 是来源 Session 的未完成投递/执行状态，不复制到子 Session。摘要启动只提交显式摘要输入。新的 Session 获得独立 Runtime 固定配置快照，并默认继承来源 Session 当前的 SessionModelConfig，创建命令可以显式覆盖。
- **状态：** 已确定。

## 4. 固定 Gateway 与客户端交互

### A-008 Gateway 是框架固定的应用级交互后端

- **问题 / 背景：** TUI、Web、桌面端、CLI 和嵌入式函数调用必须共享同一套用户操作语义，又不能直接耦合 AgentRuntime 或某种网络协议。
- **最终决定：** 标准 Agent 框架提供一个固定、进程内的 Gateway 后端；每个启动后的 Application Runtime 持有一个 Gateway 实例。所有面向用户或外部调用方的 Entrypoint 都只能通过 Gateway 操作 Agent。Gateway 不是 Slot，也不要求独立部署。
- **必须满足的不变量：** Gateway 是唯一用户交互后端边界；不拥有 Session 真相，不直接写 SessionStore，不实现模型/工具循环，不复制 AgentRuntime 状态。它只完成调用校验、身份与目标路由、结构化命令分发、Snapshot/事件投影和流式/聚合呈现。AgentRuntime 只向框架事件通道发布事实，不反向依赖具体 Gateway。
- **否决的方案及原因：** 让 Entrypoint 直接调用 Runtime 会使每种 UI 重复实现 revision、幂等、路由和错误规则；把 Gateway 当成可选网络转发服务无法保护 Agent 与 UI 的边界；每 Session 一个 Gateway 会重复交互状态并把连接生命周期绑定到会话；让 Gateway 直接承载业务真相会形成超级对象。
- **对接口、存储、Gateway 和实现的影响：** RuntimeAccess 只在框架内部由固定 Gateway 调用；Entrypoint 只获得 GatewayAccess。TUI 可以进程内直接调用，HTTP/WebSocket/ACP 等通过传输适配器调用同一接口。Gateway 不拥有 RuntimeRegistry，目标定位仍由 RuntimeCoordinator 完成。
- **状态：** 已确定。

### A-009 完整路由身份

- **问题 / 背景：** 只凭 SessionID 或 RunID 无法在多 Agent、多 Workspace 部署中稳定定位目标。
- **最终决定：** 已创建会话和运行的路由使用 `AgentID + WorkspaceID + SessionID + RunID`；创建 Session 的 RPC 在返回 SessionID 前使用 AgentID 与 WorkspaceID。
- **必须满足的不变量：** 服务端验证四者归属关系，不能只做字符串转发；RunID 只在其 Session 内有效。
- **否决的方案及原因：** 仅使用内存 Loop 引用或单个 ID 会让越权检查、重连和横向扩展失去依据。
- **对接口、存储、Gateway 和实现的影响：** Gateway 调用、事件信封、日志和鉴权上下文携带稳定路由键；具体传输适配器必须无损映射这些字段。
- **状态：** 已确定。

### A-010 Gateway 核心与承载协议无关

- **问题 / 背景：** 同一 Gateway 既要支持进程内 TUI/函数调用，也要支持跨进程 Web、桌面端或外部客户端，不能把网络形态写进核心合同。
- **最终决定：** Gateway 暴露与传输协议无关（carrier-neutral）的结构化 Go 调用、结果、Snapshot 和事件合同。进程内 Entrypoint 直接调用；只有跨进程适配器才把它映射为 RPC。具体 wire protocol 由适配器决定。
- **必须满足的不变量：** 每个调用都有明确结果或结构化错误；流事件有稳定信封；直接调用和远程调用观察到相同业务语义；传输对象不能进入 Runtime 领域命令。
- **否决的方案及原因：** 强制 Gateway 内外两端都是 RPC，会让进程内调用承担无意义的序列化和网络模型；让远程 UI 直接持有 Runtime 指针则无法跨进程并暴露同步细节。
- **对接口、存储、Gateway 和实现的影响：** Gateway 核心合同不出现 HTTP、WebSocket、gRPC、socket 或响应码；这些映射属于 `interaction.entrypoint`、`gateway.transport` 和相关适配器。wire protocol 仍可分别评审，但不再决定 Gateway 的内部边界。
- **状态：** 已确定核心合同；各传输 wire protocol 待商榷。

### A-011 断开不取消 Run

- **问题 / 背景：** 客户端网络断开不代表用户要求停止长任务。
- **最终决定：** 连接断开不取消 Run；客户端可重新连接并恢复可观察状态。
- **必须满足的不变量：** Run 生命周期由领域命令和安全限制控制；连接生命周期不能隐式改变执行状态。
- **否决的方案及原因：** 把 socket 断开映射为 Cancel 会让移动网络和页面刷新造成意外任务终止。
- **对接口、存储、Gateway 和实现的影响：** Gateway 提供 Snapshot 和后续事件订阅；Cancel 必须是显式 Gateway 命令。远程适配器负责把连接重建映射到这两项能力。
- **状态：** 已确定。

### A-012 `Send/Steer` 返回持久化 `MessageID`

- **问题 / 背景：** 客户端需要引用已提交输入，但不能依赖内存 Run 对象。
- **最终决定：** `Send` 和 `Steer` 成功后返回持久化 `MessageID`；Run 使用稳定 `RunID` 表示。
- **必须满足的不变量：** 返回成功意味着消息已持久化；相同幂等键不能制造重复消息。
- **否决的方案及原因：** 暴露 `RunHandle` 或 Runtime 指针无法跨重启和 RPC，并泄漏内部同步模型。
- **对接口、存储、Gateway 和实现的影响：** Gateway 返回 ID 与 revision；后续编辑、删除、取消均使用稳定 ID。
- **状态：** 已确定。

### A-013 内部唯一执行模式是流式

- **问题 / 背景：** 同时维护流式和非流式两套执行路径会产生行为分叉。
- **最终决定：** AgentRuntime 和 ModelExecutor 内部只采用流式事件执行；非流式由 Gateway 聚合包装。
- **必须满足的不变量：** 两种客户端模式观察到相同的最终持久化事实和错误语义。
- **否决的方案及原因：** 两套执行路径容易在取消、工具调用、重试和消息边界上不一致。
- **对接口、存储、Gateway 和实现的影响：** Gateway 提供 stream 和 aggregate 两种呈现；Runtime 只发布一套事件。
- **状态：** 已确定。

### A-014 非流式返回全部 assistant 文本消息

- **问题 / 背景：** 一次 Run 可能在工具调用前后产生多条 assistant 文本。
- **最终决定：** 非流式结果按顺序返回本次 Run 产生的全部 assistant 文本消息，并保留消息边界。
- **必须满足的不变量：** 不能只取最后一条，也不能无损失地假定所有文本可拼成一条。
- **否决的方案及原因：** 只返回最终文本会丢失同一执行中已经提交的用户可见输出。
- **对接口、存储、Gateway 和实现的影响：** 聚合响应是消息数组，并包含 RunID 与最终状态。
- **状态：** 已确定。

### A-015 重连基于 Snapshot 和 revision

- **问题 / 背景：** 临时 chunk 不持久化，客户端仍需要在重连后恢复一致界面。
- **最终决定：** 重连时读取 Session Snapshot 和持久化 revision；AgentSlot 不保存临时 chunk 游标，也不保存框架级客户端 ACK 或消费游标。
- **必须满足的不变量：** Snapshot 只包含已提交事实和当前状态；客户端以 revision 去重和替换本地临时内容；传输回执不能改变 History、Context、Run 或业务完成状态。实时订阅缓存必须有内存安全边界，落后时明确 overflow 并要求重连，不能静默丢失持久事实或无界增长。
- **否决的方案及原因：** 把每客户端 ACK 或游标放进 SessionStore，会把展示与传输状态变成核心业务状态，并带来客户端身份、租期和无界清理问题。
- **对接口、存储、Gateway 和实现的影响：** Gateway 提供 Snapshot 查询和后续事件流；Subscribe 只接受刚取得的当前 revision，两步之间发生提交则返回 conflict 并重新拉 Snapshot。订阅断开或 overflow 不取消 Run。具体传输适配器或外部消息系统可以自行实现可靠投递，但不新增标准 ACK Slot。
- **状态：** 已确定。

## 5. Session 的三个业务视图

### A-016 Session 明确分为 History、Context、Queue

- **问题 / 背景：** 已发生事实、模型当前输入和待处理消息有不同的修改规则。
- **最终决定：** Session 对外明确提供 History、Context、Queue 三个业务视图。
- **必须满足的不变量：** 同一条数据处于哪个视图必须可判断；跨视图迁移由原子提交完成。
- **否决的方案及原因：** 用一个可变 messages 数组承载三种语义，会让压缩、编辑和审计互相破坏。
- **对接口、存储、Gateway 和实现的影响：** Session API、Snapshot 和事件都区分三类视图。
- **状态：** 已确定。

### A-017 History 严格 append-only

- **问题 / 背景：** History 是模型交互和工具事实的审计来源。
- **最终决定：** History 是唯一的会话事实账本，按真实发生顺序保存全部已提交模型交互和工具事实，并严格只追加。
- **必须满足的不变量：** 已提交项不能修改、删除、换位或向前插入；批量追加原子且可幂等重试；History 不以“可直接发送给某个 Provider”为合法性标准。
- **否决的方案及原因：** 压缩时改写或删除旧 History 会失去恢复、审计和重新派生 Context 的依据。
- **对接口、存储、Gateway 和实现的影响：** Store 提供尾 revision/CAS 和批量 append；更正只能追加新事实；完整 tool call 一旦产生就立即成为 History 事实。
- **状态：** 已确定。

### A-018 Context 是版本化派生视图

- **问题 / 背景：** 模型输入需要压缩，但压缩不应篡改事实。
- **最终决定：** Context 是从 History、Queue 消费结果和配置派生、且满足所选模型协议的版本化视图；压缩生成新版本。
- **必须满足的不变量：** 每次模型调用绑定明确 ContextVersion；旧 History 保持不变；未配对 tool call 不进入下一次模型请求。
- **否决的方案及原因：** 直接裁剪 History 会把模型预算优化变成数据丢失。
- **对接口、存储、Gateway 和实现的影响：** Context 记录来源 revision、压缩元数据和必要协议尾部。
- **状态：** 已确定。

### A-019 Queue 保存未进入 Context 的消息

- **问题 / 背景：** 正在运行、暂停或离线时到达的输入不能直接篡改当前 Context。
- **最终决定：** Queue 持久化尚未进入 Context 的 `normal`、`steer` 和 `held` 消息。
- **必须满足的不变量：** 入队成功后可重启恢复；被认领前不出现在模型 Context 中。
- **否决的方案及原因：** 仅使用内存 inbox 会在崩溃时丢消息，并无法支持可靠编辑和重连。
- **对接口、存储、Gateway 和实现的影响：** Queue 是 Session 持久化状态，Gateway 展示其投递类型和 revision。
- **状态：** 已确定。

### A-020 Queue 消息认领前可修改

- **问题 / 背景：** 用户可能在消息尚未被模型读取前修正内容或投递方式。
- **最终决定：** Queue 消息进入 Context 前允许编辑、删除，以及在 `normal`、`steer`、`held` 间改变投递方式。
- **必须满足的不变量：** 一旦认领或进入 Context 就不可原地修改；修改行为必须产生新 revision。
- **否决的方案及原因：** 修改已经进入 Context 的消息会让实际模型输入与持久化事实不一致。
- **对接口、存储、Gateway 和实现的影响：** Queue API 提供 edit、delete、reclassify、claim 和 consume；认领记录目标 RunID，只有该 Run 在把输入纳入 Context 或完成其他持久化处理的同一事务中才能消费移除，并明确 conflict 错误。
- **状态：** 已确定。

### A-021 Queue 操作使用 expected revision/CAS

- **问题 / 背景：** Gateway、用户操作和 AgentRuntime 可能同时修改 Queue。
- **最终决定：** 所有 Queue 变更携带 expected revision 并通过 CAS 提交。
- **必须满足的不变量：** 认领后的修改返回 conflict；调用方不得静默覆盖更新。
- **否决的方案及原因：** 后写覆盖会误删已被 Runtime 认领的输入或篡改投递顺序。
- **对接口、存储、Gateway 和实现的影响：** Gateway 返回最新 revision；远程适配器必须无损传递；客户端冲突后刷新 Snapshot。
- **状态：** 已确定。

### A-022 Steer 在下一安全 step 批量优先消费

- **问题 / 背景：** Steer 要尽快影响当前 Run，又不能插入半个模型响应或半个工具提交。
- **最终决定：** 运行中的 Steer 在下一个安全 step 边界按批次优先进入 Context；normal 等待下一 Run。
- **必须满足的不变量：** 不拆分原子工具批次；同一批 Steer 保持稳定顺序；一次认领原子完成。
- **否决的方案及原因：** 任意时刻注入会产生无效协议序列；把 Steer 当 normal 会失去及时纠偏能力。
- **对接口、存储、Gateway 和实现的影响：** AgentRuntime 在 step 边界检查 Queue；事件标明认领批次和目标 Run。
- **状态：** 已确定。

### A-023 正常完成可自动消费，异常停止不自动消费

- **问题 / 背景：** Queue 自动推进需要清楚区分正常结束和不确定状态。
- **最终决定：** Run 正常完成后可以在同一原子边界按 FIFO 启动下一条 normal；取消、错误或进程重启后 Session 回到 `idle`，但不得自动消费旧 Queue。
- **必须满足的不变量：** 异常边界之后旧 Queue 保持不变；只有新的 `Send` 或显式 `RunPending` 才能重新启动执行。
- **否决的方案及原因：** 异常后自动继续可能在未知副作用或坏 Context 上扩大损失。
- **对接口、存储、Gateway 和实现的影响：** Session 执行状态只需要 `idle/running`；正常 drain 与异常停止都必须和 Run 终态提交绑定。
- **状态：** 已确定。

### A-024 Send 和 RunPending 是两种显式启动方式

- **问题 / 背景：** 异常停止后，用户可能通过新输入继续，也可能要求在没有新输入时恢复未完成工作。
- **最终决定：** Runtime 在 `idle` 时收到新 `Send` 会持久化消息并立即启动执行；`RunPending` 用于没有新消息时显式继续可恢复工作或旧 Queue。`ResumeSession` 只表示从存储恢复 Session 并创建 Runtime。
- **必须满足的不变量：** Send 和 RunPending 都可审计并使用 expected revision；不能因 Session 中存在旧 Queue 就自行唤醒；Steer 不能替代启动命令。
- **否决的方案及原因：** 保留独立 `paused` 状态会让“是否正在执行”和“是否允许自动 drain”混成一个持久状态；复用 Resume 同时表达会话恢复和执行继续会造成歧义。
- **对接口、存储、Gateway 和实现的影响：** Send、RunPending 属于 Runtime 命令；二者都通过 SessionStore 原子认领 Run 执行权，不创建第二个循环对象。
- **状态：** 已确定。

### A-025 重启后的未消费 Steer 进入 held

- **问题 / 背景：** Steer 原本针对旧 Run 的下一 step，重启后不能假定仍适用。
- **最终决定：** 恢复时未消费 Steer 转为 `held`，由 Gateway 展示并允许用户编辑、删除或重新投递。
- **必须满足的不变量：** held 不被自动消费；原 MessageID 和来源 Run 可追踪。
- **否决的方案及原因：** 自动把旧 Steer 注入新 Run 可能改变用户已经无法预期的上下文。
- **对接口、存储、Gateway 和实现的影响：** 恢复事务执行类型迁移；Snapshot 明确 held 原因。
- **状态：** 已确定。

### A-026 Session 聚合中的持久化职责分离

- **问题 / 背景：** SessionStore、History、Context、Queue、RunJournal 和 SessionModelConfig 不是同义词。
- **最终决定：** SessionStore 负责 Session 聚合状态、revision 和原子提交；History 保存唯一、有序的会话事实；Context 保存合法的版本化模型输入；Queue 保存未消费输入；RunJournal 只保存执行恢复状态和证据；SessionModelConfig 保存当前 Session 可显式修改的模型配置。
- **必须满足的不变量：** 领域上分责，物理上可共用一个数据库事务；任何实现都必须提供相同原子边界；RunJournal 不能成为第二份对话事实账本。
- **否决的方案及原因：** 把全部数据都叫 History 会掩盖可变性、保留期限和恢复规则的差异。
- **对接口、存储、Gateway 和实现的影响：** `session.store` 是标准 `One` Slot；接口可分层，但跨视图提交协调必须由这个聚合存储合同完成。
- **状态：** 已确定。

### A-027 默认 Context 压缩结构

- **问题 / 背景：** 长 Session 必须在 Token 预算内保留目标和协议完整性。
- **最终决定：** `context.compactor` 是唯一可替换的压缩 Slot，不新增 `CompactionPolicy` Slot。框架只固定输入输出、来源 revision、版本和协议完整性；默认实现的压缩结果由历史执行摘要、最近三条 inbound 意图和必要协议尾部组成。
- **必须满足的不变量：** Compactor 输入当前完整 Context，输出压缩后的会话 Message 列表，不包含 SystemPrompt 和 Tool 定义；它不修改 History、不保存 ContextVersion、不提交 Session 事务。AgentRuntime 重新装配固定部分，并验证模型协议完整性与硬 Token 上限。
- **否决的方案及原因：** 只保留最近若干原始消息容易丢失长期目标；只保留摘要容易丢失近期精确意图。
- **对接口、存储、Gateway 和实现的影响：** 开发者可配置默认实现或整体替换 `context.compactor`；“摘要 + 三条 inbound + 协议尾部”不是其他实现的合规要求。
- **状态：** 高层契约已确定；默认算法属于可替换实现。

### A-028 “最近三条 inbound”范围

- **问题 / 背景：** inbound 不只来自当前人类的一种消息类型。
- **最终决定：** 默认 ContextCompactor 的最近三条包括 normal、steer，以及人类或其他被授权 Session 来源的输入意图。
- **必须满足的不变量：** 按被 Session 接受的稳定顺序选择；系统内部事件和 assistant 输出不计入三条。
- **否决的方案及原因：** 只数 human normal 会漏掉 sub-agent 协作和用户纠偏。
- **对接口、存储、Gateway 和实现的影响：** Message 元数据必须表达来源主体和投递类型；替换实现可以采用其他选择规则。
- **状态：** 默认实现行为已确定，不是框架强制算法。

### A-029 摘要模型与阈值

- **问题 / 背景：** 压缩模型和触发阈值若含糊，会导致各实现行为不可比较。
- **最终决定：** 默认 ContextCompactor 使用当前 Session 配置的模型生成摘要；外部配置的比例或预算最终解析成确定 Token 数。替换实现可以选择其他摘要算法或模型。
- **必须满足的不变量：** 一次压缩记录来源 Context revision；AgentRuntime 在调用模型前能够检查明确的硬 Token 上限。
- **否决的方案及原因：** 在核心中硬编码某个便宜模型会引入 Provider 偏好；运行中反复解释比例会产生漂移。
- **对接口、存储、Gateway 和实现的影响：** 默认实现的配置加载阶段解析阈值，并通过 ModelExecutor 调用当前模型；框架接口不硬编码摘要模型。
- **状态：** 框架校验边界已确定；摘要选择属于可替换实现。

### A-030 压缩后可查询完整 History

- **问题 / 背景：** 模型在摘要不足时仍可能需要精确历史事实。
- **最终决定：** 提供标准 Session History 工具，让模型分页查询被压缩掉的完整 History。
- **必须满足的不变量：** 工具只读、受权限和配额约束、结果可追溯到 History revision。
- **否决的方案及原因：** 把全部 History 永久塞回 Context 会抵消压缩；让模型直接访问数据库会绕过边界。
- **对接口、存储、Gateway 和实现的影响：** 需要一个标准只读工具能力；最终 Slot ID 待商榷。
- **状态：** 行为已确定；Slot ID 待商榷。

## 6. Provider、Model 与配置稳定性

### A-031 Provider 配置与 Model 配置分层

- **问题 / 背景：** 连接凭据与具体模型能力的变化频率和复用关系不同。
- **最终决定：** Provider 配置保存协议适配器、BaseURL、CredentialRef 等连接信息；Model 条目引用 Provider，并保存模型能力与限制。配置来源由最终产品决定。
- **必须满足的不变量：** 核心不读取特定配置文件或密钥；Model 不复制明文凭据。
- **否决的方案及原因：** 把每个模型做成完整 Provider 配置会重复连接信息；让核心决定配置来源会污染通用层。
- **对接口、存储、Gateway 和实现的影响：** Agent 配置只保存新 Session 的默认模型选择；SessionStore 持久化每个 Session 当前的 SessionModelConfig。Provider 连接配置继续留在 Provider/Executor 组件内，而不是把地址、凭据或文件路径写入 Session。
- **状态：** 已确定。

### A-032 Model 条目表达协议、模态、限制和 Reasoning

- **问题 / 背景：** 同一 Provider 可承载不同协议和能力，不能只保存一个模型字符串。
- **最终决定：** 每个 Model 条目选择协议、支持模态、Context/Input/Output 限制，以及该模型支持的 Reasoning 枚举和默认值。
- **必须满足的不变量：** 不向不支持的模型发送 Reasoning 值；限制在构建请求前可检查。
- **否决的方案及原因：** 任意字符串参数会把错误推迟到 Provider，并使跨模型配置不可验证。
- **对接口、存储、Gateway 和实现的影响：** SessionModelConfig 使用 ProviderKey、ModelID、Reasoning 和标准模型参数表达当前选择；ModelCatalog 提供有限稳定语义，Provider wire 参数留在适配器。
- **状态：** 已确定。

### A-033 Runtime 固定配置与 Session 模型配置分离

- **问题 / 背景：** SystemPrompt、工具集合和 Context 规则属于 Agent 定义，而用户需要在保留 Session 历史的前提下显式更换模型。
- **最终决定：** Agent 配置分成三部分：`AgentRuntimeConfig` 固定 `SystemPrompt`、`ToolKeys` 和 Context 配置；Agent 默认模型只用于初始化新 Session；`SessionModelConfig` 保存当前 Session 实际使用的 Provider、Model、Reasoning 和模型参数。
- **必须满足的不变量：** Runtime 固定配置在一个 AgentRuntime 生命周期内不变；SessionModelConfig 是 Session 聚合的持久状态，Resume、Close/Resume 和应用重启都不能用 Agent 默认值覆盖它。
- **否决的方案及原因：** 把模型继续放在 Runtime 不可变快照中，会迫使用户关闭并恢复 Runtime 才能切换；把 SystemPrompt、ToolKeys 也变成 Session 热配置，则会扩大可变面并破坏 Agent 身份和工具边界。
- **对接口、存储、Gateway 和实现的影响：** CreateSession 从 Agent 默认值初始化 SessionModelConfig；ResumeSession 从 SessionStore 恢复；完整 fork、摘要启动和 sub-agent Session 默认继承来源 Session 当前配置，但允许创建命令显式覆盖。
- **状态：** 已确定。

### A-034 SessionModelConfig 只能在 idle 时显式更新

- **问题 / 背景：** 已有历史的 Session 需要切换模型，但运行到一半更换会让同一 Run 前后由不同模型决策。
- **最终决定：** 固定 AgentRuntime 提供读取和更新 SessionModelConfig 的后端命令。更新只允许在 `idle`；`running` 返回 `active_run` 且不自动取消，调用方必须先显式 `Cancel` 并等待 `WhenIdle`。
- **必须满足的不变量：** 每个 Run 启动时冻结一份 SessionModelConfig 快照，该 Run 的全部模型 step 使用同一版本；更新使用 expected Session revision/CAS 原子提交，并产生不进入模型 Context 的 `ModelConfigChanged` Session 事件。
- **否决的方案及原因：** 运行中从下一 step 热切换会改变当前 Run 的决策主体；入口自动 Cancel 会把展示行为变成有破坏性的隐式控制；只保存在内存会使恢复后的模型悄悄回退。
- **对接口、存储、Gateway 和实现的影响：** `AgentRuntime` 增加 `ModelConfig` 和 `UpdateModelConfig`；每个 Run、模型请求和完整 assistant 结果记录实际配置版本。Session 的显式选择优先，ModelSelector 可以校验、解析或拒绝，但不能无提示替换成另一模型。Runtime 固定配置更新仍只影响后来创建的 Runtime，Agent 默认模型更新只影响新 Session。
- **状态：** 已确定。

### A-035 ModelExecutor 统一模型调用和网络重试

- **问题 / 背景：** Provider 协议转换和瞬时网络故障不应散落在 Runtime 状态机中。
- **最终决定：** AgentRuntime 发起一次逻辑模型调用；`model.executor` 是公共 `One` Slot，负责背后一次或多次真实 Provider 请求、Provider 适配，以及重试、原生续传或终止决定。
- **必须满足的不变量：** Runtime 只消费临时输出、`reset`、完整结果和最终失败等标准事件，不理解 Provider 差异；每次真实请求有唯一 AttemptID；只有完整模型结果进入 Session History。
- **否决的方案及原因：** Runtime 自己适配 Provider 会复制协议逻辑并产生不同重试语义；把 `model.provider` 全局设为必需会排斥不通过本地 Provider 注册表实现的 Executor。
- **对接口、存储、Gateway 和实现的影响：** 标准 Profile 必需一个 ModelExecutor；`model.provider` 改为可选 `Many`，由具体 Executor 显式声明依赖；AttemptID 和用量进入运维事件而非 Session History。
- **状态：** 职责已确定；数值默认值待商榷。

### A-036 半流失败由 ModelExecutor 恢复

- **问题 / 背景：** Provider 可能已经发出部分 chunk 后断网，这些 chunk 不是完整事实。
- **最终决定：** Provider 半流失败后，由 ModelExecutor 根据协议能力决定重试、原生续传或终止；需要撤销临时展示时向 Runtime 发出 `reset`。不再强制所有 Provider 使用相同 Context 从头重试。
- **必须满足的不变量：** 临时 chunk 不进入 History；已提交完整消息不回滚；最终失败结束当前 Run，使 Session 回到 `idle`，且不自动消费旧 Queue。
- **否决的方案及原因：** Runtime 猜测 Provider 恢复方式会把供应商协议污染进状态机；把半截持久化为 assistant message 会制造伪事实。
- **对接口、存储、Gateway 和实现的影响：** 流事件带 AttemptID；Gateway 能撤销对应临时展示；物理尝试的计费和观测由模型调用组件记录。
- **状态：** 已确定。

## 7. 工具循环、并发与崩溃恢复

### A-037 工具结果后必须继续调用模型

- **问题 / 背景：** 工具结果本身不是面向用户的自然完成结论。
- **最终决定：** 工具结果进入 Context 后必须继续调用模型，直到自然完成、取消或安全限制触发。
- **必须满足的不变量：** Runtime 不能把“执行完工具”当作 Run 成功结束；每轮都检查取消和上限。
- **否决的方案及原因：** 让实现自由决定是否继续会产生不可预测的半成品响应。
- **对接口、存储、Gateway 和实现的影响：** 状态机固定 model -> tool -> model 循环；安全上限配置待定。
- **状态：** 行为已确定；安全上限默认值待商榷。

### A-038 工具 call/result 在 History 中按事实先后追加

- **问题 / 背景：** History 要保存真实发生顺序，而模型 Context 又必须满足 call/result 配对协议。
- **最终决定：** 完整 tool call 产生后立即追加到 History；tool result 在执行完成后单独追加，不要求 call/result 原子成对写入 History。
- **必须满足的不变量：** 每个 ToolCallID 最终只能有一个终态结果：成功、结构化错误或 `outcome_unknown`；未配对 call 不进入下一次模型请求。
- **否决的方案及原因：** 为了让 History 看起来像 Provider 消息数组而延迟记录 call，会混淆事实账本与模型协议投影。
- **对接口、存储、Gateway 和实现的影响：** History 支持逐事实 append；Context 投影负责筛除未配对 call，直到对应终态结果存在。
- **状态：** 已确定。

### A-039 pending 工具意图进入 RunJournal

- **问题 / 背景：** 工具有外部副作用，执行前不留证据会在崩溃后无法判断是否已发生。
- **最终决定：** 工具执行前，在同一 Session 事务中把完整 call 追加到 History，并在 RunJournal 建立 pending 执行记录；事务成功后才允许执行工具。
- **必须满足的不变量：** Journal 记录 ToolCallID、Run/Step、执行状态和必要恢复证据，但不重复承载完整对话事实；pending 记录写入成功后才允许执行。
- **否决的方案及原因：** 完全不记录会诱发盲目重跑；只写 Journal 而不写 History 会隐去已经真实产生的模型调用事实。
- **对接口、存储、Gateway 和实现的影响：** SessionStore 的事务同时覆盖 History append 和 Journal pending；敏感执行证据按安全策略存储。
- **状态：** 已确定。

### A-040 崩溃恢复使用 `outcome_unknown`

- **问题 / 背景：** 进程可能在工具产生副作用后、结果提交前崩溃。
- **最终决定：** 恢复时不自动重跑未知调用；为已经存在于 History 的 call 追加唯一的结构化 `outcome_unknown` 结果，再允许后续模型执行。
- **必须满足的不变量：** 已确认完成的调用使用真实结果；未知调用不得伪装成功或失败；恢复事务结束当前 Run 并把 Session 置为 `idle`，不得自动消费旧 Queue。
- **否决的方案及原因：** 自动重跑可能重复付款、写文件或执行命令；丢弃调用会隐瞒真实风险。
- **对接口、存储、Gateway 和实现的影响：** `SessionStore.Recover` 在 Resume 边界扫描 RunJournal，原子追加 unknown 结果、结束旧 Run 并把遗留 Steer 转为 held；普通 `Load` 必须只读，不能把仍在正常运行的 Session 误判为崩溃。用户可用新 Send 或 RunPending 决定下一步。
- **状态：** 已确定。

### A-041 Tool 只声明两种调度模式

- **问题 / 背景：** 工具批次既要利用无依赖并发，也要保护有顺序要求的操作。
- **最终决定：** Tool 只声明 `ParallelSafe` 或 `Serial`；Runtime 不推测自然语言或工具名来决定并发。
- **必须满足的不变量：** Serial 工具按模型给出的稳定顺序执行；ParallelSafe 工具可同批并行；结果按原调用顺序归并。
- **否决的方案及原因：** 复杂锁域或关键字推断会增加不可验证策略，并把 Workspace 冲突错误归给 Runtime。
- **对接口、存储、Gateway 和实现的影响：** Tool 定义增加显式执行模式；ToolDispatcher 负责分组和结果归并。
- **状态：** 已确定。

### A-042 文件冲突使用乐观校验

- **问题 / 背景：** 不同 Session 可能同时修改同一 Workspace 文件。
- **最终决定：** 文件写入和精确编辑使用版本哈希或精确旧内容校验，不使用 Workspace 全局写锁。
- **必须满足的不变量：** 校验失败返回 conflict，不静默覆盖；调用参数包含模型已读取的版本证据。
- **否决的方案及原因：** Workspace 写锁会让无关文件操作互相阻塞，也无法处理进程外修改。
- **对接口、存储、Gateway 和实现的影响：** 官方文件工具定义 expectedHash/oldContent；冲突结果交给模型处理。
- **状态：** 已确定。

### A-043 工具错误作为安全结构化结果返回模型

- **问题 / 背景：** 参数错误通常需要模型修正，但内部错误可能包含凭据或基础设施细节。
- **最终决定：** 工具错误转换成安全、结构化、可行动的 ToolResult 交给模型；内部敏感错误只进入受控日志和追踪。
- **必须满足的不变量：** ToolCallID 保持配对；区分可修正错误、策略拒绝、冲突和内部失败；不得原样泄露秘密。
- **否决的方案及原因：** 遇到任意工具错误就终止 Run 会阻止模型自我修正；原样返回内部 error 会泄密。
- **对接口、存储、Gateway 和实现的影响：** 工具调度逻辑负责错误分类和净化，Runtime 随后继续调用模型。
- **状态：** 已确定。

## 8. Hook 与核心事务

### A-044 Hook 固定两个阶段

- **问题 / 背景：** 可扩展收尾行为需要 Hook，但任意阶段会让主循环不可推理。
- **最终决定：** 使用统一 `AgentHook` 注册模型，只开放 `BeforeRunComplete` 和 `AfterCommit` 两个固定阶段。`BeforeRunComplete` 在完整 assistant 消息已经保存、Run 尚未标记完成时运行；首版只能返回受控的“追加后续输入请求”。
- **必须满足的不变量：** Hook 不能直接修改 Queue、History、Context 或 Run 状态，也不能自行启动 step；AgentRuntime 校验并持久化请求，由它唯一决定继续下一 step 或完成 Run。Hook 报错只记录并忽略，其他 Hook 继续运行；`AfterCommit` 只观察已提交事实。
- **否决的方案及原因：** 为 Hook 提供暂停、取消或任意状态动作会制造第二个循环控制者；允许 AfterCommit 改事务会破坏一致性。
- **对接口、存储、Gateway 和实现的影响：** Hook 接收只读快照并返回受限 proposal；标准 `agent.hook` 是可选 `Chain` Slot；未来新增动作必须单独评审。
- **状态：** 已确定。

### A-045 Session 持久化属于核心事务

- **问题 / 背景：** 如果持久化依赖可选 Hook，关闭或失败 Hook 就会让 Agent 丢失真相。
- **最终决定：** History、Context、Queue、RunJournal 和 Session 状态提交由核心 Session 事务完成，不能委托给可选 Hook。
- **必须满足的不变量：** 先持久化再发布 AfterCommit；核心提交失败时不得宣称动作成功。
- **否决的方案及原因：** 用 Hook 保存 Session 会让正确性依赖安装顺序、Hook 可用性和外部副作用。
- **对接口、存储、Gateway 和实现的影响：** SessionStore 是 AgentRuntime 必需依赖；Hook 只做受控 proposal、通知、索引、遥测等派生工作。
- **状态：** 已确定。

## 9. Runtime 装配与交互命令

### A-046 RuntimeRegistry 随 Application Runtime 创建和销毁

- **问题 / 背景：** 同一 Session 的并发 create/resume 必须汇合到同一个 Runtime，而所有用户入口又必须经过同一个 Gateway，不能各自维护注册表或绕过交互边界。
- **最终决定：** Build 阶段只解析标准 Agent 所需的 typed Slot 并形成不可变 Assembly，不创建活跃注册表。`Application.Start` 创建应用级 Runtime 时，同时创建并持有唯一 RuntimeRegistry 和固定 Gateway，再构造操作注册表的 RuntimeCoordinator。Gateway 通过包内私有 RuntimeAccess 调用 Coordinator；所有 Entrypoint 只获得同一个 GatewayAccess。
- **必须满足的不变量：** 用户继续只使用统一的 `Build/Start/Run/Stop`；Registry 和 Gateway 不能早于应用级 Runtime 存在，也不能晚于它销毁；运行时不得扫描包、查询全局容器或保留 Build Resolver；初始化失败不得登记半成品 Runtime；Entrypoint 的启动和停止仍由提供它的 Module 生命周期负责。停止时先阻止 Entrypoint 接收新命令，再收束 AgentRuntime，最后关闭 Gateway/Entrypoint 连接和共享组件。
- **否决的方案及原因：** 每个 Entrypoint 自建注册表会产生重复 Runtime；向 Entrypoint 暴露 RuntimeAccess 会绕过 Gateway；公开 RuntimeFactory 会把固定循环伪装成替换点；让 Runtime 运行时从 Assembly 任意查询组件会形成服务定位器。
- **对接口、存储、Gateway 和实现的影响：** Build 创建尚未激活的 GatewayAccess 绑定和应用 Runtime 内部状态锚点；只有 Application Runtime 启动并创建 Registry、Coordinator 和 Gateway 后才能接受命令。Session 创建/恢复后，Coordinator 持有包内 `runtimeAccess` 窄接口并负责定位固定 AgentRuntime；Gateway 不接收或返回公开 Runtime 指针，对 Entrypoint 只返回稳定 ID、revision、snapshot 和事件。私有装配锚点不进入标准 Profile、组件地图或成熟度计分。
- **状态：** 已确定。

### A-047 交互命令只向固定 Gateway 注册，Slash 只是呈现协议

- **问题 / 背景：** `/model`、`/help` 等命令必须支持多种 UI，但不能让每个 Entrypoint 直接消费命令组件并各自实现一套后端语义。
- **最终决定：** `interaction.command` 保持可选 `Many[InteractionCommand]` Slot，Slot key 是稳定命令名；所有命令只注册到固定 Gateway。Gateway 暴露 UI-neutral 的命令目录、结构化输入、候选项、确认、执行结果和后续动作；TUI、Web、桌面端或 CLI 分别把它呈现为 Slash、命令面板、菜单、按钮或表单。
- **必须满足的不变量：** InteractionCommand 不解析 `/name`、HTTP 或其他 wire 文本，不渲染具体 UI；它只能调用 Gateway 提供的受控后端能力，不能直接访问 SessionStore、取得 RuntimeAccess、改变 Runtime 状态机或实现模型循环。可移植描述只使用有限、稳定的字段和交互词汇；产品专属复杂界面可以扩展前端，但最终操作仍必须提交给 Gateway。重复 key 在 Build 阶段失败。
- **否决的方案及原因：** `slash.command` 会把某一种 UI 语法提升为领域架构；让 Entrypoint 直接消费 InteractionCommand 会复制目录、权限、确认和执行规则；宣称任意命令都能自动生成任意 UI 会形成不可维护的万能 Schema。
- **对接口、存储、Gateway 和实现的影响：** `model` 命令可依赖 ModelCatalog 产生候选项，读取 SessionModelConfig，并在用户确认后通过 Gateway 调用 `UpdateModelConfig`。框架可以提供默认实现，但不把它硬编码进 AgentRuntime；Agent 项目可安装、替换或省略。Entrypoint 只负责把 Gateway 描述映射到具体 UI 或传输。
- **状态：** 已确定；`interaction.command` 已进入 Contracted。框架已提供显式安装的
  `model` 命令和函数式进程内 Entrypoint，验证 Slash、菜单和结构化调用共享同一个
  Gateway 后端；具体 Web/TUI 和跨进程协议仍由后续适配器实现。

### A-048 模型兼容性处理不得改写 Session 事实

- **问题 / 背景：** 旧 Session 可能包含新模型不支持的图片，或 Context 超过新模型限制；用户仍可能明确要求切换。
- **最终决定：** 未知 Provider/Model、非法 Reasoning 或非法参数直接拒绝。可能造成信息损失的模态或 Context 问题先返回结构化警告且不提交；调用方明确确认后可以保存新的 SessionModelConfig。
- **必须满足的不变量：** 强制确认不能删除或修改 History、Queue 和附件。文本模型面对图片时，模型请求不携带不支持的图片二进制，但 Context 保留稳定附件引用或省略说明；后续新图片仍正常接收和持久化。存在 OCR 等工具时，模型可通过附件引用调用工具处理。
- **否决的方案及原因：** 为适配新模型而删除图片会破坏 History append-only；拒绝任何已有图片的 Session 切换会把模型能力差异错误地变成数据锁定；静默丢弃会让用户误以为模型看过图片。
- **对接口、存储、Gateway 和实现的影响：** UpdateModelConfig 使用显式兼容性确认；ContextCompactor 先处理 Token 超限，压缩后仍超硬限制则在 Provider 调用前终止 Run，不偷偷截断事实。
- **状态：** 已确定。

### A-049 标准 Agent 必须显式进入，固定层自动挂载

- **问题 / 背景：** 通用装配核心必须保持产品中立，但标准 LLM Agent 又必须拥有统一的
  AgentRuntime、Gateway 和 Profile；不能让应用通过已安装 Slot 被框架隐式猜测。
- **最终决定：** 标准 Agent 使用独立 `standardagent.NewApplication` 入口。它返回与通用
  `agentslot.NewApplication` 相同的 `*agentslot.Application`，自动加入固定 Runtime/Gateway
  Module 和标准 Profile；通用 Application 不自动获得 Agent 能力。
- **必须满足的不变量：** 所有标准 Agent 使用相同的 Build、Start、Run、Stop 语义；导入
  `standardagent` 不产生注册副作用；产品只声明名称、Module、配置和额外 Profile 要求。
- **否决的方案及原因：** 让通用 Application 根据 Slot 推断 Agent 会制造隐藏装配和产品
  差异；增加 AgentHost、RunningApplication 或公开 RuntimeFactory 会产生第二套启动入口。
- **对接口、存储、Gateway 和实现的影响：** `standardagent` 只依赖通用核心和标准合同包；
  固定 Runtime/Gateway 由内部 Module 自动挂载，Agent 项目不能替换整个循环或 Gateway。
- **状态：** 已确定。

### A-050 标准包依赖必须保持单向 DAG

- **问题 / 背景：** Session、Gateway、Runtime 和模型合同互相引用时，容易把固定层或具体
  Provider 反向导入通用核心，最终无法独立替换和测试。
- **最终决定：** 分层从下到上固定为：通用 `agentslot` 核心 → 标准合同/稳定值类型 →
  `standardagent` 固定层 → 产品；Go import 箭头由上层指向下层，即产品/适配器 →
  `standardagent`/合同包 → `agentslot`。`agent` 值类型包不依赖固定 Runtime；合同包不依赖
  `standardagent`；适配器不被合同反向依赖。
- **必须满足的不变量：** 根包不导入 Agent、Provider、UI、存储或 Gateway；Entrypoint
  只能依赖 GatewayAccess 合同；固定层的内部包不能成为产品绕过边界的公共入口。
- **否决的方案及原因：** 在根包中直接放 AgentRuntime/Gateway 会污染通用核心；让 Session
  合同直接依赖 Runtime 会形成循环并把可替换责任锁死。
- **对接口、存储、Gateway 和实现的影响：** 先固定 `agent`、`session`、`model`、`tool`、
  `context`、`interaction` 等合同包的依赖方向，再实现公共类型和 Module；Provider/Store
  适配器只依赖合同及外部 SDK。
- **状态：** 已确定。

## 10. 外部评审问题（已讨论）

本节保留外部评审意见及本项目的处理结果。R-001～R-006 均已完成讨论，结论已经
写入受影响的 A 系列决策；这里不再形成一套平行规范。

### R-001 Hook 是否会成为第二个循环控制者

- **评审意见：** 当前 A-044 允许 `BeforeRunComplete` 追加 Steer 并继续当前 Run。若 Hook 既能决定是否继续，又能修改输入，主 Loop 和 Hook 可能形成双重循环控制者。建议 Hook 只能提出后续命令，由唯一的 Loop/Session owner 在事实提交后决定是否继续。
- **讨论结论：** 接受风险判断，并收窄 Hook 权限。首版 `BeforeRunComplete` 只能在完整 assistant 消息提交后返回“追加后续输入请求”；固定 AgentRuntime 校验、持久化并拥有唯一继续权。Hook 失败只记录并忽略，其他 Hook 继续；`AfterCommit` 纯观察。
- **对既有决策的处理：** 已修订 A-044；A-022 的用户 Steer 语义不变，Hook 请求不再被描述为可直接追加 Steer。
- **状态：** 已讨论并接受修改。

### R-002 History 是 canonical facts 还是模型协议投影

- **评审意见：** A-038 要求工具 call/result 成对写入 History，但必须先明确 History 是“真实发生事实的时间序列”，还是“可以直接发送给模型的合法协议序列”。前者可能要求已被 Provider 接受的 tool call 及时成为事实；后者才天然要求成对提交。
- **讨论结论：** 接受概念分离。History 是唯一事实账本；Context 是满足模型协议的派生投影。完整 tool call 立即写入 History，并在同一事务建立 RunJournal pending；result 后续单独追加。未配对 call 不进入模型请求。
- **对既有决策的处理：** 已修订 A-017、A-018、A-026、A-038～A-040，删除“History 必须原子成对”的旧结论。
- **状态：** 已讨论并接受修改。

### R-003 是否保存持久事实的最小客户端 ACK

- **评审意见：** 赞成不持久化临时 chunk，也不保存逐 chunk 游标；但可以考虑保存客户端最后确认的持久化 Message、revision 或 retirement。是否保存应交给 Gateway 根据可靠投递需求决定。
- **讨论结论：** 不接受把 ACK 提升为 AgentSlot 标准。框架既不保存临时 chunk 游标，也不保存客户端对持久事实的 ACK；重连继续使用客户端 revision 与 Session Snapshot。可靠投递可由具体传输适配器或外部消息系统私有实现。
- **对既有决策的处理：** 已补强 A-015；不新增 ACK Slot，不把 ACK 写入 SessionStore，传输回执不影响业务事实或完成状态。
- **状态：** 已讨论并否决标准化。

### R-004 默认压缩策略是否应成为固定架构语义

- **评审意见：** “历史摘要 + 最近三条 inbound + 协议尾部”适合作为默认策略，但不应成为 StandardAgentLoop 的固定语义。架构应只规定压缩输入、输出、来源 revision、协议完整性和版本要求；保留条数、摘要模型和选择规则应由可替换 Compaction Policy 配置。
- **讨论结论：** 接受默认算法不应成为核心语义，但不新增 `CompactionPolicy` Slot。继续使用唯一可替换的 `context.compactor`；框架固定输入输出、来源 revision、版本、协议完整性和硬 Token 上限，默认实现可配置或整体替换。
- **对既有决策的处理：** 已修订 A-027～A-029，把“摘要 + 最近三条 inbound + 协议尾部”和当前 Session 模型明确降为默认实现行为。
- **状态：** 已讨论并接受修改。

### R-005 半流失败是否必须使用相同 Context 重试

- **评审意见：** 不同 Provider 可能支持原请求重试、continuation，或只能终止，不能统一规定相同 Context 重新调用。建议 ModelExecutor 返回 typed recovery decision，并记录每次 physical attempt；Loop 只执行决定。
- **讨论结论：** 接受 Provider-specific recovery 不属于 Runtime。AgentRuntime 发起一次逻辑调用；ModelExecutor 自行管理物理尝试，并决定重试、原生续传或终止。每次真实请求有 AttemptID 和运维/用量记录，不进入 Session History。
- **对既有决策的处理：** 已修订 A-035、A-036，删除“所有 Provider 使用相同 Context 重试”的固定规则；重试次数和退避仍保留为待商榷数值。
- **状态：** 已讨论并接受修改。

### R-006 “每个打开的 Session 一个 Loop”的生命周期是否过重

- **评审意见：** 风险不一定是立即 Bug，主要是生命周期含糊：浏览或恢复 1,000 个 Session 是否会常驻 1,000 个 Loop、队列和取消对象；配置更新后旧 Loop 何时释放；并发打开同一 Session 是否会生成两个 Loop。建议改为“每个具有执行租约的活跃 Session 最多一个 Loop”，无活跃 Run 时 Session 的正确性不依赖 Loop 常驻。
- **讨论结论：** 接受“浏览不能创建执行对象”和“同 Session 必须单实例”的风险判断，但不采用每个 Run 创建临时 Loop。显式 CreateSession/ResumeSession 成功时创建轻量 AgentRuntime；Runtime idle 时常驻，昂贵的活跃 Run、取消和模型流资源只在 running 期间存在。
- **对既有决策的处理：** 已修订 A-002～A-004、A-006、A-023～A-024、A-033～A-034；删除独立 Loop 和持久 `paused`，Session 执行状态只保留 `idle/running`，`closed` 属于 Runtime 生命周期。并发 resume 汇合为同一 Runtime；Close/Resume 可以取得最新 Runtime 固定配置，但 SessionModelConfig 按持久状态恢复，也可在 idle 时显式修改。
- **状态：** 已讨论，接受问题但修改原建议。

## 11. 架构定案与实现门槛

架构已经完整定案，可以开始按实施计划开发全部目标系统。开发按阶段推进是 TDD 和
风险控制，不是把架构缩减为“第一版”；完整 AgentRuntime、固定 Gateway、Session
聚合、模型/工具循环、重连和所有扩展边界都已经在本文及全景架构中定义。

实现前仍需要用失败测试收敛具体 Go 方法名、错误包装和数据库字段，但这些细化不能
改变对象所有权、调用方向、状态机、事务边界或 Slot 扩展边界。当前代码中的
`Assembly.Describe()` 已完成目标名称和描述格式迁移；后续实现直接使用该 API。

后续实现必须遵守：

- 所有 Agent 项目继续使用现有 `Application.Build/Start/Run`，不增加产品专属启动方式；
- AgentRuntime 是框架固定对象，不是可替换 Slot；
- `CreateSession`/`ResumeSession` 返回成功时，对应 Runtime 已初始化完成；
- 启动后的 Application Runtime 持有进程内 RuntimeRegistry；其管理的全部
  AgentRuntime 位于同一进程，同一 SessionID 只有一个 Runtime；
- RuntimeCoordinator 只操作注册表，不拥有注册表；Runtime.Stop 必须关闭并清空它；
- Build 阶段只解析声明的 Slot 依赖并形成稳定依赖集合，运行时不使用服务定位；
- 固定 Gateway 是所有用户交互入口的唯一后端边界，不是 Slot，也不是独立部署要求；
- Entrypoint 只能调用 GatewayAccess，不能取得 RuntimeAccess、直接消费
  InteractionCommand 或自行实现模型/工具循环；
- InteractionCommand 只注册到 Gateway，并只提供 UI-neutral 描述和结构化执行；
- 进程内 Gateway 可以通过私有 RuntimeAccess 操作 Runtime；Entrypoint 只能取得稳定
  ID、revision、snapshot、命令结果和事件；包内私有装配 Slot 不属于公共扩展边界；
- 目标 Build 产物名称是 `Assembly`；当前实现已经完成迁移，不提供旧 `Plan` 别名。
- 标准 Agent 通过 `standardagent.NewApplication` 显式启用，通用 Application 不得隐式
  猜测 Profile；固定 Gateway 从应用运行骨架阶段开始就是所有测试和产品入口的唯一后端。

## 12. 实施参数与适配器选择

以下事项不再是架构缺口。它们的架构边界已经确定，只需要在实现和真实消费者中选择
具体参数或适配器；选择结果不能反向改变本文结论：

| 事项 | 当前边界 | 需要的决定或证据 |
| --- | --- | --- |
| Gateway 传输适配器协议 | 固定 Gateway 核心与承载无关；Entrypoint 必须通过 Gateway | 分别评审 HTTP/SSE、WebSocket、gRPC、ACP 或独立进程协议的错误、重连和版本化能力 |
| Queue 容量、背压和配额 | 只确定 Queue 必须持久化和 CAS | 给出按 Session、Workspace、Agent 的限制以及超限行为 |
| 模型重试和 Run 安全默认值 | 只确定必须重试可重试网络错误并有上限 | 确定次数、退避、最大 Step、最大工具调用和最大运行时间 |
| RunJournal、Session History Tool | RunJournal 属于 SessionStore 聚合；History 查询通过受授权的标准 Tool | 具体数据库查询和 Tool 实现 |
| 第一批真实 Provider 适配器 | 需要至少两个独立协议验证接口 | 根据真实消费者选择范围，不能只包两层同一 SDK |

## 13. 文档约束

- 本文记录架构结论；组件地图记录标准生态位和成熟度；实施计划记录代码顺序与验收。
- 新证据推翻已确定结论时，必须同时修改本文、实施计划和受影响的中英文组件地图。
- “已写出一个 AgentRuntime”不等于任何可替换 Slot 已 Proven；框架对象不参与 Slot 成熟度计分，组件成熟度规则也不能因进度压力降低。
