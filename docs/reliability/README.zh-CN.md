# AgentSlot 运行可靠性设计

## 一句话结论

AgentSlot 应固化所有 Agent 产品都必须遵守的**持久事实、生命周期不变量和跨实现协议**；具体 Provider wire、超时数值、重试偏好、用户提示和任务验收继续由实现与组装项目负责。

## 文档状态

本文是 AgentSlot 可靠性工作的设计入口。ASR-001 至 ASR-005 与 ASR-007 的当前合同已经实现并进入普通测试门禁；ASR-006 的共享 token budget 与 Runtime 取消已经固化，具体 Provider 分层超时和重试仍属于 Executor/组装项目后续工作。

专项设计：

- [Session 与 Runtime 一致性](session-runtime-consistency.zh-CN.md)
- [Model 执行与失败语义](model-execution-boundary.zh-CN.md)
- [InputGate 框架验收记录](input-gate-round-3.zh-CN.md)
- [ToolPreflight 框架验收记录](tool-preflight-round-4.zh-CN.md)
- [ToolResultHook 框架验收记录](tool-result-hook-round-5.zh-CN.md)
- [可靠性回归账本](regression-ledger.zh-CN.md)

## 为什么必须在 AgentSlot 内设计

AgentSlot 的固定 Runtime 已经负责 Session 真值、CAS 提交、Run 生命周期、取消、恢复和终态提交。若这些规则留给每个组装项目自行补丁，`session.store`、`agent.loop` 和 `model.executor` 即使类型兼容，也会产生不同的安全语义，框架最终只剩一个模块注册器。

反过来，把 Anthropic thinking、OpenAI 字段、某个兼容网关的特殊行为或编码测试命令放入 AgentSlot，会污染通用契约，并迫使其他产品继承 LAS 的产品选择。

因此，本设计只下沉“不知道 LAS、Anthropic、OpenAI、TUI 或 Git 也仍然成立”的规则。

## 设计目标

| 编号 | 框架能力 | 必须保证的结果 |
|---|---|---|
| ASR-001 | ToolCall 稳定身份 | JSON 表示变化不能改变同一个持久工具调用的语义身份 |
| ASR-002 | Store 语义一致性 | memory/file/未来 Store 对同一提交序列产生相同可观察事实 |
| ASR-003 | Run 终态收敛 | Runtime 可接受新 Run 时，持久 Session 必为 idle，上一 Run 必有唯一终态 |
| ASR-004 | ModelStream 完整提交 | 临时流、半截流和非法 Completion 不得成为持久 assistant 消息或工具动作 |
| ASR-005 | 通用失败事实 | 非成功 Run 在 Store 可提交时保留稳定、脱敏、可程序判断的最终原因 |
| ASR-006 | 预算与取消闭合 | 物理 Attempt 不逃逸 Run 预算，取消能终止等待、重试和续传 |
| ASR-007 | 黑盒一致性验证 | 标准契约通过可复用 conformance 与故障注入测试证明，而非只靠参考实现单测 |
| ASR-008 | 扩展调用恢复 | 外部扩展调用的意图、执行边界、终态和业务后果分别持久化；pending 不重放，且不改写 History |

## 固化边界

### 应进入 AgentSlot 的内容

- Session aggregate、History、Queue、RunJournal、ExtensionJournal 和 Run lifecycle 的不变量。
- Runtime 内存状态与持久 Session 状态的收敛规则。
- ToolCall、Model Attempt、Run 和 Step 的稳定身份及包含关系。
- Provider-neutral 的 `ModelExecutor`、`ModelStream`、`Completion`、Attempt 记录和取消协议。
- 跨 Provider、跨产品仍成立的有限状态词汇与安全错误边界。
- Store、Runtime 和 Executor 的可复用 conformance 套件。
- 组装项目可配置的预算机制，但不包含某个产品选择的默认数值。

### 应留在实现或组装项目的内容

- Provider 请求/响应结构、thinking block、tool wire name 和私有 continuation 解释。
- 具体 route 的兼容策略、能力矩阵、base URL、凭据和模型清单。
- 连接、首事件、流空闲和总时长的产品默认值。
- 重试次数、退避、成本偏好以及面向用户的“是否建议重试”。
- TUI、CLI、日志文案和恢复入口的呈现方式。
- 编码 verification、工作区 diff、项目测试和真实 LLM 评测。

### 只有积累证据后才考虑标准化的内容

- Provider-neutral 的输出截断或 completion termination 词汇。
- 通用 Run wall-clock budget 公共接口。
- 独立 verification Slot。
- route capability 或 Provider adapter 公共合同。

这些能力必须先由至少两个语义独立实现和一个真实消费者证明共同边界，再决定是否进入 AgentSlot；不能因为 LAS 首先需要就宣布为通用标准。

## 跨层责任

| 问题 | AgentSlot 机制 | 组装项目策略 |
|---|---|---|
| Provider 超时 | 传播取消；记录 Attempt；禁止半截 Completion 入历史 | 选择超时阶段、具体数值、重试次数和退避 |
| Provider continuation | 持久保存 opaque 状态；保证与 Provider/model 身份绑定 | 校验、规范化并构造具体 wire block |
| Run 失败 | 持久记录通用 source/kind/code 和安全消息 | 映射为用户文案、恢复按钮和是否建议重试 |
| Tool 副作用未知 | `pending → outcome_unknown`，禁止无条件重放 | 提供人工对账或产品级恢复流程 |
| 长任务预算 | 保证 Attempt 共享 Run 预算并响应取消 | 选择 token、时间和成本上限 |
| 结果验收 | 不把 Run completed 定义成业务成功 | 决定是否以及如何执行项目验证 |

## 核心不变量

1. 持久事实是权威真值；Runtime 只能从已验证快照得出可接受新工作的状态。
2. History 严格追加；兼容旧数据通过读取和比较实现，不回写旧事实。
3. 同一个持久身份不因 JSON 空白、对象成员顺序或等价转义发生变化。
4. 一个 started Run 最终最多有一个 terminal fact；Run idle 与 terminal fact 必须原子提交。
5. `prepared` 表示副作用尚未开始，可以按原 ToolCallID 恢复；`pending` 表示副作用可能已发生，只能对账或进入 `outcome_unknown`。
6. Model delta 和 reset 是临时事件；只有完整、合法的 `EventComplete` 可以进入 History。
7. Provider 私有 continuation 对 Runtime 保持 opaque，只有对应 Executor 可以解释。
8. 重试性是当前实现和产品策略，不是不可变历史事实；未知副作用从 Journal 推导，不重复造一份 Run 状态。
9. ExtensionJournal 可以推进一次调用的状态与 effect/context disposition，但不得修改、删除、换位或插入旧 History；pending 恢复为 outcome_unknown，不能自动重放。

## 非目标

- 不引入多进程协调、租约、后台 Job 或 dead-letter 队列。
- 不承诺任意外部系统 exactly-once。
- 不把 SessionStore 改造成事件总线或第二套 Workflow Store。
- 不让 AgentLoop、Hook、GatewayChannel 或 Observer 直接修改 Session 真值。
- 不在 AgentSlot 核心登记任何具名 Provider 或产品兼容规则。
- 不因可靠性工作增加一个可绕过固定 Runtime 的第二控制面。

## 设计准入规则

一个可靠性行为只有同时满足以下条件才进入 AgentSlot 公共契约：

1. 不依赖具名 Provider、产品 UI 或业务验收。
2. 若缺失，会让两个声称实现同一 Slot 的组件产生不同安全语义。
3. 固定 Runtime 或通用 leaf package 能机械执行或校验。
4. 可以编写不依赖真实模型的黑盒测试。
5. 公共 API 增量小于重复实现和语义漂移造成的长期成本。

仅服务一个具体实现的辅助函数可以留在 AgentSlot 内部包，但不能因此提升成公共 Slot 或宣称 Proven。

## 交付与成熟度

- 先写复现历史事故的确定性测试，再修改实现。
- `session.store` conformance 增加 JSON 表示变化、恢复中断和终态一致性场景。
- FileStore 的短写、临时文件 sync/close、取消、rename 与目录 sync 故障通过包私有注入边界进入普通测试；rename 前失败保持旧文档，rename 后模糊结果依靠幂等记录安全观察。
- ExtensionJournal 的 memory/file parity、v1/v2 条件升级、损坏读取、pending 恢复、payload 清理和有界诊断进入普通门禁；未使用扩展的 Session 继续保持 v1。
- `model.executor/v1` conformance 已建立，覆盖公共 stream lifecycle、终态关闭、Attempt 配对和共享 token budget；当前 FakeExecutor 与参考 OpenAI-compatible Executor 驱动同一套测试，因此只能达到 Conformant，不能称为 Proven。
- `scripts/reliability-gate.sh` 固定执行格式检查、故障矩阵、完整 race、vet 和 build；它不读取真实 Provider 凭据，也不依赖外网。
- 公共状态或合同变化必须同步更新 `COMPONENT_MAP.md` 与 `COMPONENT_MAP.zh-CN.md`。
- AgentSlot 修复发布独立版本；LAS 只升级依赖并保留消费方回归，不复制框架修复。

## 与 LAS 的关系

LAS 的可靠性设计负责提出真实产品事故和消费方验收，AgentSlot 文档负责定义其中可迁移的通用机制。二者不是主从复制：

- AgentSlot 不引用 OPA、Updo 的成功率作为框架正确性证据；
- LAS 不把 AgentSlot conformance 全绿当作 Provider 兼容或编码任务成功证明；
- 一个跨仓库问题先在消费方保留最小失败用例，再在 AgentSlot 建立通用测试，最后回到 LAS 做消费验证。

## 相关现有文档

- [AgentSlot Standard Component Map](../../COMPONENT_MAP.md)
- [AgentSlot 标准组件地图](../../COMPONENT_MAP.zh-CN.md)
- [README](../../README.md)
