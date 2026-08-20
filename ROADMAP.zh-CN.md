# AgentSlot 组件接口标准化路线图

Session 运行模型的确定结论见
[Agent 设计的架构讨论](docs/agent-architecture-discussion.zh-CN.md)，可执行代码顺序见
[AgentRuntime 与标准 Slot 实施计划](docs/agent-runtime-standard-slots-implementation-plan.zh-CN.md)。
[Agent 框架全景架构](docs/agent-framework-architecture.zh-CN.md) 是完整对象、所有权和调用链的权威说明。
路线图只安排成熟度推进，不把尚未实现的设计写成已经交付。

## 1. 我们要解决什么问题

今天开发一个 Agent，团队往往要重复处理模型接入、工具调用、Session、历史、
界面、权限、计费和运行监控。不同项目又会把这些能力揉成不同的大包，导致组件
很难替换，也很难被另一个 Agent 复用。

AgentSlot 要交付的第一产品不是一个脚手架，也不是一个更复杂的依赖注入容器，
而是一张行业可复用的组件接口地图。它要让开发者一眼看明白：

- 一个 Agent 通常由哪些能力组成；
- 哪些能力可以单独开发和替换；
- 每项能力需要一个实现、多个具名实现，还是一条有顺序的处理链；
- 一个组件依赖什么，以及缺少它时能否启动；
- 当前标准是真正可用，还是仍在讨论和验证。

未来的组件开发者只需实现一项清楚的标准能力，Agent 开发者则通过选择组件完成
组装，把精力留给产品差异和组件质量。

## 2. AgentSlot 最终交付什么

AgentSlot 的完整交付由五部分组成：

1. **标准组件地图**：定义 Agent 可以定制的能力边界。
2. **组件接口与兼容测试**：让不同来源的实现可以真正替换。
3. **装配内核**：在启动前检查缺失、冲突和依赖关系，并管理启动与关闭。
4. **参考 Agent**：用真实运行链路证明接口地图不是纸上设计。
5. **旧 SDK 适配器**：让现有生态平滑进入新标准，不要求旧产品同步重写。

成绩按已经证明可替换的标准组件计算，不按接口、文件或 Module 数量计算。
Module 只是组件注册和生命周期的载体，不代表一个新的组件生态位。

## 3. 通用装配与标准 LLM Agent（启动规则）

“AgentSlot 通用装配核心”和“标准 LLM Agent Profile”不是同一个概念，本路线图将
两者分开：

### 通用装配应用

底层 `agentslot.NewApplication` 只要求应用显式给出名称、Module 和自己的 Profile。
它提供统一的 `Build`、`Start`、`Run` 和 `Stop`，但不偷偷加入 Agent 领域要求。
因此确定性工作流、远程桥接器和非 LLM 程序仍可使用通用核心，自行声明本地 Slot。

标准 LLM Agent 显式使用 `standardagent.NewApplication`。它返回同一个通用
`*agentslot.Application`，自动安装固定 AgentRuntime/Gateway Module 和标准 Profile，
所以所有 Agent 项目继续使用完全相同的 `Build`、`Start`、`Run`、`Stop` 入口；产品只
提供名称、Module、配置和额外 Profile 要求。框架不通过已安装 Slot 反向猜测 Agent
类型，也不增加 AgentHost 或第二套启动对象。

通用核心不提供可替换的标准循环。项目若需要与标准 LLM Agent 完全不同的循环，
可以定义项目本地 Slot 和明确的非标准 Profile，但不能把它登记为 `agent.loop` 标准
生态位。

### 标准 LLM Agent

标准 Profile 必须安装：

- 一个 `SessionStore`；
- 一个 `ModelExecutor`；
- 至少一个 `Entrypoint`。

`SessionManager`、`AgentRuntime` 和内部循环由框架提供，不是 Slot。`ModelProvider`、Tool、Context
组件、AgentHook 和 InteractionCommand 全局可选；固定 Gateway 同样由框架提供，
不是 Slot。所有 Entrypoint 只能通过 Gateway 接入。如果某个 ModelExecutor 需要本地
Provider 集合，由它通过 Slot 依赖显式声明。

Tool 不作为所有 Agent 的强制要求：

- 没有 Tool 的 Agent 仍能正常对话；
- SessionStore 可以是内存实现，但仍必须满足 History、Context、Queue、RunJournal
  SessionModelConfig 和原子 revision/CAS 合同；
- 编程、运维等 Profile 可以明确要求一组 Tool 和相应 Policy。

### 多 Workspace、多 Session 运行模型

一个 Application Assembly 只装配并启动一次应用级组件，可以同时服务多个 Workspace
和 Session：

- Build 阶段只解析声明的 Slot 依赖并形成不可变 Runtime 依赖集合；
- `Application.Start` 创建的应用级 Runtime 持有唯一的进程内 RuntimeRegistry 和固定
  Gateway；RuntimeCoordinator 只操作注册表，不拥有注册表；
- RuntimeAccess 只提供给固定 Gateway；全部 Entrypoint 只获得同一个 GatewayAccess，
  不能取得 AgentRuntime 指针或直接消费 InteractionCommand；
- 浏览或列出 Session 不创建 Runtime；CreateSession/ResumeSession 成功时立即初始化
  一个绑定该 Session 的 AgentRuntime；
- 一个启动后的应用级 Runtime 是单进程执行边界，它登记的全部 AgentRuntime 位于
  同一进程；同一 SessionID 只有一个 Runtime，并发 resume 返回同一实例；仅持久化但
  尚未打开的 Session 不占用 Runtime；
- 单进程所有权是标准架构决策，不是第一版的妥协；未来如果要支持跨进程 Session
  所有权、租约或迁移，必须重新进行架构评审；
- Runtime idle 时常驻，只在显式 Close 或应用停止时释放；Close 不删除 Session；
- 同一 Session 同时最多一个活跃 Run，不同 Session 可以并行；
- SessionModelConfig 持久保存当前 Provider、Model、Reasoning 和模型参数；只允许在
  Runtime idle 时显式更新，并在每个 Run 开始时冻结快照；
- Session 明确提供 History、Context、Queue 三个业务视图；
- Runtime 提供 Send、Steer、RunPending、Queue 修改、ModelConfig、UpdateModelConfig、
  Cancel、WhenIdle 和 Close；
- Resume 只表示从存储恢复 Session，不再兼任“继续执行”；
- Queue 持久化尚未进入 Context 的 normal、steer 和 held 消息；
- History 是唯一、有序、append-only 的事实账本；Context 才投影合法模型协议；
- RunJournal 只保存进行中工具调用的恢复证据，不成为第二份对话账本；
- 正常完成可以 FIFO 自动处理下一条 normal；取消、错误和重启回到 idle，但不自动消费旧 Queue；
- 应用级 Gateway 通过稳定身份路由，不为每个 Session 重复创建；它是进程内固定交互
  后端，不是可选 Slot，也不要求独立部署；
- `interaction.entrypoint` 继续表达至少一种用户接入方式，但它只是 Gateway 适配器，
  与 Gateway 不是互相替代的概念。

## 4. 当前地图如何演进

当前中英文组件地图中的 41 个生态位是正式基线。在完成逐项评审之前，不用一张
新表直接覆盖它，也不为了追求数量随意增加或删除 Slot。

以下规则已经确定：

- Slot ID 保留领域前缀，例如 `interaction.entrypoint`、`gateway.route`，避免不同
  领域出现同名能力；
- `model.catalog` 允许不同 Provider 独立贡献模型目录，因此保持多个具名实现；
- Policy 负责作出风险判断，Approval 负责完成人工审批，两者不能合成一个接口；
- Trace 和 Metric 是不同的运维数据，不能因为经常一起使用就合成一个 Sink；
- Gateway 核心固定；其传输、身份、路由策略和投递组件可以独立替换，不能把某个
  HTTP/WebSocket 服务误当成 Gateway 本身；
- 标准 `agent.loop` 已删除，因为固定 AgentRuntime 不是开发者可替换的生态位；
- 固定 SessionManager 与 `session.store` 分离，前者是框架能力，后者是负责完整
  Session 聚合持久化和原子事务的可替换 Slot；
- `model.executor` 是标准必需 `One` Slot，`model.provider` 是由具体 Executor
  选择性依赖的可选 `Many` Slot；
- `agent.hook` 是可选 `Chain` Slot，只允许受控 proposal 和提交后观察；
- `interaction.command` 是可选 `Many` Slot，只注册到固定 Gateway；Gateway 公开
  UI-neutral 命令目录，Entrypoint 再把稳定 key 渲染成 Slash、菜单、按钮、表单或
  命令面板；
- Session 的 History、Context、Queue 和 RunJournal 必须按不同修改规则建模，
  即使具体存储实现把它们放在同一个事务数据库中；
- Interrupt、Steer、Retry 等控制命令可以由不同 Entrypoint 发起，但都必须经过
  Gateway。它们在 Gateway 后面的控制能力仍作为 `control.inbox` 候选能力单独评审，
  不能把具体执行策略塞进 Gateway；
- Skill、Model Middleware、Tool Middleware 是否继续作为独立 Slot，要通过真实
  消费者证明，不能无说明地删除，也不能仅凭旧 SDK 已经存在就宣布完成。

修改 Slot ID、职责、数量规则或启动要求时，必须先写清楚三个问题：

1. 开发者因此能够单独替换什么？
2. 现有组件和应用受到什么影响？
3. 哪两个独立实现能够证明这个边界成立？

## 5. ComponentCatalog 是唯一成绩账本

AgentSlot 将建立显式 `ComponentCatalog`，记录每个标准 Slot 的：

- 领域和稳定 ID；
- 面向开发者的职责说明；
- 数量规则：唯一、多个具名实现或有顺序的处理链；
- 哪些 Profile 要求它；
- 当前成熟度和标准版本；
- 接口、兼容测试、独立实现和真实装配的证据链接。

中英文组件地图由 Catalog 生成，并通过自动化检查防止代码、成绩单和文档互相
矛盾。Catalog 只描述标准，不保存组件实例、产品配置或密钥。

Catalog 展示整个行业地图；`Assembly.Describe()` 展示某个应用实际装上的组件。
当前实现已经使用 `Assembly` 名称和 `agentslot.assembly/v0` 描述格式。
后者要标出组件来自开发者显式选择还是标准默认值，并展示依赖和启动顺序，但
不能输出配置、组件值或凭据。

## 6. 一个 Slot 怎样取得成绩

每个生态位按五个阶段推进：

| 阶段 | 对开发者意味着什么 |
| --- | --- |
| `mapped` | 职责、边界、稳定 ID 和数量规则已经说清楚，但还没有承诺 Go 接口。 |
| `contracted` | 已有 AgentSlot 自有接口和强类型 Slot，可以开始实现。 |
| `conformant` | 已有共享兼容测试，可以判断一个实现是否遵守标准。 |
| `proven` | 至少两个真正独立的实现通过兼容测试。 |
| `assembled` | 参考 Agent 或 LAS 已在真实链路中替换这些实现，消费者没有具体类型分支。 |

只有进入 `contracted` 的生态位才建立正式领域包。该领域包提供：

- 标准接口和唯一 Slot 声明；
- 简单的 `Provide...` Module 包装器；
- 组件间必须共享的稳定数据类型；
- 可被实现方直接复用的兼容测试套件。

“两个独立实现”不是把同一个实现包两层适配器。它们必须来自不同协议、不同存储
机制或不同实现路径，能够暴露接口设计中真正的共同语义和差异。

测试按组件的业务规则设计，而不是所有 Slot 机械复制同一份清单。例如：

- 唯一组件验证重复安装；
- 多实现组件验证重复键和明确选择；
- 处理链验证顺序和失败传播；
- 可选组件缺失时应正常运行，只有要求它的 Profile 才应失败；
- 生命周期由 Module 负责，不把启动关闭方法重复塞进每个领域接口。

## 7. 先固定跨组件共同语言

组件能够替换，不仅要求方法名一致，还要求它们对核心业务对象有相同理解。第一批
接口开始前，要先固定最小共同语言：

- Agent、Workspace、Session、Run、Step、Turn、Message 和 ToolCall 的稳定身份；
- 文本、图片、音频、工具调用和工具结果；
- 模型停止原因、用量、错误和取消；
- Agent、Turn、Message、Tool、Retry、Compaction 等事件；
- Artifact 引用和 Credential 引用。

这些类型只表达跨实现都稳定的业务事实。Provider 网络报文、产品配置、模型 ID、
数据库字段和 UI 状态继续留在具体实现中。

History 的“严格追加”必须成为可以验证的合同，而不是一句口号。至少要保证：

- 已发布记录不能修改、删除、换位或向前插入；
- 一批记录要么全部追加成功，要么全部失败；
- 多个入口同时写入时能够发现尾位置冲突；
- 重试同一次写入不会制造重复历史。
- 完整 tool call 产生后立即写入 History，并在同一事务建立 RunJournal pending；
- tool result 后续单独追加，每个 ToolCallID 最终只有一个终态结果。

Session 的其他视图也必须有可验证合同：

- Context 是版本化派生视图，压缩只能创建新版本，不能改写 History；
- Context 只投影满足模型协议的 call/result，未配对 call 不进入模型请求；
- Queue 使用 expected revision/CAS，消息被认领后不得原地编辑或删除；
- Session 执行状态只有 idle/running；取消、错误和重启后不自动消费旧 Queue；
- RunJournal 在工具执行前记录 pending 状态，崩溃后未知结果不得自动重跑。

## 8. 默认组件只减少重复劳动，不替开发者做隐蔽决定

标准包可以提供默认组件，但必须遵守以下规则：

- 开发者显式安装的组件永远优先，与 Module 安装顺序无关；
- 默认实现只在对应的唯一 Slot 缺失时生效；
- 两个显式唯一实现继续报错，两个默认实现同样报错；
- 多个 Model Provider、执行环境或 Tool 之间的选择不能靠默认安装顺序决定；
- 默认组件由 `standard.Defaults()` 一类明确入口安装，导入包本身不会改变应用；
- 最终 Assembly 必须标明默认或显式来源。
- 默认 ContextCompactor 可以采用“摘要 + 最近三条 inbound + 必要协议尾部”，但
  `context.compactor` 整体可替换，该算法不是所有 Agent 的框架不变量。

AgentSlot 禁止反射扫描、`init()` 自动注册和隐藏的全局组件容器。

### 当前开发进度

通用 Assembly、标准 Slot 合同、固定 Gateway/Runtime 主骨架、Session 内存与文件实现、
固定执行状态机、ToolDispatcher/Bash/文件/HTTP、Context/Hook/模型配置交互、Gateway
命令与呈现，以及第一批真实生态适配已经完成。
当前 Runtime 会安装版本化 Context、执行可替换 Source/Compactor、校验模型协议和硬
Token 上限，并通过 Gateway 在 idle 状态完成带兼容性确认的模型切换。`ModelCatalog`
曾是第 10 个进入 Contracted 的生态位；PolicyGuard、ApprovalService、TraceSink、
MetricSink、AuditSink 和 UsageRecorder 使当前总数达到 16。框架现已提供显式安装的
`model` 命令、函数式进程内 Entrypoint、行式 CLI、Gateway live event、非流式同 Run
聚合、Snapshot/revision 重连、OpenAI Chat Compatible Executor、FileSessionStore、
JSON Lines 观察模块和无具体 Runtime 分支的参考 Agent。所有这些仍是开发与合同证据：
尚无生态位达到 Conformant 或 Proven；下一步是共享 conformance suite、更多独立适配器
与跨进程 Gateway 能力，不把完整生产部署条件错误设成开发门禁。

## 9. 参考实现分三层

参考实现的任务是证明接口，而不是在 AgentSlot 中再造一个巨型产品。

### 第一层：最小对话 Agent

- 框架固定的 AgentRuntime，以及 Send/Steer/RunPending/ModelConfig/
  UpdateModelConfig/Cancel/WhenIdle/Close；
- 无密钥确定性 ModelExecutor，用于自动化测试；
- 正式的 OpenAI Chat Compatible 配置入口；
- 支持多 Session 隔离的固定 SessionManager 与内存 SessionStore；
- 一个最小交互入口；
- Tool 为零也能完成任务。

### 第二层：工具 Agent

- 具名 Tool 和结构化结果；
- 流式事件；
- 内存严格追加 History；
- Context Source 和 Compactor；
- Policy Guard 与 Approval Service；
- Steer、Send、RunPending 和 ModelExecutor 内部重试；
- 持久化 Queue、Session 恢复和 RunJournal 崩溃恢复；

### 第三层：编程 Agent 示例包

- 文件读取、写入和精确编辑；
- 受控控制台执行；
- 写入和执行统一经过 Policy 与人工审批；
- 所有高风险工具都可以完全关闭。

[pi agent loop](https://github.com/badlogic/pi-mono/blob/main/packages/agent/src/agent-loop.ts)
和 [coding-agent SDK](https://github.com/badlogic/pi-mono/blob/main/packages/coding-agent/docs/sdk.md)
已经证明工具循环、Steering、追加输入、流式事件和上下文处理是有效的行为分层。
AgentSlot 参考这些行为，不复制 pi 的类型、聚合 Session 或产品目录结构。

## 10. 实施顺序

### 阶段 0：先让地图可信

- 建立 ComponentCatalog；
- 从 Catalog 生成中英文地图；
- 建立防漂移测试；
- 所有候选生态位先保持诚实成熟度，不虚报接口完成度；
- 对每项 Slot 增删改记录业务理由和兼容影响。

### 阶段 1：建立共同语言

- 完成 Agent、Workspace、Session、Run、Step、Message、ToolCall 身份，以及
  History、Context、Queue、RunJournal、SessionModelConfig 的状态与事件类型；
- 用失败测试固定 revision conflict、消息已认领、无活跃 Run、无待处理工作、Runtime
  已关闭、取消和不可恢复 Session 等最小错误分类；
- 用并发、幂等和崩溃场景收敛 SessionStore 事务表达，不能直接把无约束
  `SessionMutation` 当成正式公共 API；
- 用临时 chunk、reset、唯一完整结果、最终失败、取消和关闭场景收敛 ModelEvent
  协议，不能让不同 Executor 各自解释流语义；
- 保持模型模态和工具 JSON Schema 规则；
- 声明 `session.store`、`model.executor`、`agent.hook`、
  `interaction.command` 及第一批关联 typed Slot，并用红测试固定基数、依赖和错误语义；
- 继续使用 `Assembly`、`AssemblyDescription` 和 `agentslot.assembly/v0`，不保留旧
  `Plan` 名称的同义 API；
- 清理示例和测试夹具中的旧 `agent.loop` 标准叙述，通用 Slot 示例使用明确的本地 ID；
- 只提供最小假实现证明装配，不实现完整 AgentRuntime，不打 tag、不发布；
- 接口批次通过评审后，才进入 Runtime 实现。

### 阶段 2：先证明 Session 合同（参考实现已完成）

- 完成内存 SessionStore 与固定 SessionManager；当前实现位于 `session` 包，尚未标记
  `Conformant` 或 `Proven`；
- 验证 append-only、revision/CAS、幂等和跨 History/Context/Queue/RunJournal 的原子边界；
- 验证 create、resume、完整 fork、摘要启动和崩溃恢复；
- 验证新 Session 使用 Agent 默认模型，resume 保留 SessionModelConfig，派生 Session
  默认继承且允许显式覆盖；
- 验证浏览不创建 Runtime，并发 resume 的单实例语义不制造半成品。

### 阶段 3：跑通固定 AgentRuntime（执行内核参考实现已完成）

- 已实现框架 AgentRuntime，不新增 Runtime Slot、Host 或公开 Factory；
- 已完成 FakeModelExecutor、内部 RuntimeAccess 和无密钥确定性链路；真实 Provider
  适配器按后续生态批次实现，不作为开发启动门禁；
- 已验证 idle/running/closed、Send、Steer、RunPending、ModelConfig、UpdateModelConfig、
  Cancel、WhenIdle 和 Close；
- 验证 running 拒绝更新、Cancel/WhenIdle 后更新、CAS 冲突、跨 Provider 切换、
  兼容性确认和单个 Run 配置不变；
- 已验证一个 Application Assembly 下的零 Tool、模型流接入、多 Session 隔离和 Runtime
  idle 常驻；Gateway 对外流式/聚合与重连已在后续交互批次完成；
- 已验证正常完成自动 FIFO，取消、错误和重启后回到 idle 且不自动消费旧 Queue；
- Run 开始和终态作为 append-only History 事实记录冻结的模型配置及来源 revision，临时
  chunk/reset 不持久化。
- 已实现受限 AttemptRecorder；每次物理请求在发出前记录 started，在重试或逻辑终态前
  记录 terminal、细分 TokenUsage、Provider request identity 或带来源的本地估算。
- 已实现 ContextSource 贡献先持久化、完整逻辑请求快照、LatestOnly/RetainAll 和
  MaxTokensPerRun；预算耗尽形成 RunBudgetExceeded 和可继续的新 Run 边界。

### 阶段 4：完成第一批可扩展能力

- 已完成固定 ToolDispatcher、工具事实事务、Serial/ParallelSafe 调度和首个显式安装的
  Bash Tool；Events、Context、Policy 和 Approval 继续按各自批次完成；
- 完成持久化 Queue、RunJournal、Context 版本与完整 History 查询；
- 验证固定 Runtime 更换 ModelExecutor、Provider、Tool、SessionStore、
  Context、Hook 和 Policy
  时没有具体类型分支；
- 验证工具 call 事实与 Journal pending 同事务、result 后续唯一终结、未知副作用恢复和跨 Session 文件版本冲突；
- Bash 已验证固定工作目录、显式环境、进程组超时取消、stdout/stderr 分离限制和非零
  退出码结构化返回；标准 Application 不自动安装 Bash；
- 验证 ModelExecutor 管理 Provider-specific 物理尝试和 AttemptID，Runtime 不包含供应商恢复分支；
- 验证替换 ContextCompactor 不受默认“最近三条”算法限制，但仍满足协议和 Token 硬上限；
- 加入工具 Agent 和编程 Agent 示例包。

### 阶段 5：建立 Gateway 主链路并逐域扩展

- Environment、Artifact 和 Credential；
- Memory、Checkpoint、Workflow 和多 Agent；
- 固定 Gateway、GatewayAccess、Entrypoint 和私有 RuntimeAccess 已在应用运行骨架阶段
  建立；InteractionCommand、默认 `model` 命令、进程内 Entrypoint 和流式/聚合重连也已
  完成基础实现。本阶段继续完成 Gateway 传输/身份/路由/投递适配组件和控制命令；
- 已验证 Entrypoint 只能通过 GatewayAccess 接入，多种 UI 表达从同一命令目录执行同一
  后端命令；下一步以真实跨进程适配器验证 wire 映射不改变这套业务语义；
- Usage、Billing、Quota、Audit、Trace、Metric 和 Health。

每个阶段只按已经取得的成熟度记分，不因为写了空接口、空 Module 或示例文件就
算完成。

### 阶段 6：真实生态迁移

- 旧 SDK 通过新增 `adapters/agentslot` 暴露拆分后的组件；
- 保留旧 SDK 原有装配入口，不要求 lyleCode 同步迁移；
- LAS 只消费 AgentSlot 标准接口和 Slot，承担全地图参考装配与替换测试；
- lyleCode 始终只读，用于行为基线和旧装配兼容验证。

## 11. 发布门槛

`v0.0.1` 保持不变。在以下条件满足前，不创建或推送新 tag：

- Catalog、Go Slot 声明和中英文地图完全一致；
- 标准 LLM 四件套至少达到 `conformant`；
- 四件套都有两个独立实现，并在参考 Agent 中完成无分支替换；
- Tool、Events、History、Context、Policy 和 Approval 已进入真实运行链路；
- 无密钥自动化任务和至少一个真实 Provider 配置入口可用；
- 缺失、冲突、依赖环、启动回滚、取消、并发隔离和 History 严格追加全部通过；
- 目标 `Assembly.Describe()` 能说明最终装配，但不泄露配置、组件值或密钥；
- AgentSlot、SDK 适配器和 LAS 分仓测试、分仓提交；
- `gofmt -w .`、`go test -race ./...` 和 `go vet ./...` 全部通过。

达到门槛只代表可以讨论下一版本，不代表自动发布。发布前仍要审查接口是否足够
小、命名是否稳定，以及真实应用是否真的减少了重复劳动。

## 12. 判断一项工作是否值得进入 AgentSlot

面对新的组件建议，只问四个问题：

1. 它是否代表开发者会单独替换的一项业务能力？
2. 两个独立实现是否都需要这条边界？
3. 抽出后是否减少了消费者对具体 SDK 或产品的依赖？
4. 它能否被兼容测试和真实装配证明？

如果答案是否定的，它更可能是某个实现的内部细节、产品配置或一个 Module 中的
普通代码，不应该为了扩大地图而进入 AgentSlot 标准。
