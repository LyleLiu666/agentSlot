# Model 执行与失败语义设计

## 一句话结论

AgentSlot 固化逻辑 Model 调用、物理 Attempt、临时流、持久 Completion 和 Run 终止原因之间的通用关系；Provider wire 的校验、超时实现、重试参数和兼容规则继续属于具体 Executor/adapter 与组装项目。

## 文档角色

本文细化 [ASR-004、ASR-005 与 ASR-006](README.zh-CN.md)，同时定义 `model.executor` 第一版 conformance 应验证什么。它不设计 Anthropic、OpenAI 或其他具名 Provider 的请求结构。

## 当前合同基础

现有 AgentSlot 已经建立三层身份：

1. **Run/Step**：固定 Runtime 拥有的逻辑执行边界；
2. **ModelExecutor.Execute**：一次逻辑模型调用，可以包含多次物理请求；
3. **Attempt**：一次真实 Provider 请求，拥有独立 AttemptID 和终态事实。

`AttemptRecorder` 是 Executor 唯一可写入的持久能力，`Completion.Continuation` 对 Runtime 保持 JSON opaque。这些边界继续保留，本设计不让 Executor 直接读取或修改 Session。

当前主要缺口不是缺少 Provider 错误字符串，而是：

- `ModelStream` 的完整性规则还没有独立 conformance；
- Run terminal fact 没有保存最终通用失败原因；
- Provider Attempt 已经有错误码，但请求构造、Loop、Context、Tool、Policy 和 Session 失败无法形成统一持久结论；
- 长请求的 Provider 超时与 Runtime 的总预算边界还没有写成明确责任。

## 设计决定

### 1. Provider wire 在 EventComplete 之前结束

Executor/adapter 必须在发出 `EventComplete` 前完成：

1. Provider 响应解析；
2. wire block 完整性校验；
3. Provider 私有 continuation 校验和规范化；
4. tool wire name 到内部工具名的可逆映射；
5. Provider stop/finish 状态的解释；
6. Provider-neutral `Completion.Valid()` 校验。

任何一步失败都只能形成 failed Attempt 和失败的逻辑调用，不能先发出一个包含 ToolCall 的 `EventComplete`，更不能等工具执行后才在下一轮暴露协议错误。

AgentSlot 只验证 Provider-neutral 结果，不理解 thinking block、response item、content block 或兼容网关私有字段。

### 2. ModelStream 使用单一终止协议

一个逻辑流遵守以下状态机：

```text
open → (delta* → reset)* → delta* → complete
open → failed
open → delta+ → reset → failed
open → canceled
```

约束如下：

- `delta` 与 `reset` 仅用于实时展示，不进入 History；
- Executor 若在已经发出 delta 后重试或失败，必须先发出 reset；
- 一个逻辑流最多有一个 terminal outcome；
- `complete` 之后必须返回 `ErrStreamClosed`；
- `failed` 之后不得继续输出 delta 或 complete；
- context 取消可以直接结束 Recv，但 Runtime 必须把 Run 收敛为 canceled，而不是 Provider failed；
- Provider 连接正常关闭但缺少协议终止标记，属于 failed，不是 complete；
- 非法或空 `Completion` 不得提交 assistant Message。

Runtime 保留 assistant MessageID、Session/Run/Step containment 和 History commit 的分配权。Executor 不生成持久 Message，也不自行提交工具调用。

### 3. Attempt 与重试规则

- 每次真实 Provider dispatch 使用新的 AttemptID。
- `AttemptRecorder.Started` 在发送任何请求字节之前完成持久提交。
- `AttemptRecorder.Finished` 在 Executor 重试或向 Runtime 暴露逻辑终态之前完成持久提交。
- 请求未发送但已经进入一次可观察的 Provider 尝试时，也必须以稳定的 pre-dispatch 错误结束 Attempt；不能留下 started 无终态。
- partial stream 重试前先 reset，再创建新 Attempt；不得把两个 Attempt 的文本拼成一个未经证明完整的 Completion。
- retry-after、退避和重试次数由 Executor 实现决定，但不得逃逸 Runtime 提供的共享预算和取消信号。
- Attempt 的 ProviderRequestID、HTTP 类别和安全诊断继续记录在 Attempt fact，不复制到每个 Run terminal fact。

### 4. Run terminal 增加最小通用原因

`RunFact` 保持生命周期事实，并为非 completed 终态增加可选的通用 termination：

```text
RunTermination
  source       失败所属的稳定组件阶段
  kind         agent.ErrorKind
  code         agent.ErrorCode
  safe_message 可选、受限、已脱敏的单行说明
```

建议的有限 `source` 词汇：

- `model`
- `context`
- `loop`
- `tool`
- `policy`
- `budget`
- `session`
- `runtime`

字段规则：

- 新产生的 `failed`、`interrupted` 和 `canceled` RunFact 必须带 termination；
- `started` 和 `completed` 不得带 termination；
- `kind` 表达调用方可采取的通用反应，`code` 表达稳定领域原因；
- `safe_message` 可为空，但非空时必须有长度上限、合法 UTF-8、去除首尾空白且不含控制字符；
- 未分类实现错误持久化为 `internal` 和稳定的通用 code，原始 cause 只进入受控日志；
- 旧 Session 缺少 termination 时仍可加载，并由读取方明确视为 legacy unknown。

不保存以下字段：

- `retryable`：是否重试取决于当前预算、配置和产品策略，不是永久事实；
- `effect_state`：Tool 副作用是否未知从 RunJournal 的 `outcome_unknown` 推导；
- Provider body、请求头、凭据和未分类原始错误；
- 面向某个 UI 的恢复按钮或长篇建议。

如果 SessionStore 本身无法接受终态提交，框架不伪造“已经持久化的失败”。Runtime 必须 fail closed，留下安全 trace，并由后续显式 Recover 在 Store 可写时形成 interrupted 事实。

### 5. Provider stop reason 不直接进入公共 Completion

当前 `ModelExecutor` 明确拥有 Provider retry 和 continuation 差异。因此：

- Provider 正常 stop：Executor 可以发出 complete；
- Provider tool use：Executor 映射为合法 ToolCalls 后发出 complete；
- Provider 长度截断：Executor 在预算内完成协议续传，或者以稳定 `model_output_truncated` 失败；
- Provider 内容过滤或私有阻断：Executor 以安全分类失败；
- 半截流或未知 stop：Executor 失败。

本阶段不向 `Completion` 添加公共 `FinishReason`。只有当至少两个语义独立 Executor 和一个真实 AgentLoop 证明它们需要把同一 termination 语义暴露到逻辑层时，再另行设计有限词汇。

这样可以避免把 Provider wire stop reason 泄漏给每个 Loop，也防止 Runtime 把“有一些文本”误当成完整结果。

### 6. 超时与预算分层

| 边界 | AgentSlot 责任 | Executor/组装项目责任 |
|---|---|---|
| Run token budget | Runtime 计算累计 Attempt usage；所有重试共享 | 选择上限 |
| Run cancellation | Runtime 关闭 Run context；等待、退避和流读取必须响应 | UI 或调用方触发取消 |
| Provider connect/header timeout | 传播分类结果和 Attempt 事实 | adapter 实现及配置数值 |
| Provider first-event timeout | 不把无事件等待当作进展 | adapter 实现及配置数值 |
| Provider stream-idle timeout | 不提交半截 Completion | adapter 实现及配置数值 |
| Attempt 总时长 | 保证取消和终态闭合 | Executor profile 数值 |
| Run wall-clock budget | 待第二种真实需求证明后再决定公共接口 | 当前可由产品在 Runtime 外层控制 |
| token/cost 后续重试 | `AttemptRecorder.Budget` 提供已用 token 事实 | Executor 根据配置决定是否再试 |

本阶段不把一个固定 60 秒或 5 分钟写进 AgentSlot 默认值，也不让 Runtime 根据 Provider 名称选择时长。

### 7. Capability 只标准化可移植事实

`ExecutionCapabilities` 继续表达 Runtime 在调用前必须知道的可移植能力，例如模态、reasoning 范围、context window 和最大输出。

某条 route 是否接受 empty signed thinking、工具名允许哪些字符、是否要求特殊 header，属于 adapter capability/compatibility 配置，不进入 AgentSlot 公共描述。

新的公共 capability 必须满足：

- 至少两个独立协议存在相同语义；
- Runtime 或通用调用方确实需要在 dispatch 前判断；
- 不能仅靠 Executor 内部适配解决；
- 有 conformance 能机械验证。

## Run 失败归因流程

```text
具体组件错误
  → agent.ErrorKind + agent.ErrorCode
  → Runtime 选择稳定 source
  → 非成功 RunFact 携带 RunTermination
  → Session History 持久化
  → 组装项目映射为用户文案和恢复建议
```

Provider Attempt 的详细事实和 Run 的最终事实各司其职：

- Attempt 回答“哪一次真实请求发生了什么”；
- Run termination 回答“整个 Run 为什么没有成功”；
- Journal 回答“工具副作用是否已经获得执行权以及结果是否已知”。

三者通过 RunID、StepID、AttemptID 和 ToolCallID 关联，不互相复制整份诊断。

## ModelExecutor conformance

第一版黑盒套件至少覆盖：

### 身份与事实

- 每次物理 dispatch 恰好一个 started 和一个 terminal Attempt fact；
- 重试使用新 AttemptID，RunID/StepID/config 保持冻结；
- ProviderRequestID 仅在实际获得后记录；
- usage 合计不重复计算 cached/reasoning token。

### 流与重试

- 单次成功流；
- delta 后成功；
- delta 后失败先 reset；
- delta 后重试先 reset，随后新 Attempt；
- 缺少终止标记失败；
- 非法 Completion 失败；
- complete/failed 后 `ErrStreamClosed`；
- terminal 后继续输出被拒绝。

### 取消与预算

- dispatch 前取消；
- 请求中取消；
- 退避中取消；
- stream Recv 中取消；
- token budget 已耗尽时不开始下一 Attempt；
- 多次 Attempt 共享同一 Run budget。

### 失败安全

- 凭据、请求构造、连接、HTTP、流解析和协议错误具有稳定安全分类；
- 未识别 Provider body 不进入 ErrorMessage；
- 控制字符、超长文本和无效 UTF-8 被拒绝；
- partial 内容不进入持久 Completion；
- Attempt observer 或 recorder 失败时不继续 Provider 重试掩盖持久化失败。

具体 Anthropic/OpenAI fixture 不进入通用 conformance；adapter 实现用自己的协议 fixture 驱动同一黑盒行为。

## 兼容性

- `RunTermination` 对旧文件是可选读取字段；新 Runtime commit 必须写入。
- `ModelAttemptFact` 保持现有身份和错误字段，不做破坏性迁移。
- `Completion.Continuation` 继续 opaque，不要求旧 Session 理解新 Provider 状态。
- 不修改 `ModelExecutor.Execute` 的所有权：具体重试与 continuation 仍在 Executor 内。
- 若公共错误 code 增加，只增加有限稳定值，不把 HTTP 状态或 Provider 私有字符串全部提升为 AgentSlot 常量。

## 非目标

- 不提供统一 Provider SDK。
- 不把某个 adapter 的 timeout struct 放进 AgentSlot 核心。
- 不由 Runtime 解析 Provider continuation。
- 不让 Loop 决定物理 HTTP 重试。
- 不持久化“用户应该点击哪个按钮”。
- 不在只有一个消费方时增加 verification 或 provider-route Slot。

## 完成定义

- 每个非成功 Run 在 Store 可写时有通用、脱敏的 termination；
- 用户取消不再被记录为 Provider timeout 或模型失败；
- 半截流、非法 continuation 和缺少终止标记无法产生持久 assistant Message 或工具动作；
- 所有物理 Attempt 在重试和逻辑结束前完成持久闭合；
- ModelExecutor conformance 可由 FakeExecutor、参考 OpenAI-compatible Executor 和 LAS 消费 adapter 使用；
- AgentSlot 只因通过某个实现而标记 Conformant，不在两个独立实现前标记 Proven；
- LAS 能在不读取原始错误字符串的情况下，把 Run、Attempt 和 Journal 事实映射成产品级失败归因。
