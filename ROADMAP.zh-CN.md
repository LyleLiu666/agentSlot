# AgentSlot 组件接口标准化路线图

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

## 3. 两种运行 Profile（启动规则）

“能运行的 Agent Host”和“标准 LLM Agent”不是同一个概念，本路线图将两者分开：

### 通用 Agent Host

至少需要：

- 一个 `AgentLoop`：决定一次任务如何执行；
- 一个 `SessionManager`：提供稳定的 Session 身份；
- 至少一个 `Entrypoint`：接收输入并返回结果，例如 TUI、Web、桌面端或 ACP。

这种 Host 可以运行确定性工作流、远程 Agent 桥接器或其他不直接调用模型的 Loop。

### 标准 LLM Agent

在通用 Host 基础上，还必须至少安装一个 `ModelProvider`。AgentSlot 自带的参考
Agent、LAS 的标准装配和所有对外宣称的 LLM Agent 都必须遵守这项要求。

Tool 和持久化 History 不作为所有 Agent 的强制要求：

- 没有 Tool 的 Agent 仍能正常对话；
- 没有 History 的 Agent 可以完成单次任务；
- 需要多轮连续对话的 Profile 应安装内存或持久化 History；
- 编程、运维等 Profile 可以明确要求一组 Tool 和相应 Policy。

底层 `agentslot.NewApplication` 保持通用，不偷偷加入业务要求。标准 Profile 由
专门的上层入口提供，开发者可以清楚看到自己选择了哪套启动规则。

## 4. 当前地图如何演进

当前中英文组件地图中的 40 个生态位是正式基线。在完成逐项评审之前，不用一张
新表直接覆盖它，也不为了追求数量随意增加或删除 Slot。

以下规则已经确定：

- Slot ID 保留领域前缀，例如 `interaction.entrypoint`、`gateway.route`，避免不同
  领域出现同名能力；
- `model.catalog` 允许不同 Provider 独立贡献模型目录，因此保持多个具名实现；
- Policy 负责作出风险判断，Approval 负责完成人工审批，两者不能合成一个接口；
- Trace 和 Metric 是不同的运维数据，不能因为经常一起使用就合成一个 Sink；
- Gateway 的接入、身份、路由和投递可以独立替换，不能只保留出站投递；
- Interrupt、Steer、Retry 等控制命令不只来自 Gateway。它们作为
  `control.inbox` 候选能力单独评审，不直接塞进 Gateway；
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

Catalog 展示整个行业地图；`Plan.Describe()` 展示某个应用实际装上的组件。
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

- Agent、Session、Run、Turn、Message 和 ToolCall 的稳定身份；
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

## 8. 默认组件只减少重复劳动，不替开发者做隐蔽决定

标准包可以提供默认组件，但必须遵守以下规则：

- 开发者显式安装的组件永远优先，与 Module 安装顺序无关；
- 默认实现只在对应的唯一 Slot 缺失时生效；
- 两个显式唯一实现继续报错，两个默认实现同样报错；
- 多个 Model Provider、执行环境或 Tool 之间的选择不能靠默认安装顺序决定；
- 默认组件由 `standard.Defaults()` 一类明确入口安装，导入包本身不会改变应用；
- 最终 Plan 必须标明默认或显式来源。

AgentSlot 禁止反射扫描、`init()` 自动注册和隐藏的全局组件容器。

## 9. 参考实现分三层

参考实现的任务是证明接口，而不是在 AgentSlot 中再造一个巨型产品。

### 第一层：最小对话 Agent

- Provider 无关的基础 Loop；
- 无密钥确定性 Provider，用于自动化测试；
- 正式的 OpenAI Chat Compatible 配置入口；
- 内存 SessionManager；
- 一个最小交互入口；
- Tool 为零也能完成任务。

### 第二层：工具 Agent

- 具名 Tool 和结构化结果；
- 流式事件；
- 内存严格追加 History；
- Context Source 和 Compactor；
- Policy Guard 与 Approval Service；
- Steering、Follow-up 和内部重试。

### 第三层：编程 Agent 示例包

- 文件读取、写入和精确编辑；
- 受控控制台执行；
- 写入和执行统一经过 Policy 与人工审批；
- 所有高风险工具都可以完全关闭。

[pi agent loop](https://github.com/badlogic/pi-mono/blob/main/packages/agent/src/agent-loop.ts)
和 [coding-agent SDK](https://github.com/badlogic/pi-mono/blob/main/packages/coding-agent/docs/sdk.md)
已经证明工具循环、Steering、Follow-up、流式事件和上下文处理是有效的行为分层。
AgentSlot 参考这些行为，不复制 pi 的类型、聚合 Session 或产品目录结构。

## 10. 实施顺序

### 阶段 0：先让地图可信

- 建立 ComponentCatalog；
- 从 Catalog 生成中英文地图；
- 建立防漂移测试；
- 所有现有 40 项先保持 `mapped`，不虚报接口完成度；
- 对每项 Slot 增删改记录业务理由和兼容影响。

### 阶段 1：建立共同语言

- 完成身份、消息、内容、事件、停止原因、用量、错误和引用类型；
- 保持模型模态和工具 JSON Schema 规则；
- 用两个 Provider 协议和两个 Session/History 实现校验这些类型没有偏向单一 SDK。

### 阶段 2：跑通标准 LLM Agent

- 完成 AgentLoop、SessionManager、ModelProvider 和 Entrypoint；
- 完成无密钥确定性链路和真实 OpenAI Chat Compatible 入口；
- 验证零 Tool、取消、错误、流式事件和多 Session 隔离；
- 提供极简交互入口，但不把文件和 Shell 工具作为标准 Agent 的必需能力。

### 阶段 3：完成第一批可扩展能力

- 完成 Tool、Events、History、Context、Policy 和 Approval；
- 验证 StandardLoop 更换 Provider、Tool、Session、History、Entrypoint 和 Policy
  时没有具体类型分支；
- 加入工具 Agent 和编程 Agent 示例包。

### 阶段 4：逐域扩展

- Environment、Artifact 和 Credential；
- Memory、Checkpoint、Workflow 和多 Agent；
- Gateway 和控制命令；
- Usage、Billing、Quota、Audit、Trace、Metric 和 Health。

每个阶段只按已经取得的成熟度记分，不因为写了空接口、空 Module 或示例文件就
算完成。

### 阶段 5：真实生态迁移

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
- `Plan.Describe()` 能说明最终装配，但不泄露配置、组件值或密钥；
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
