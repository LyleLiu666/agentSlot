# AgentRuntime 与标准 Slot 实施计划

## 1. 文档状态

本文把[Agent 设计的架构讨论](agent-architecture-discussion.zh-CN.md)和[标准 Agent
框架全景架构](agent-framework-architecture.zh-CN.md)转换成可执行的开发顺序。本文
覆盖完整目标架构；分阶段只是为了让每个边界先由测试证明，不代表框架只设计到某个
阶段，也不代表后续阶段可以另造一套运行模型。标准组件的实际成熟度以中英文组件地图
为准。

每一阶段都必须实现目标架构中对应的完整边界，并先用合同测试固定不可变的语义；未
完成的阶段是工程进度，不是架构缺口。接口批次通过评审后再进入实现，但不能因此
把固定 Gateway、AgentRuntime 或完整 Session 模型降级为“以后再设计”。

## 2. 目标与非目标

### 目标

- 所有标准 LLM Agent 使用框架固定的 `AgentRuntime` 和同一套循环不变量。
- 一个 Application Assembly 服务多个 Workspace 和 Session；不同 Session 可并行。
- 所有用户界面和外部调用都通过框架固定 Gateway 进入，不直接取得 AgentRuntime。
- Session 生命周期、持久化、模型执行、工具、Context 和 Hook 都有明确可替换边界。
- Agent 项目继续使用统一的 `Application.Build`、`Start`、`Run` 和 `Runtime.Stop`。
- 开发者能从组件地图判断哪些是框架规则、哪些是 Slot、哪些只是产品配置。

### 非目标

- 不把 `AgentRuntime`、循环状态机、事务不变量做成可替换 Slot。
- 不把固定 Gateway 做成可替换 Slot、网络服务或万能业务容器。
- 不新增 AgentHost、RunningApplication、公开 RuntimeFactory 或第二套启动容器。
- 不规定各 Gateway 传输适配器的 wire protocol、Provider 网络格式、数据库 Schema 或
  配置来源。
- 不把默认压缩算法、标准工具包或某个 Provider 适配器写成框架强制语义。
- 不通过反射扫描、`init()` 或隐藏服务定位器自动发现组件。

## 3. 术语与对象关系

```mermaid
flowchart TD
    A["Application：Build / Start / Run"] --> P["Assembly：共享组件装配结果"]
    A --> AR["启动后的应用级 Runtime"]
    AR --> REG["进程内 RuntimeRegistry"]
    AR --> RC["框架 RuntimeCoordinator"]
    AR --> G["固定 Gateway"]
    P --> RC
    P --> SM["SessionManager"]
    SM --> SS["SessionStore"]
    RC --> SM
    RC --> REG
    REG -->|"CreateSession / ResumeSession"| R["框架 AgentRuntime"]
    R --> AC["AgentRuntimeConfig 固定快照"]
    R --> S["Session"]
    S --> MC["SessionModelConfig 可持久化状态"]
    S --> H["History"]
    S --> C["Context"]
    S --> Q["Queue"]
    S --> J["RunJournal"]
    R --> ME["ModelExecutor"]
    R --> T["Tools"]
    R --> CX["ContextSource / ContextCompactor"]
    R --> HK["AgentHooks"]
    P --> CMD["InteractionCommand 组件"]
    E["Entrypoint / 传输适配器"] --> G
    CMD --> G
    G --> RC
```

| 对象 | 作用域 | 职责 |
| --- | --- | --- |
| Application | 进程内应用定义 | 装配模块，提供统一 Build、Start、Run 入口 |
| Assembly | Application 生命周期 | 保存经过校验的共享组件选择和启动顺序 |
| 应用级 Runtime | 一次 Application.Start | 由启动入口创建，持有进程内 RuntimeRegistry 和已启动 Module 生命周期 |
| RuntimeRegistry | 应用级 Runtime 生命周期 | 保存已 create/resume 的 `SessionID → AgentRuntime`；不是 Slot |
| RuntimeCoordinator | 应用级 Runtime 生命周期 | 操作 Registry 和固定生命周期命令，但不拥有 Registry |
| Gateway | 应用级 Runtime 生命周期 | 固定的进程内用户交互后端；所有 Entrypoint 的唯一 Agent 访问边界，不是 Slot |
| Entrypoint | Module 生命周期 | 把 TUI、Web、桌面端、CLI、ACP 或函数入口适配到 Gateway，不直接访问 Runtime |
| AgentRuntimeConfig | AgentRuntime 生命周期 | 固定 SystemPrompt、ToolKeys 和 Context 配置 |
| SessionModelConfig | Session 持久生命周期 | 保存当前 Provider、Model、Reasoning 和模型参数，可在 idle 时修改 |
| Agent 默认模型 | Agent 产品配置 | 只初始化新 Session，不覆盖已经存在的 SessionModelConfig |
| Session | 持久会话 | 拥有 History、Context、Queue、RunJournal 和 revision |
| AgentRuntime | 已恢复 Session 的内存生命周期 | 绑定一个 Session，接收命令并执行固定循环 |
| Run | Session 内一次执行 | 从开始持续到自然完成、取消或失败 |
| Step | Run 内执行边界 | 一次逻辑模型调用或一个工具批次 |
| ModelExecutor | Application 共享组件 | 完成逻辑模型调用并屏蔽 Provider 重试与续传差异 |

`CreateSession`、`ResumeSession` 是框架固定的 Session 生命周期入口，不是已经存在
的 `AgentRuntime` 实例方法，也不是 SessionManager 组件可以重写的循环入口。固定
入口由 Gateway 调用内部 RuntimeCoordinator；Coordinator 使用 SessionManager 创建或
恢复 Session、装配配置并初始化 Runtime，再以包内 `runtimeAccess` 操作它。Gateway 返回
成功时，对应 Runtime 已经可用，但不会把 Runtime 指针交给调用方。
Runtime 内部命令面提供 `Send`、`Steer`、`RunPending`、
`ModelConfig`、`UpdateModelConfig`、`Cancel`、`WhenIdle`、Queue 操作、查询和 `Close`。

这里的“启动程序持有注册表”不是要求产品的 `main` 函数手写一个 map，而是由统一的
`Application.Start` 在创建应用级 Runtime 时一并创建并持有 Registry。产品启动代码
只持有返回的应用级 Runtime，并通过它完成关闭；RuntimeCoordinator 是内部操作组件，
负责并发汇合和命令协调，但不是 Registry 的生命周期所有者。

一个启动后的应用级 Runtime 是长期确定的单进程执行边界：它登记的所有 AgentRuntime
都在同一进程。数据库中仅持久化、尚未 create/resume 的 Session 不进入 Registry，
也不占用 AgentRuntime。跨进程拆分同一 Application Runtime 不属于当前标准；若未来
引入 Session 跨进程所有权、租约或迁移，必须重新进行架构评审。

## 4. 框架固定边界与开发者扩展边界

| 层级 | 可否替换 | 内容 | 约束 |
| --- | --- | --- | --- |
| 装配框架 | 否 | Application、Module、typed Slot、Assembly、Build/Start/Run、生命周期回滚 | 通用核心保持小且稳定 |
| AgentRuntime | 否 | Session 绑定、命令串行化、循环、状态机、模型配置更新边界和事务顺序 | 不是 Slot，不允许实现另一套标准循环 |
| Gateway | 否 | 统一交互命令、路由、Snapshot、事件、流式/聚合呈现边界 | 不是 Slot；不得包含 Session 真相或模型/工具循环 |
| 正确性不变量 | 否 | 同 Session 唯一活跃 Run、History append-only、CAS、工具结果后继续模型、异常不自动消费旧 Queue | 不能被配置或 Hook 关闭 |
| 标准组件 Slot | 是 | SessionManager、SessionStore、ModelExecutor、Tool、ContextSource、ContextCompactor、AgentHook、Entrypoint、InteractionCommand 等 | 替换实现必须满足同一合同和兼容测试 |
| 默认实现 | 是 | 默认 Compactor、标准工具包、Provider 适配器、内存 Store | 默认算法不是所有实现的语义 |
| 产品配置 | 由项目决定 | Runtime 固定配置、Agent 默认模型、压缩参数、Provider 地址和凭据引用 | 固定配置在 Runtime 内使用快照；默认模型只初始化新 Session |
| Runtime 内部端口 | 否，除非以后证明为生态位 | Clock、ID 生成器、执行协调器、锁和调度细节 | 可用于测试注入，不自动升级成公共 Slot |

开发者如果确实要实现完全不同的循环，可以使用 AgentSlot 通用装配核心定义项目本地
Slot 和 Profile；它不属于标准 LLM Agent Profile，也不能冒充标准 AgentRuntime。

## 5. 标准 Slot 与依赖关系

### 5.1 标准 Profile 必需项

| Slot ID | 候选合同 | 类型 | 基数 | 直接消费者 |
| --- | --- | --- | --- | --- |
| `session.manager` | `SessionManager` | `One` | 恰好 1 | 框架 Runtime 创建逻辑 |
| `session.store` | `SessionStore` | `One` | 恰好 1 | SessionManager 和 AgentRuntime |
| `model.executor` | `ModelExecutor` | `One` | 恰好 1 | AgentRuntime |
| `interaction.entrypoint` | `Entrypoint` | `Many` | 至少 1 | 把外部调用方或 UI 适配到固定 Gateway |

### 5.2 与 Runtime 直接相关的可选项

| Slot ID | 候选合同 | 类型 | 说明 |
| --- | --- | --- | --- |
| `agent.hook` | `AgentHook` | `Chain` | 有序执行受控 Hook；缺失时循环正常运行 |
| `model.provider` | `ModelProvider` | `Many` | 仅在所选 ModelExecutor 需要时成为该模块的依赖 |
| `tool` | `Tool` | `Many` | 零工具对话 Agent 合法；ToolKeys 只能选择已安装键 |
| `context.source` | `ContextSource` | `Chain` | 按顺序提供模型调用的派生上下文 |
| `context.compactor` | `ContextCompactor` | `One` | 多轮 Profile 可要求；算法可整体替换 |
| `interaction.command` | `InteractionCommand` | `Many` | 只向固定 Gateway 注册 UI-neutral 命令；由 Entrypoint 映射为具体界面 |

其他策略、审批、观察、审计、用量和 `gateway.*` 适配组件 Slot 继续按组件地图定义。某个
ModelExecutor 模块如果需要 Provider，必须通过 `RequiredSlots` 显式声明
`model.provider` 依赖；标准 Profile 不再全局强制 Provider 数量。

## 6. 第一批 Go 合同

本节的第一批公共合同已经通过合同测试落地。它们固定职责、数据流、Slot ID、基数和
扩展权限，但不代表已有真实实现或 Conformant/Proven 成熟度。后续实现可以增加不破坏
这些边界的具体字段和适配器，不得绕过它们直接暴露 Runtime、Store 或 Provider。

### 6.1 固定 Gateway、GatewayAccess 与内部 RuntimeAccess

Gateway 是框架结构体，不是 Slot。下面的方法只表示稳定职责，不是已经发布的签名：

```go
type Gateway struct { /* framework-owned fields */ }

func (g *Gateway) ListSessions(context.Context, ListSessionsRequest) (SessionList, error)
func (g *Gateway) CreateSession(context.Context, CreateSessionRequest) (SessionOpened, error)
func (g *Gateway) ResumeSession(context.Context, ResumeSessionRequest) (SessionOpened, error)
func (g *Gateway) ForkSession(context.Context, ForkSessionRequest) (SessionOpened, error)
func (g *Gateway) StartSessionFromSummary(context.Context, SummarySessionRequest) (SessionOpened, error)
func (g *Gateway) Send(context.Context, SendRequest) (EnqueueReceipt, error)
func (g *Gateway) SendAndWait(context.Context, SendRequest) (RunResult, error)
func (g *Gateway) Steer(context.Context, SteerRequest) (EnqueueReceipt, error)
func (g *Gateway) RunPending(context.Context, RunPendingRequest) (RunReceipt, error)
func (g *Gateway) Cancel(context.Context, CancelRequest) error
func (g *Gateway) WhenIdle(context.Context, WhenIdleRequest) error
func (g *Gateway) EditQueued(context.Context, EditQueuedRequest) (CommitReceipt, error)
func (g *Gateway) DeleteQueued(context.Context, DeleteQueuedRequest) (CommitReceipt, error)
func (g *Gateway) ReclassifyQueued(context.Context, ReclassifyQueuedRequest) (CommitReceipt, error)
func (g *Gateway) ModelConfig(context.Context, ModelConfigRequest) (SessionModelConfigView, error)
func (g *Gateway) UpdateModelConfig(context.Context, UpdateModelConfigRequest) (ModelConfigReceipt, error)
func (g *Gateway) Snapshot(context.Context, SnapshotRequest) (SessionSnapshot, error)
func (g *Gateway) Subscribe(context.Context, SubscribeRequest) (EventStream, error)
func (g *Gateway) Commands(context.Context, CommandScope) ([]CommandDescriptor, error)
func (g *Gateway) InvokeCommand(context.Context, CommandInvocation) (CommandResult, error)
func (g *Gateway) CloseSession(context.Context, CloseSessionRequest) error
```

- `GatewayAccess` 是 Entrypoint 使用的固定、与传输协议无关（carrier-neutral）的调用面，
  不是 Slot；进程内
  适配器直接调用，跨进程适配器负责请求/响应和事件信封映射。
- Gateway 返回稳定 ID、revision、snapshot、receipt 和事件，不返回 `*AgentRuntime`。
- Gateway 负责统一校验、主体与目标路由、命令目录和调用结果投影；不直接写
  SessionStore，不实现循环，也不保存第二份 Session 真相。
- 包内私有 `RuntimeAccess` 是 RuntimeCoordinator 到固定 Session Runtime 的唯一操作
  通道；Gateway 只通过 Coordinator 取得该窄接口。Entrypoint、InteractionCommand 和
  产品 UI 都不能取得它或 `*AgentRuntime`。
- AgentRuntime 通过框架事件端口发布临时与持久事件；它不依赖 Gateway 具体类型。

### 6.2 SessionManager

```go
type SessionManager interface {
	Create(context.Context, CreateSessionCommand) (Session, error)
	Resume(context.Context, ResumeSessionCommand) (Session, error)
	Fork(context.Context, ForkSessionCommand) (Session, error)
	StartFromSummary(context.Context, SummarySessionCommand) (Session, error)
}
```

实际公共接口位于 `session`、`model`、`tool`、`context`、`hook` 和 `interaction` 包；
本节代码用于说明合同，不是另一个平行 API。

所有尚未持久化的用户输入、Steer、Queue 编辑、摘要启动输入和 Hook follow-on proposal
统一使用 `agent.MessageInput`，只携带 provider-neutral 内容块。调用方和 Hook 不能预先
指定 MessageID、SessionID、RunID、StepID、Role 或时间；固定 Runtime 在 Session 事务中
分配这些字段后才形成可进入 History 的 `agent.Message`。

- 负责创建、恢复和派生 Session，不执行 Agent 循环。
- Manager 的 `Resume` 必须完成恢复检查；不能把损坏或半恢复 Session 交给 Runtime。
- 同一应用级 Runtime 对同一 SessionID 的并发 resume 必须汇合为同一个
  AgentRuntime；注册表和单航班逻辑属于框架实现，不扩成公共 Factory Slot。
- Manager 依赖 `session.store`，但不能要求具体存储实现。

包内 `runtimeAccess` 不是 Slot，也不对产品代码导出。RuntimeCoordinator 通过它操作
固定 Session Runtime，Gateway 只向 Coordinator 提交结构化命令并接收数据结果：

```go
type runtimeAccess interface {
	id() SessionID
	revision() Revision
	snapshot(context.Context, SnapshotRequest) (SessionSnapshot, error)
	send(context.Context, SendRequest) (EnqueueReceipt, error)
	// 其余固定 Runtime 命令；不包含 Store 或任意服务定位能力。
}
```

Entrypoint 的提供方使用标准 Module 包装器，在 Build 构造阶段接收 GatewayAccess，
不能接收 RuntimeAccess。Entrypoint 的具体领域方法由合同测试收敛。启动监听器、停止
连接和资源清理由提供它的 Module 实现 `Lifecycle`，不把 `Start/Stop` 重复塞进
Entrypoint 接口。

### 6.3 SessionStore 与 Session

```go
type SessionStore interface {
	Create(context.Context, NewSession) (Snapshot, error)
	Load(context.Context, SessionRef) (Snapshot, error)
	Recover(context.Context, SessionRef) (Snapshot, error)
	Commit(context.Context, CommitRequest) (Commit, error)
}

type Session interface {
	ID() SessionID
	View(context.Context) (SessionView, error)
	Revision() Revision
}
```

`SessionStore.Commit` 是 Session 聚合的唯一持久化提交入口；`Recover` 只在
`ResumeSession` 的恢复边界执行崩溃收束，普通 `Load` 保持只读。它必须覆盖 History、
Context、Queue、RunJournal、SessionModelConfig 和执行状态之间需要一致的更新，支持
expected revision、CAS 和幂等键。`Session` 是已恢复会话的窄句柄，不暴露存储实现，
也不允许调用方绕过 AgentRuntime 随意改写聚合状态。

参考实现 `session.MemoryStore` 使用进程内原子聚合提交，`session.MemoryManager` 负责
默认模型、派生 Session 和稳定 ID 分配。它只用于合同验证和开发测试；生产实现可以
使用数据库、事件存储或其他持久化方案，但必须保留相同事务与恢复边界。

### 6.4 SessionModelConfig

```go
type SessionModelConfig struct {
	ProviderKey string
	ModelID     string
	Reasoning   Reasoning
	Parameters  ModelParameters
}

type UpdateModelConfigCommand struct {
	Config                  SessionModelConfig
	ExpectedRevision        Revision
	AcceptCompatibilityLoss bool
}
```

- AgentRuntimeConfig 的 SystemPrompt、ToolKeys 和 Context 配置在 Runtime 生命周期内固定。
- Agent 默认模型只用于 CreateSession；ResumeSession 必须从 SessionStore 恢复当前配置。
- fork、摘要启动和 sub-agent Session 默认继承来源 Session 当前配置，创建命令可以覆盖。
- SessionModelConfig 只保存模型选择和标准参数，不保存 BaseURL、CredentialRef 或明文凭据。
- 一次 Run 原子认领时冻结配置快照，后续所有模型 step 使用同一版本。
- 更新产生 `ModelConfigChanged` Session 事件，但不产生用户 Message，也不进入 Context。

### 6.5 ModelExecutor

```go
type ModelExecutor interface {
	Execute(context.Context, ModelRequest) (ModelStream, error)
	Inspect(context.Context, Config) (ExecutionCapabilities, error)
	CountTokens(context.Context, ModelRequest) (int, error)
}

type ModelStream interface {
	Recv(context.Context) (ModelEvent, error)
	Close() error
}
```

- Runtime 发起一次逻辑调用；Executor 可在内部执行多次真实 Provider 请求。
- 每次真实请求有独立 `AttemptID`，进入用量、Trace 和运维事件，不进入 Session History。
- Executor 统一产生临时输出、`reset`、完整结果或最终失败。
- 重试、原生 continuation 或终止由 Executor 根据 Provider 能力决定；Runtime 不猜测
  Provider 差异，也不强制“相同 Context 从头重试”。
- 只有完整模型结果可以成为 History 事实；半流 chunk 不持久化。

### 6.6 Tool 与工具调度

`Tool` 是可替换标准组件。每个 Tool 只声明 `ParallelSafe` 或 `Serial`；固定 Runtime
根据声明形成批次并调用工具。工具参数必须先通过其 Schema 校验。

工具错误形成安全、结构化的 tool result 并返回模型。内部堆栈、凭据和敏感路径不得
原样暴露。一个工具批次结束后 Runtime 必须再次调用模型，直至自然完成、取消、
最终模型失败或安全上限触发。

### 6.7 ContextSource 与 ContextCompactor

```go
type ContextSource interface {
	Contribute(context.Context, ContextInput) ([]model.Input, error)
}

type ContextCompactor interface {
	Compact(context.Context, CompactionInput) (CompactionOutput, error)
}
```

- Compactor 输入当前完整 Context 及来源 revision，输出较小的 `model.Input` 投影。
- 输出不包含 SystemPrompt 和 Tool 定义；Runtime 验证协议完整性与硬 Token 上限后重新
  装配固定部分并提交 ContextVersion。
- Compactor 不改写 History、不直接写 Store、不分配 ContextVersion。
- 规划中的产品默认实现可以采用“历史摘要 + 最近三条 inbound + 必要协议尾部”，但这
  不是框架强制算法。当前仓库只提供需显式安装的 `TailCompactor` 参考实现，Agent 项目
  可以配置或整体替换。

### 6.8 AgentHook

```go
type AgentHook interface {
	BeforeRunComplete(context.Context, RunCompleteView) (FollowOnProposal, error)
	AfterCommit(context.Context, CommitView) error
}
```

- `BeforeRunComplete` 在完整 assistant 消息已提交、Run 尚未结束时调用，只能提出追加
  后续输入；Runtime 校验并持久化 proposal，并且拥有是否继续的唯一决定权。
- Hook 不能直接修改 Queue、History、Context、RunJournal 或 Runtime 状态。
- Hook 报错只记录并继续执行其他 Hook；不能破坏核心事务。
- `AfterCommit` 只观察已提交事实，不能回滚或改变提交结果。
- 首版不提供暂停、取消或任意命令动作；未来扩展动作必须重新评审。

### 6.9 InteractionCommand

```go
type InteractionCommand interface {
	Describe() CommandDescriptor
	Invoke(context.Context, CommandInvocation, CommandActions) (CommandResult, error)
}
```

- `interaction.command` 是可选 `Many` Slot，Slot key 是稳定命令名；重复 key 在 Build
  阶段失败。
- Invocation 是 Gateway 已接收并完成主体、Session 和 revision 基础校验的结构化调用。
  InteractionCommand 不解析 `/name` 文本，不依赖 HTTP、WebSocket、ACP 等 wire 对象。
- CommandDescriptor 使用有限、稳定的 UI-neutral 词汇表达文本字段、单选/多选、布尔值、
  确认、候选项和结果；Entrypoint 决定渲染成 Slash、菜单、按钮、表单或命令面板。
- CommandActions 是 Gateway 提供的受控能力，不能暴露 `*AgentRuntime`、RuntimeAccess、
  SessionStore 或内部锁，也不能重新进入 `InvokeCommand` 形成命令递归。命令不能修改
  Runtime 内部状态或实现模型循环。
- 框架提供可显式安装的 `interaction.NewModelCommandModule`。其命令通过本次 Invocation
  绑定的 CommandActions 读取 SessionModelConfig 与 ModelCatalog 候选项，用户确认后再
  请求 `UpdateModelConfig`；它拿不到 Runtime、Store 或 Catalog 组件对象。默认实现属于
  可选组件包，不硬编码进 Runtime，也不会因 import 自动安装。自定义命令 Module 仍可
  对自己声明的标准 Slot 依赖使用普通 Build 构造注入。

### 6.10 AgentRuntime 的固定命令面

AgentRuntime 由框架实现为普通 Go 结构体。候选命令面如下：

```go
type AgentRuntime struct { /* framework-owned fields */ }

func (r *AgentRuntime) Send(context.Context, SendCommand) (EnqueueResult, error)
func (r *AgentRuntime) Steer(context.Context, SteerCommand) (EnqueueResult, error)
func (r *AgentRuntime) RunPending(context.Context, RunPendingCommand) (RunID, error)
func (r *AgentRuntime) Cancel(context.Context, CancelCommand) error
func (r *AgentRuntime) WhenIdle(context.Context) error
func (r *AgentRuntime) EditQueued(context.Context, EditQueuedCommand) (Commit, error)
func (r *AgentRuntime) DeleteQueued(context.Context, DeleteQueuedCommand) (Commit, error)
func (r *AgentRuntime) ReclassifyQueued(context.Context, ReclassifyQueuedCommand) (Commit, error)
func (r *AgentRuntime) ModelConfig(context.Context) (SessionModelConfigView, error)
func (r *AgentRuntime) UpdateModelConfig(context.Context, UpdateModelConfigCommand) (ModelConfigCommit, error)
func (r *AgentRuntime) Snapshot(context.Context) (RuntimeSnapshot, error)
func (r *AgentRuntime) SessionView(context.Context) (SessionView, error)
func (r *AgentRuntime) Close(context.Context) error
```

这里不定义 `AgentRuntime` 接口或 Slot，以免制造“标准循环可以整体替换”的错误承诺。
这些方法是 Gateway 经私有 RuntimeAccess 调用的框架内部命令面，不是 Entrypoint API。
为了测试而抽出的 Clock、ID 生成器或协调器保持包内小接口。

## 7. 初始化与注入

### 7.1 Application 级共享依赖

| 依赖 | 来源 | 是否为标准必需项 | Runtime 用法 |
| --- | --- | --- | --- |
| SessionManager | `session.manager` | 是 | 创建、恢复、fork Session |
| SessionStore | `session.store` | 是 | Session 聚合事务与 Snapshot |
| ModelExecutor | `model.executor` | 是 | 逻辑模型调用 |
| 固定 Gateway | 框架自动创建 | 是；不是 Slot | 用户交互唯一后端，调用 RuntimeAccess 并发布统一结果/事件 |
| Entrypoint | `interaction.entrypoint` | 至少一个 | 把具体 UI、函数或传输协议适配到 GatewayAccess |
| InteractionCommand 集合 | `interaction.command` | 否 | 注册到 Gateway，提供 UI-neutral 目录和结构化执行 |
| ModelCatalog 集合 | `model.catalog` | 否 | 为交互式模型选择提供候选项和能力说明 |
| ModelProvider 集合 | `model.provider` | 否 | 由特定 Executor 选择性依赖 |
| Tool 集合 | `tool` | 否 | 按 AgentRuntimeConfig.ToolKeys 冻结选择 |
| ContextSource 链 | `context.source` | 否 | 组装 Context |
| ContextCompactor | `context.compactor` | 否或由多轮 Profile 要求 | 压缩 Context |
| AgentHook 链 | `agent.hook` | 否 | 受控扩展与提交后观察 |

### 7.2 创建 Runtime 时的 Session 级输入

| 输入 | 来源 | 规则 |
| --- | --- | --- |
| AgentID、WorkspaceID、SessionID | Create/Resume 结果 | 稳定持久身份 |
| 已恢复 Session | SessionManager | 已完成恢复，不能是半成品 |
| AgentRuntimeConfig | 产品配置解析结果 | SystemPrompt、ToolKeys、Context 配置在 Runtime 生命周期内不可变 |
| SessionModelConfig | SessionStore | Create 时从默认值初始化，Resume 时恢复持久状态 |
| ModelExecutor 引用 | Assembly | 共享，不复制客户端或连接池 |
| 选中的 Tools | Assembly + ToolKeys | 键必须存在且无歧义 |
| Context 组件、Hooks | Assembly | 保持 Assembly 的稳定顺序 |

Runtime 初始化必须一次完成：任一必需依赖、ToolKey、SessionModelConfig 或 Session 恢复校验
失败时，`CreateSession`/`ResumeSession` 返回错误，不能缓存半成品 Runtime。

### 7.3 自动挂载链路

标准 Agent 的目标入口是 `standardagent.NewApplication`。它自动安装框架内部
Runtime/Gateway 模块，同时返回并继续使用统一 Application、Assembly 与 Runtime 层级，
不新增 AgentHost、RunningApplication 或公开 Factory。当前 Go 实现直接使用 `Assembly`
名称，不提供旧 `Plan` 别名。

1. 内部模块通过 `RequiredSlots` 声明 SessionManager、SessionStore、ModelExecutor、
   InteractionCommand 以及 Runtime/Gateway 所需可选组件，并贡献包内私有、尚未激活的
   GatewayAccess 绑定和应用 Runtime 状态锚点；`runtimeAccess` 只在 Session Runtime
   创建后存在，不伪装成 Build 期 Slot。
2. Build 期间使用受限 Resolver 一次性解析依赖，形成不可变 Runtime 依赖集合；Resolver
   关闭后不得保存或再次调用。
3. `Application.Start` 创建应用级 Runtime、Registry、RuntimeCoordinator 和固定 Gateway；
   Gateway 只通过 Coordinator 路由到包内 `runtimeAccess`，GatewayAccess 只绑定到
   Gateway 的公开交互面。
4. 标准 Entrypoint Module 包装器在构造 Entrypoint 时注入同一个 GatewayAccess；
   Entrypoint 不能取得 RuntimeAccess、Store、Executor、AgentRuntime 或内部锁。
5. Coordinator 内部 CreateSession/ResumeSession 创建或取得 `runtimeAccess`，GatewayAccess
   始终只返回稳定 ID、revision、snapshot、receipt 和事件；是否跨进程不改变该语义。
6. 依赖方向保证固定 Gateway 先于 Entrypoint Module 激活；Application 逆序停止时，
   Entrypoint Module 先停止监听并收束连接，应用级 Runtime 随后拒绝新调用、等待已经
   路由的调用结束、关闭全部 AgentRuntime 并清空 Registry，最后逆序停止共享组件。
   启动失败沿同一依赖顺序反向回滚。

包内 `runtimeAccess` 类型、GatewayAccess 绑定和应用 Runtime 状态锚点都不导出。Build
阶段可以把 GatewayAccess 绑定注入标准 Entrypoint Module 包装器，但在
`Application.Start` 绑定固定 Gateway 之前，任何命令都必须明确返回未启动错误。它们
都不进入标准 Profile、组件地图或成熟度计分，也不能被 Agent 开发者贡献或替换。

## 8. 生命周期与状态机

Session 执行状态只有 `idle`、`running`；内存 Runtime 另有 `closed` 终态。

```mermaid
stateDiagram-v2
    [*] --> idle: CreateSession / ResumeSession 完成
    idle --> idle: UpdateModelConfig 原子提交
    idle --> running: Send 或 RunPending 原子认领
    running --> running: Steer / 工具批次 / 下一模型 step
    running --> idle: 自然完成
    running --> idle: Cancel / 最终错误 / 恢复终止
    idle --> closed: Close
    running --> closed: Close 先取消并等待收束
    closed --> [*]
```

固定不变量：

- 一个启动后的应用级 Runtime 及其 RuntimeRegistry 只存在于一个进程；注册表中的所有
  AgentRuntime 都在该进程内。这是标准架构边界，不是第一版实现限制。
- 浏览或列出 Session 不创建 Runtime；显式 create/resume 才创建。
- 一个进程内同一 SessionID 最多一个 Runtime；并发 resume 返回同一个实例。
- Runtime 在 idle 时仍驻留，不做隐藏空闲回收；仅 `Close` 或应用停止释放。
- 同一 Session 同一时刻最多一个活跃 Run；不同 Session 可并行。
- `Close` 不删除持久 Session；再次 resume 可以创建新 Runtime，并使用最新
  AgentRuntimeConfig，但继续恢复该 Session 已持久化的 SessionModelConfig。
- AgentRuntimeConfig 在 Runtime 生命周期内固定；SessionModelConfig 可以在 idle 时显式更新。

## 9. 命令语义

### Send

1. 校验幂等键、权限、消息和 expected revision。
2. 以 `normal` 持久化到 Queue，并返回 `MessageID` 和新 revision。
3. 若 Runtime 为 idle，本次提交同时认领消息、创建 RunJournal 和 RunID，进入 running。
4. 若正在 running，消息保持 normal，等待当前 Run 自然完成后的 FIFO drain。

### Steer

1. 只允许针对当前活跃 Run；idle 返回 `no_active_run`，不偷偷降级为 normal。
2. 成功后持久化为 `steer` 并返回 `MessageID` 和 revision。
3. Runtime 在下一安全 step 边界按稳定顺序批量认领 Steer，优先于 normal。

### RunPending

用于没有新消息时显式处理异常停止后遗留的 Queue 或完成恢复动作。它不是 Session
恢复；Session 恢复只由 `ResumeSession` 表达。没有可处理工作时返回确定的
`nothing_pending`，不能创建空 Run。

### Queue 修改

Edit/Delete/Reclassify 必须携带 expected revision。仅未认领、尚未进入 Context 的
消息可修改；认领后返回 conflict。操作成功产生新 revision。

### ModelConfig 与 UpdateModelConfig

- `ModelConfig` 返回当前 SessionModelConfig、其 Session revision 和兼容性状态。
- `UpdateModelConfig` 只允许在 idle；running 返回 `active_run` 且不能隐式 Cancel。
- 调用方需要切换时必须先显式 `Cancel`，再等待 `WhenIdle` 成功。
- 更新携带 expected Session revision；CAS 冲突不修改任何状态。
- 未知 Provider/Model、非法 Reasoning 或非法参数直接失败，不能通过强制标记绕过。
- 模态或 Context 可能损失信息时，第一次返回结构化警告且不提交；只有显式接受兼容性
  损失的再次调用才保存配置。
- 成功事务同时更新 SessionModelConfig、推进 revision 并追加 `ModelConfigChanged`
  Session 事件；History、Queue 和附件保持原样。
- `/model`、菜单或按钮只是 Entrypoint 对 Gateway 中同一个 `model` InteractionCommand
  的呈现，不改变上述事务和错误语义。

### Cancel、WhenIdle、Close

- `Cancel` 只取消当前 Run；重复 Cancel 幂等。Run 收束后回到 idle，不自动消费旧 Queue。
- `WhenIdle` 等待 Runtime 到达 idle 或 closed；调用方 context 取消只结束等待。
- `Close` 拒绝新命令，取消当前 Run，等待核心事务收束并释放内存资源；Session 数据保留。

## 10. Session 数据模型与提交边界

### SessionModelConfig

SessionModelConfig 是 Session 聚合的版本化配置状态，不属于 History 或 Context。
CreateSession 从 Agent 默认模型初始化；ResumeSession 恢复持久值；fork、摘要启动和
sub-agent Session 默认继承来源 Session 当前值。每个 Run 的开始/终态 History 事实记录
认领时冻结的完整配置和来源 revision；ModelRequest 携带同一 ConfigRevision，因此后续
模型切换不能改写既有 Run 的配置证据。RunJournal 继续只承担未完成工具执行的恢复证据，
不复制 Run 配置事实。

### History

History 是唯一、严格 append-only 的事实账本，按真实发生顺序保存完整用户消息、
assistant 消息、tool call/result 和 Run 事实。SystemPrompt、Tool 定义和临时 chunk
不是 History。已提交事实不能修改、删除、换位或向前插入。

### Context

Context 是下一次模型调用使用的版本化合法协议投影。每个版本记录父版本、来源
History/Queue revision、Compactor 实现与配置版本和 Token 计量。未配对 tool call
不能进入模型请求。

切换到不支持图片的文本模型不会改写事实：文字部分继续进入 Context，图片二进制不
进入不支持它的模型请求，但投影保留稳定附件引用或明确的省略说明。后续图片仍正常
持久化；安装 OCR 等工具时，模型可以使用附件引用调用工具。Context 超过新模型限制
时先调用 ContextCompactor，压缩后仍超硬限制则在 Provider 请求前失败。

### Queue

Queue 保存未进入 Context 的 `normal`、`steer`、`held`。消息被认领前可以 CAS
编辑、删除或改投；claim 记录目标 RunID，只有该 Run 在持久化 Context 推进的事务中
才能 consume 移除。重启时未消费 steer 解除旧 Run 认领并转为 held，等待用户处理。

### RunJournal

RunJournal 保存活跃 Run、Step、待执行工具意图、Attempt 关联和恢复状态，不复制
History。它是执行恢复证据，不是第二份对话账本。

### 必须原子的事务

- Queue 入队、MessageID 分配、幂等结果和 revision 推进。
- idle 到 running：认领启动输入、创建 RunID、写 RunJournal 和状态转换。
- idle 下更新 SessionModelConfig、写 `ModelConfigChanged` 事件和 revision 推进。
- tool call 追加 History 与同一 ToolCallID 的 RunJournal pending 建立。
- 工具终态结果追加 History、Journal 终结和 Context 候选推进。
- Run 自然完成与下一 normal 的 FIFO 认领；异常终止不得顺带认领旧 Queue。
- ContextVersion 安装与来源 revision 校验。

每个 ToolCallID 最终只能有一个终态结果：成功、结构化错误或
`outcome_unknown`。崩溃恢复先追加 `outcome_unknown` 并终结 pending，不能自动
重跑可能已经产生外部副作用的工具。

## 11. 标准执行时序

```mermaid
sequenceDiagram
    participant E as Entrypoint
    participant G as Gateway
    participant R as AgentRuntime
    participant S as SessionStore
    participant M as ModelExecutor
    participant T as Tool

    E->>G: Send(structured request)
    G->>G: 校验主体、路由、revision 与幂等信息
    G->>R: Send(internal command)
    R->>S: 入队、冻结 SessionModelConfig 并原子认领 Run
    S-->>R: MessageID + RunID + config revision
    R->>S: 认领输入并安装 ContextVersion
    R->>M: Execute(logical request)
    M-->>R: 临时 chunk / reset
    R-->>G: 临时流式事件
    G-->>E: carrier-neutral event
    M-->>R: 完整模型结果
    alt 包含 tool calls
        R->>S: 原子追加完整响应事实 + calls + Journal pending
        R->>T: 按 Serial/ParallelSafe 执行
        T-->>R: 结构化 results
        R->>S: 追加 results + 终结 pending
        R->>M: 下一次逻辑调用
    else 自然完成
        R->>S: 追加完整 assistant 消息
        R->>R: BeforeRunComplete hooks
        R->>S: 提交完成或受控 follow-on
        R-->>G: 最终持久事件
        G-->>E: receipt / event / aggregate result
    end
```

临时 chunk 只用于当前连接和事件订阅，不持久化。delta/reset 事件携带 RunID、StepID
和 AttemptID；半流失败时 Executor 可以发出 `reset`，Entrypoint 据此撤销对应物理尝试
的临时投影。最终只有完整消息提交 History。单订阅者的进程内缓存有固定安全上限；
落后的订阅者收到明确 overflow 并按 Snapshot/revision 重连，不能用无界内存伪装可靠
投递。

## 12. Gateway、断线与重连

- Gateway 是 Application Runtime 持有的固定进程内对象，不是 Slot，不为每个 Session
  创建，也不要求独立部署。
- 所有 Entrypoint 都通过 GatewayAccess 接入；TUI/嵌入式调用可以直接使用 Go 调用，
  Web、桌面端远程模式、ACP 等通过各自适配器映射到相同合同。
- 只有 Gateway 消费 `interaction.command` 组件。Slash、菜单、按钮、表单和命令面板是
  Entrypoint 对 Gateway 命令目录的不同渲染，不是不同后端。
- InteractionCommand 接收结构化调用和受控 CommandActions，不解析 wire 对象，不直接
  访问 SessionStore、RuntimeAccess 或 AgentRuntime。
- 路由键为 `AgentID + WorkspaceID + SessionID + RunID`；创建 Session 前不含 SessionID。
- Gateway 核心与 carrier 无关；只有跨进程 Entrypoint 使用 RPC 或其他 wire protocol。
- 远程连接断开不取消 Run；Cancel 必须是显式 Gateway 命令。
- 流式是内部唯一执行路径；非流式由 Gateway 聚合本 Run 全部 assistant 文本消息。
- 重连以客户端持有的 revision 请求完整 Session Snapshot；只有携带该 Snapshot 当前
  revision 的 Subscribe 才建立后续实时订阅。Snapshot 与订阅之间若已有新提交，订阅
  返回 revision conflict，调用方重新获取 Snapshot，不能静默制造事件缺口。
- 订阅关闭或 overflow 只结束观察，不调用 Cancel，也不改变 Run 事实。
- AgentSlot 不保存 chunk 游标或框架级客户端 ACK；可靠投递由具体传输适配器或外部
  消息系统私有实现，不能改变 History、Context、Run 或业务完成状态。

## 13. sub-agent 与 Session 派生

- 每个 sub-agent 使用独立 Session、Queue、Context、History、RunJournal 和 Runtime。
- 完整 fork 复制指定 revision 的完整可审计历史，并记录父子来源。
- 来源 Session 尚未完成的 Queue 和 RunJournal 不进入 fork；子 Session 使用独立空闲
  执行状态，所有复制事实都重写为新的 Session/Message/ToolCall/Run/Step 身份，Context
  按子 Session 最终模型重新派生。
- 摘要启动创建新 Session，只把显式摘要作为新会话输入，不伪装成完整 fork。
- 两种方式都生成新的 AgentRuntimeConfig 快照，并默认继承来源 Session 当前的
  SessionModelConfig；创建命令的显式覆盖及最终配置必须可检查。
- 派生完成时即初始化子 Session 的 Runtime；仅浏览父子关系不会创建 Runtime。

## 14. 分阶段 TDD

### 阶段 0：文档与目录基线

- 中英文组件地图的 42 个 Slot ID、基数、职责、数量和成熟度一致。
- README、架构讨论、路线图和本计划不存在旧 `agent.loop` 标准语义。
- Slash/菜单呈现与 SessionModelConfig 后端能力保持分层。
- 所有 Entrypoint 只指向固定 Gateway，不存在 Entrypoint 到 RuntimeAccess 的直接边。
- 阶段 0 基线中的 9 个公共合同标为 Contracted；`model.catalog` 在阶段 7 成为第 10 个。
  其余生态位仍为 Mapped，不把候选代码写成 Conformant 或 Proven。

### 阶段 1：公共类型、typed Slot 和合同测试（已完成）

- 已固定身份、Revision、History/Context/Queue/RunJournal、SessionModelConfig 所需的
  公共值边界，并完成最小错误分类。
- 已声明并测试 `session.manager`、`session.store`、`model.executor`、`tool`、
  `context.source`、`context.compactor`、`agent.hook`、`interaction.entrypoint` 和
  `interaction.command` 的 typed Slot。
- 已固定 SessionStore 的 expected revision、幂等键和结构化 Change 输入；CAS 决策仍由
  Store 实现负责，未发布无约束的万能 SessionMutation。
- 已固定 ModelEvent/ModelStream 的临时 delta、reset、唯一完整结果和最终失败边界；
  Executor 可以在内部管理 Provider 的多次物理请求。
- 已完成 Assembly 描述安全性、Slot 基数、重复键、依赖和错误分类合同测试；没有实现
  AgentRuntime、Gateway、真实 Provider、Bash 或 UI。
- 本阶段当时有 9 个生态位标为 Contracted；阶段 7 新增 `model.catalog` 后当前总数为
  10。它们均尚未标为 Conformant 或 Proven。

### 阶段 2：应用级 Runtime、Registry 与 Gateway 主骨架（已完成）

- 实现 `standardagent.NewApplication` 的自动挂载语义：内部 Runtime/Gateway Module、
  标准 Profile 和同一个通用 Application Build/Start/Run 入口。
- 实现应用级 Runtime 持有的 RuntimeRegistry、RuntimeCoordinator、固定 Gateway、
  `GatewayAccess` 和私有 `RuntimeAccess`；注册表所有权属于启动后的 Application Runtime。
- 实现 Entrypoint Module 的 GatewayAccess 注入和能力隔离测试：Entrypoint、Command 和
  UI 都不能取得 Runtime、Store、Executor 或内部锁。标准应用必须通过
  `standardagent.NewEntrypointModule` 安装入口；内部 Build 校验器拒绝绕过包装器的原始
  `interaction.entrypoint` 贡献，该内部锚点不进入公共 Profile 或组件地图。
- 已实现 SessionID 路由、Snapshot/KnownRevision 重连基础语义、命令目录和受控命令
  Action 通道；Action 固定绑定本次 Invocation 的 Session scope，不能自行指定另一个
  Session，也不能在 Invoke 返回后继续使用。尚未进入本阶段范围的执行命令返回 typed
  unavailable，不伪造成功。
- 已测试启动/停止顺序、失败回滚、并发 Resume 单飞、并发 Resume/Close 和无半成品
  登记。完整主体授权、RunID 路由、事件信封、流式/聚合和断线订阅在对应 Gateway 与
  Runtime 阶段实现，不用临时传输逻辑绕过固定 Gateway。
- 为框架内部可选组件依赖增加 `OptionalOne/Many/Chain`：缺少可选 Provider 时 Build
  正常完成，一旦安装仍建立正确的构造和生命周期依赖；Resolver 仍只在 Build 构造期
  有效。

### 阶段 3：SessionStore 与 SessionManager（已完成参考实现）

- 已以内存实现驱动 append-only HistoryFact、CAS、幂等、原子提交和恢复测试。
- 已测试 Queue 编辑/删除/改投/认领冲突、单活跃 Run、tool call/pending 与
  result/terminal 的事务配对，以及 `outcome_unknown` 恢复。
- 已测试 create/resume/完整 fork/summary-start；并发 resume 的 Runtime 单实例语义由
  应用级 RuntimeRegistry 测试覆盖。
- 已测试 Create 使用 Agent 默认模型、Resume 保留 SessionModelConfig、派生 Session 默认
  继承且允许显式覆盖。
- 引入第二个独立存储实现前，不把生态位标记 Proven。

### 阶段 4：固定 AgentRuntime 与并发状态机（基础执行内核已完成）

- 已覆盖 idle/running/closed、Send、Steer、RunPending、Cancel、WhenIdle、Close，以及
  Queue 修改和 ModelConfig/UpdateModelConfig 的固定 Gateway 命令面。
- 已验证 idle 更新、running 拒绝、CAS、每 Run 配置快照和 Run 配置事实；跨 Provider
  能力兼容性确认仍在模型配置交互批次完成。
- 已验证同 Session normal 输入串行成不同 Run、Steer 在同一 Run 下一安全 step 继续，
  不同 Session 可并行。
- 已验证正常完成自动 FIFO；取消、模型失败和 Close 不自动消费旧 Queue；Close 后恢复
  同一持久 Session。真实 Provider、Tool、Context、Hook 和 Gateway 事件不混入本阶段。

### 阶段 5：ModelExecutor 与流式一致性（确定性执行器已完成）

- 已使用 FakeModelExecutor 测试临时 chunk、reset、完整结果、最终失败和取消；Executor
  只返回无持久身份的 Completion，唯一由 Runtime 分配 Message/Run/Step 身份。
- Executor 合同允许在一次逻辑调用内管理 Provider-specific retry/continuation，Runtime
  只消费统一事件；真实 Provider 差异的端到端证据属于阶段 9。
- AttemptID 已无损进入临时 Gateway 事件且不进入 History；运维/用量事件的持久或外发
  证据属于阶段 9 的观察组件。

### 阶段 6：工具循环与崩溃恢复（固定 Dispatcher 与 Bash 已完成）

- 已测试 Serial/ParallelSafe 分组、Schema 校验、安全错误和结果后继续模型；模型只提出
  无持久身份的 ToolCallRequest，Runtime 分配 ToolCallID 和包含关系。
- 已验证 call/pending 与 result/terminal 各自原子提交、唯一终态和 outcome_unknown 恢复；
  Tool 不能主动伪造 outcome_unknown。
- 已提供显式安装的内置 Bash Tool：固定绝对工作目录、显式环境、超时进程组取消、
  stdout/stderr 独立限制，以及非零退出码结构化返回；省略该 Module 即为禁用。
- 验证跨 Session 文件写入使用版本哈希和精确内容校验，不依赖 Workspace 全局写锁。

### 阶段 7：Context、Hook 与模型配置交互（已完成基础实现）

- 已实现有序 ContextSource、可替换 ContextCompactor、ContextVersion 与来源 revision；
  固定 SystemPrompt 和 Tool 定义在请求时重新装配，不写入动态 Context 或 History。
- 已测试模型协议完整性、硬 Token 上限、Compactor 二次计量、History 不变，以及文本
  模型把不支持的图片/音频投影为稳定附件说明。框架不偷偷截断事实。
- 已实现 `BeforeRunComplete` proposal 与异步 `AfterCommit` 观察；Runtime 重新分配
  proposal 身份并拥有唯一继续权，可替换组件不在 Runtime 状态锁内执行。
- 已实现 `ModelExecutor.Inspect/CountTokens`、`ModelCatalog`/`StaticCatalog`、idle-only
  模型配置切换、结构化兼容性确认，以及与配置更新同事务的
  `ModelConfigChanged` Session 事件。Run 内继续使用启动时冻结的配置。
- 当前没有把 TailCompactor 宣称为所有 Agent 的默认算法，也没有实现 Web/TUI；
  Session History 查询工具留在后续工具批次，Gateway 命令呈现进入阶段 8。

### 阶段 8：InteractionCommand 与进程内 Gateway 呈现（已完成基础实现）

- 已实现统一命令目录、受限 CommandActions、显式安装的默认 `model` 命令和
  `interaction/inprocess` 函数式 Entrypoint；重复命令 key 继续由 Build 拒绝。
- 已验证多个 Entrypoint 获得同一个 GatewayAccess 和同一命令目录；显式 `/model`、菜单
  与直接结构化调用最终执行同一个 UpdateModelConfig 后端事务。Slash 解析只存在于明确
  协议适配器，不用自然语言关键字猜测命令。
- 已实现 Gateway live EventStream、`SendAndWait` 非流式聚合和 Snapshot/revision 精确
  重连握手；流式 commit 与聚合结果引用相同持久消息，断线/慢订阅者 overflow 不取消
  Run，Session 事件不会串流。
- EventStream 不持久化 chunk 或客户端 ACK；进程内缓存有安全上限。Web/RPC、生产可靠
  投递、主体授权和完整跨进程协议不在本阶段冒充完成。
- `interaction.entrypoint` 与 `interaction.command` 仍保持 Contracted；一个进程内实现和
  一个默认命令不足以晋级 Conformant 或 Proven。

### 阶段 9：真实 Provider、持久化与生态适配（已完成基础实现）

- 已实现单进程、崩溃安全的 FileSessionStore；完整 Session 聚合采用私有文件、临时
  文件 fsync、原子 rename 和 revision/CAS，重启后可以恢复同一个 Session。
- 已实现 OpenAI Chat Compatible 流式 ModelExecutor：一次逻辑调用可以包含多个物理
  Attempt，支持有界 SSE、retry/reset、工具调用片段聚合、安全错误映射和 Provider
  报告的 Token Usage。它没有把某个 Provider 的 wire 类型泄漏给 Runtime。
- 已将 `policy.guard` 与 `approval.service` 接入固定 ToolDispatcher。Guard 只收到调用
  副本并返回 allow/deny/require-approval；策略、审批或插件 panic 均 fail closed，只有
  Dispatcher 可以执行原始 ToolInvocation。
- 已提供显式安装的根目录约束文件工具（读、SHA-256 CAS 写、精确编辑）和允许列表
  HTTP 工具；二者都覆盖越界、并发、大小、超时和结构化失败测试。
- 已定义并接入 Trace、Metric、Audit、Usage 四条被动 Chain Slot，并提供线程安全的
  JSON Lines 模块。观察实现不在 Runtime 锁内运行，其错误或 panic 不控制 Runtime。
- 已实现生命周期归属的行式 CLI Entrypoint。普通文本走 SendAndWait；明确 `/slash`
  走固定 Gateway 命令或控制面，不用关键词匹配猜测自然语言意图。
- `examples/reference` 只装配公共模块并调用 Gateway/CLI，不取得具体 Runtime 类型。
  真实全链路测试覆盖 CLI、Gateway、Runtime、OpenAI-compatible Provider、Bash 工具
  回传、assistant 完整消息、FileSessionStore 持久化和进程重启 Resume。
- 以上生态位仍保持 Contracted：独立实现数量增加不等于已经拥有共享 conformance
  suite。跨进程 wire protocol、可靠投递、调用者身份、分布式 Session 租约和企业部署
  继续作为后续能力，而不是本轮开发门禁。

阶段 2 起所有可运行测试都必须经过固定 Gateway；后续阶段只是逐步把 Session、Runtime、
模型、工具、Context 和真实适配器接入这条已经固定的边界。任何测试入口不得绕过 Gateway
直接调用 AgentRuntime。

## 15. 验证与发布门槛

每阶段运行：

```sh
gofmt -w .
go test -race ./...
go vet ./...
```

文档阶段额外运行 `git diff --check`、链接检查和关键术语扫描。不要为了文档链接
新增无业务价值的 `documentation_test.go`；可使用独立静态检查脚本或 CI 命令。

只有一个框架 Runtime 或一个组件实现，不能把任何 Slot 标记为 Proven。每个标准
生态位必须经过公开合同、兼容测试、两个语义独立实现和真实无分支消费者，才能按
组件地图规则晋级。

## 16. 实施参数（不是架构缺口）

| 事项 | 已固定边界 | 后续要决定 |
| --- | --- | --- |
| Gateway 传输适配器协议 | 固定 Gateway 核心 carrier-neutral、断线不取消、Snapshot/revision | HTTP/SSE、WebSocket、gRPC、ACP 等适配协议 |
| Queue 容量与配额 | 持久化、CAS、异常不自动 drain | Session/Workspace/Agent 限额和背压错误 |
| 重试与安全上限 | Executor 负责 Provider 恢复，Runtime 必须有终止边界 | 次数、退避、最大 Step、工具调用和运行时间默认值 |
| RunJournal 与 History 查询工具 | RunJournal 属于 SessionStore 聚合；History 查询通过受授权的标准 Tool | 具体数据库查询和 Tool 实现 |
| 第一批 Provider 适配器 | Provider 不是全局必需，Executor 可声明依赖 | 选择能证明协议差异的两个真实实现 |

`session.store`、`model.executor`、`agent.hook` 和 `interaction.command` 的 Slot ID、职责
及基数已经确定；固定 Gateway、AgentRuntime 自动挂载、`standardagent.NewApplication`
以及 Entrypoint 只能通过 GatewayAccess 接入也已定案，不再列为待商榷事项。表中的选择
只决定具体适配器、数值和实现，不得改变全景架构。
