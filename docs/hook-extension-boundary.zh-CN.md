# AgentSlot Hook 扩展边界设计

## 一句话结论

AgentSlot 当前的 `agent.hook` 只是 Run 结束前的受限续跑提议点，不是完整的产品 Hook
系统。用户配置、事件匹配、命令执行、项目信任、超时和展示属于组装产品；AgentSlot 只在固定
Runtime 必须参与时提供窄而有类型的事实与决策边界。

## 文档角色

本文是 AgentSlot 对 Hook 类扩展的框架设计依据，回答“哪些语义可以进入通用框架、各组件拥有
什么权力”。它不定义 LAS、Claude Code 或其他具名产品的配置格式和事件清单。

当前实现事实：

- `agent.hook` 只有 `BeforeRunComplete`，只能返回追加输入 proposal；
- `policy.guard` 和 `approval.service` 决定工具能否执行；
- `session.commit.observer`、Trace、Metric、Audit、Usage 只观察已经发生的事实；
- Goal 由 `goal.evaluator` 直接判断，不通过 `agent.hook` 实现；
- 一个合同存在并通过框架测试，不等于组装产品已经提供用户可配置 Hook。

## 术语与所有权

| 名称 | 所有者 | 权力 |
|---|---|---|
| AgentHook | AgentSlot | Run 原本准备结束时，基于只读证据提出后续输入；不能修改 Session |
| PolicyGuard / ApprovalService | AgentSlot 合同，产品提供实现 | 允许、拒绝或要求批准外部动作；不能替换待执行调用 |
| Observer | AgentSlot 合同，产品提供实现 | 观察已提交事实；不能回滚提交或控制 Runtime |
| 产品 Hook | LAS 等组装产品 | 配置、显式匹配、命令或回调运行、信任、超时、输出协议、诊断和展示 |

“Hook”是产品层的统称，不是扩大框架组件权力的理由。产品可以把不同 Hook 事件适配到不同的
typed Slot；AgentSlot 不提供一个携带任意 map、允许任意修改的万能事件总线。

## 事件应落在哪条边界

| 产品事件族 | 首选通用边界 | 当前状态 | 约束 |
|---|---|---|---|
| Run 停止前续跑 | `agent.hook` | 已有 | 只能追加输入；用户 steer 优先；续跑受 Run 预算限制 |
| Run 停止前阻断或失败收敛 | `hook.CompletionGate` | 已设计、待实现 | 保留 legacy AgentHook；新 gate 的错误由 Runtime 收敛，不能静默当作完成 |
| Tool 执行前 | `hook.ToolPreflight` → `policy.guard` / `approval.service` | 已设计、待实现 | Runtime 先持久化外部 preflight；结果再进入既有 Policy/Approval，allow 不能绕过 Guard |
| Tool 成功或失败后被动处理 | `session.commit.observer` / Observation | 已有 | 只看已提交结果；失败不能改变业务提交 |
| Tool 结果后同步追加模型上下文 | `hook.ToolResultHook` | 已设计、待实现 | ToolResult 与调用意图先提交；追加内容成为 ContextContribution，不能改写原结果 |
| 用户输入提交前 | `hook.InputGate` | 已设计、待实现 | Gateway 先保留调用 occurrence；只能拒绝或附加独立上下文，不能暗改用户原文 |
| Session 打开与关闭 | `hook.SessionLifecycle` | 已设计、待实现 | coordinator 明确 open kind；只读身份和状态，不能持有 Runtime 或改写旧 History |
| PermissionRequest | `approval.service` | 已有 | 不再创建同义 Hook 权力面 |
| sub-agent、Worktree、文件监视、Elicitation | 对应生态的 typed contract | 尚无准入证据 | 先有真实产品消费者，再决定是否进入 AgentSlot |

## 通用合同准入规则

新的 Hook 类能力只有同时满足以下条件，才进入 AgentSlot：

1. 触发点位于固定 Runtime、Gateway 或 Session 事务内部，产品无法在外层可靠实现。
2. 输入是有限、稳定、provider-neutral 的只读事实，不携带产品配置或任意动态对象。
3. 输出权力可以用有限枚举表达，并由固定 Runtime 校验和执行。
4. 决定外部副作用或控制流的结果在生效前形成可恢复事实；被动观察只发生在提交之后。
5. 超时、取消、panic、排序和多 Hook 合并语义可以写成确定性合同测试。
6. 至少有一个真实产品消费者；达到 Proven 仍遵守组件地图要求的独立实现和 conformance 门槛。

不满足这些条件时，能力留在产品适配层。不得为了复刻某个产品的事件名，把具名配置、Shell
runner、项目目录发现、用户提示文案或协议私有字段放入 AgentSlot。

## 已确认的最小新增合同

### 为什么不是一个万能 Hook

LAS 的真实消费者已经证明，以下五种权力都位于固定事务内部，但它们不能互相替代：

| typed Slot | cardinality | 唯一权力 |
|---|---:|---|
| `hook.InputGate` | Chain | 接受或拒绝一次拟提交输入，并可提出独立上下文 |
| `hook.ToolPreflight` | Chain | 对已校验的 ToolCall 表示 allow、deny 或 require approval |
| `hook.ToolResultHook` | Chain | 在 ToolResult 已形成后提出下一 Step 的独立上下文 |
| `hook.CompletionGate` | Chain | 允许 Run 自然完成，或提出有界 continuation |
| `hook.SessionLifecycle` | Chain | 观察一次 Runtime instance open/明确 close；open 可提出后续上下文 |

每个 component 是一个独立 Chain contribution，并提供构建期冻结的 descriptor key 和 definition digest；
Chain 顺序是唯一执行顺序，同一 Slot 的重复 key 在构建时失败。Tool 和 lifecycle component 还提供有限、
静态的 scope metadata：全体或精确 Tool key、有限 ToolResult status、open/close。Runtime 在创建 invocation
记录前按 metadata 过滤；不调用自然语言 matcher，也不把产品 profile 放进框架。

descriptor key 最多 128 bytes、trimmed、有效 UTF-8 且无 control；digest 固定为 `sha256:` 加 64 位
lowercase hex。descriptor/scope 只在 build 读取并深拷贝一次，运行期不能热变。

一个外部命令必须对应一个 component invocation。不得让一个 adapter 在内部执行多条无法分别恢复的
命令，也不得让 component 自己取得 Session 后旁路提交。

现有 `agent.hook` 不改合同：它继续提供错误隔离的 best-effort follow-on。需要 fail-stop 的产品不能
偷偷改变它的语义，而应使用 `hook.CompletionGate`。

### 有限结果

- InputGate：`accept | reject`；accept 可带 context，不能返回替换 input。
- ToolPreflight：`allow | deny | require_approval`；不能替换 ToolCall。
- ToolResultHook：只能返回 context proposal，不能改 ToolResult。
- CompletionGate：`complete | continue`；continue 必须带有效 context。
- SessionLifecycle：open 可带 context，close 不允许 context。

框架硬合同把单个 ContextProposal 限为最多 16 个 input、合计 256 KiB provider-neutral metadata/text，
reason 限为 1 KiB 安全单行。组装产品可以更严格，但不能放宽；该值不跨 occurrence 累计，也不冒充
Run token 或时间预算。

所有 view 都是 provider-neutral 深拷贝。组件不得获得 Runtime、Session、Store、Dispatcher 或可写
History。Runtime 校验无效结果、panic、error、取消和顺序。

## ExtensionJournal

### 为什么属于 Session aggregate

外部 component 可能在输入提交、Tool pending、下一模型 Step 或 Run 终态之前产生副作用。只有固定
Runtime 知道这些事务位置；若把状态放在组装产品 sidecar，会出现两个 revision 和两个恢复真相。

因此 Session 增加通用 `ExtensionJournal`，它不是产品事件总线，也不是模型 History。entry 只记录：

- invocation ID、Session 内单调 extension sequence、descriptor key、definition digest、有限 boundary kind；
- Session 和可选 Run、Step、Message、ToolCall identity；
- 规范化 typed input digest；
- `prepared | pending | succeeded | failed | canceled | outcome_unknown`；
- typed result、安全错误码、时间、effect disposition 与 context disposition。

不记录产品配置、命令 argv、环境、stdin/stdout/stderr 全文或任意 JSON state。

### 状态与重放

```text
prepared -> pending -> succeeded | failed | canceled | outcome_unknown
prepared -> failed | canceled
```

- Runtime 在调用 component 前先提交 pending；component 此后才可能产生副作用。
- active Run 内的 prepared 只有在输入可从 durable facts 重建且 descriptor key/digest 精确匹配时才可
  恢复执行；Prompt input 尚未持久化不重放，旧 lifecycle prepared 随未完成 open/close occurrence
  失效并 canceled。
- pending 永不自动重放；进程恢复时标为 outcome_unknown。
- terminal context 只消费一次，消费或丢弃与对应业务 mutation/Step commit 关联。
- command status 与 effect 必须分开：任何 terminal outcome 的固定成功/失败后果都先记 pending，再记
  applied/discarded；旧 revision 决策或尚未提交的 RunInterrupted 不能只靠 terminal status 猜测。
- terminal pending、对应业务 mutation 与 applied/discarded 可以作为同一个 `Store.Commit` 内的有序
  changes 原子提交；状态语义仍分离，但不强迫 FileStore 为无恢复窗口的连续步骤多做一次全量写入。
- occurrence 提前结束时，未调用的 prepared entry 必须转为 canceled，idle Session 不留悬空 prepared。
- digest 基于规范化 typed value，不比较可能因空格、转义或 object key 顺序变化的原始 JSON 文本。

模型可见的附加内容仍通过现有 `ContextContributionFact` 保存，绑定精确 Run/Step 和来源；
`ExtensionJournal` 只保存恢复证据，不能成为第二份模型历史。

## 固定运行顺序

### Tool

1. ToolCall、Tool RunJournal prepared 与当时全部静态匹配的 preflight invocation prepared 在同一次
   commit 中提交，不能暴露“Tool 已 prepared、preflight 集合尚未建立”的恢复窗口。
2. schema 校验失败时不调用 preflight，匹配的 prepared entry 统一 canceled/discarded；校验通过后，
   preflight 按 ToolCall 顺序、再按 component 装配顺序执行。
3. 决策按 `deny > require_approval > allow` 合并，再进入现有 Policy/Approval。
4. 只有最终允许后，Tool Journal 才进入 pending；获准 Tool 保留原 ParallelSafety。
5. 只有 Tool 真正进入 pending 并返回 succeeded/failed 才匹配 result hook；preflight/Policy 拒绝和
   outcome unknown 不冒充 Tool 执行后事件。
6. ToolResult terminal 与匹配的 result-hook prepared 在同一次 Session commit 中形成。
7. result hook 全部 terminal、context 一次性消费后，下一模型 Step 才可开始；中途失败会丢弃该
   occurrence 的半成品 context。

恢复旧版本遗留、完全没有 preflight entry 的 prepared ToolCall 时，Runtime 可以按当前构建期冻结的
component 集合在一次 commit 中补齐 reservation；只要已经存在任一 preflight entry，该 ToolCall 的
集合就视为冻结，缺失、重复或 definition digest 不匹配必须 fail closed，不能拼接新旧集合。

### Run completion

用户 steer 和 cancel 优先。active Goal 先形成 candidate：continue/blocked 使用现有 Goal 收敛并跳过
completion gate；done 只有在 gate 允许后才提交。没有 active Goal 时直接调用 gate。gate continue 保持
同一 Run，不重置任何预算；具体产品必须提供可恢复的 follow-on 上限。

legacy AgentHook 的相对顺序由 conformance 固定，但 fail-stop 产品不得同时安装同义 legacy 和 gate
实现，避免两个完成裁决面。

### 输入与 Session lifecycle

输入 gate 的 prepared commit 是调用者 ExpectedRevision 的 linearization point；之后业务 mutation 使用
当前内部 revision，并重新校验 subject。框架不能在 journal 推进 revision 后继续冒充原 CAS 仍有效。
ClientMessageID 仍是 correlation，不因引入 gate 变成幂等键。

Runtime coordinator 必须显式区分 create、resume、fork、summary；重复返回已打开 Runtime 不算再次 open。
close 只来自明确 Gateway CloseSession，不由 View/Subscribe 断开或进程崩溃伪造。
Fork/summary 新 Session 不复制父 Session 的 ExtensionJournal；resume 继续原 journal。

create/fork/summary 保留通用 Manager Create，注册赢家随后提交 open prepared，命令收口后 open 才成功
返回；不能把产品 component 模板塞进 Session Manager。resume 也由注册赢家在执行前提交 prepared。
CloseSession 持锁校验 caller CAS 后用同一 revision 提交 close prepared，随后才进入 close worker。
没有匹配 component 时保留原 fast path，不制造空 journal commit。

并发 create/resume 先注册唯一 `opening` instance，所有 Gateway operation 等待同一个 open barrier；只有
注册赢家执行 lifecycle open。恢复先标记旧 pending unknown，open 收口后恢复/激活 prepared/active Run
或提交中断终态，再切换 idle/running 并关闭 barrier，避免模型、外部命令或新输入抢在 SessionStart 前执行。

## 不可破坏的运行语义

- 原始用户输入、模型 ToolCall 和 ToolResult 一旦可见，继续保持严格追加，Hook 不得原地替换。
- 多个决策组件按装配顺序执行；任一拒绝都不能被后续 allow 覆盖。
- Hook 不能批准 Policy 已拒绝的动作，也不能绕开 `approval.service`。
- 同步 Hook 受调用方 context、超时、输出上限和 Run 预算约束；取消优先于自动续跑。
- 新 typed boundary 的故障由框架合同固定收敛：InputGate 拒绝 mutation，ToolPreflight 不执行 Tool，
  ToolResultHook/CompletionGate 中断当前 Run，SessionLifecycle 不阻止安全 open/close。component 不能
  逐项选择 fail-open/fail-closed；被动 Observer 继续使用原隔离语义。
- 被动 Observer 的慢、错或 panic 不得阻塞提交；需要影响下一模型 Step 的能力不能伪装成 Observer。
- 所有影响模型上下文或工具执行的 Hook 结果必须可审计，并能关联 Session、Run、Step 和 ToolCall。
- Gateway 只投影有界安全诊断；SessionView 给最近摘要，独立只读方法按 immutable extension sequence
  bounded 分页。不暴露 component 原始输入输出、环境或 additional context 全文。open receipt 返回本次
  lifecycle 诊断；CloseSession 返回明确 receipt，使“安全关闭已完成但 End component 失败”不会被
  non-nil error 伪装成关闭失败；输入 gate 的 typed error 必须携带 journal 推进后的当前 revision。
- Run 因扩展基础设施失败而 interrupted 时使用新的 provider-neutral `TerminationExtension`；不能归因成
  model、tool 或 runtime。组装产品可把该 source 显示成自己的 Hook 名称。

## FileStore 兼容边界

当前 FileStore v1 严格拒绝未知字段。ExtensionJournal 进入 Snapshot 时必须显式升级为
`agentslot.session-file/v2`：新版本双读 v1/v2，v1 映射为空 journal；空 journal 的新 Session 和普通
v1 mutation 继续省略新字段并写 v1，第一次实际 invocation commit 才原子升级 v2，已是 v2 则不降级。
必须增加 golden、损坏恢复、memory/file parity 和 crash conformance。

旧二进制仍能读取从未使用新能力而保持 v1 的会话，但不能读取已升级 v2 的会话，属于明确的局部
downgrade 限制。不得在 v1 名称下静默写新字段，也不得为回滚便利引入 sidecar store。组装产品应先
发 RC 并在发布说明中披露会话文件升级。

新增 diagnostics 方法、open/close receipt 和 typed input error 会改变 `GatewayAccess` 及 transport DTO，
属于明确的 pre-1.0 source/protocol compatibility 变化。in-process、CLI、gRPC、ACP adapter、测试替身和
conformance 必须同批更新，不能用错误文本或只在某个 Channel 私加字段来绕开公共合同。

## 成熟度与验收口径

Hook 相关能力按五层记录证据：

1. **Contracted**：typed contract 和框架单测成立。
2. **Runtime-wired**：固定 Runtime 在真实事务位置调用合同。
3. **Product-assembled**：组装产品安装真实消费者，不是只在测试中构造类型。
4. **User-configurable**：用户配置、执行、诊断和安全边界端到端成立。
5. **Regression-proven**：真实工作流与故障注入稳定通过。

只有第 4、5 层可以写成“产品 Hook 已交付”。当前 `agent.hook` 处于 Runtime-wired；LAS 是否达到
更高层级，应由 LAS 自己的组件覆盖与验收文档记录。

## 当前非目标

- 通用字符串事件总线或任意 JSON 状态修改协议；
- 让 Hook 成为第二个 Agent Loop、Tool Dispatcher、Policy 或 Session Store；
- 在 AgentSlot 内运行 Shell、HTTP、prompt Hook 或加载项目配置；
- 因参考产品存在某个事件名，就提前为尚无消费者的生态增加 Slot。
