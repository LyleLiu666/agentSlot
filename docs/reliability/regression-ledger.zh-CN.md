# AgentSlot 可靠性回归账本

## 文档角色

本账本记录已经有代码证据的框架故障、最小复现入口、责任轮次和转绿条件。它不是产品缺陷清单，也不登记具名 Provider 的 wire 差异。

已修复条目必须保留最小复现，并进入普通测试门禁。新的未修复条目才允许短期使用显式 opt-in，且必须写明转绿责任轮次。

## 当前条目

| 编号 | 设计要求 | 触发条件 | 当前错误 | 责任轮次 | 状态 |
|---|---|---|---|---|---|
| ASR-REG-001 | ASR-001、ASR-002 | 同一 ToolCall 的 JSON 参数经等价重排、压缩和转义变化后推进 Journal | 旧实现按原始字节比较，误报 prepared identity 被修改 | 第 2 轮 | 已修复，普通门禁已覆盖 |
| ASR-REG-002 | ASR-003 | Run 收尾提交失败后，`Store.Recover` 返回仍为 running 或不一致的快照 | 旧 Runtime 只检查 Recover error，随后错误进入 idle | 第 2 轮 | 已修复，普通门禁已覆盖 |
| ASR-REG-003 | ASR-004、ASR-007 | ModelStream 在 delta 后直接关闭、切换 Attempt 未 reset、重复终态或返回非法 Completion | 临时或不完整输出可能越过通用边界，且各消费者校验语义可能漂移 | 第 5 轮 | 已修复，普通门禁已覆盖 |
| ASR-REG-004 | ASR-005 | Runtime 因 Model、Context、Loop、Tool、Budget、Session 或取消失败 | Run 只有 failed/interrupted/canceled 枚举，消费方只能猜测原始错误字符串 | 第 5 轮 | 已修复，普通门禁已覆盖 |
| ASR-REG-005 | ASR-002、ASR-007 | FileStore 临时文件发生短写、sync/close/rename 前失败，或 rename 后目录 sync 失败 | 短写可能发布截断文档；故障点没有可重复的原子性与幂等语义证据 | 第 9 轮 | 已修复，确定性发布门禁已覆盖 |
| ASR-REG-006 | ASR-002、ASR-008 | ExtensionJournal 状态推进、FileStore 首次升级或进程在 pending 后退出 | 身份可能因 JSON 表示漂移；半个 v2 可能发布；pending 可能被重放；扩展 context 可能污染 History | Hook 自动化第 1 轮 | 已修复，普通门禁已覆盖 |
| ASR-REG-007 | ASR-002、ASR-008 | InputGate accept/reject、慢 Hook、Queue edit/delete/claim 竞态或重启 | 输入可能在 CAS 推进后半可见；旧 context 可能串到新内容；一次性 context 可能在每个后续模型请求中重复投影并放大 token | Hook 自动化第 3 轮 | 已修复，普通门禁已覆盖 |
| ASR-REG-008 | ASR-002、ASR-008 | ToolPreflight deny/approval、并行 Tool batch、schema 失败或 pending 后重启 | Tool 可能绕过 Preflight；allow 可能覆盖 Policy；pending 外部命令可能重放；全批工具可能被无谓串行化 | Hook 自动化第 4 轮 | 已修复，普通门禁已覆盖 |
| ASR-REG-009 | ASR-002、ASR-008 | ToolResult 已提交、Post pending/terminal/context consume 任一切点失败或重启 | ToolResult 可能被回滚/改写；Post command 可能重放；半套 context 可能进入下一模型请求；idle Session 可能残留未决 entry | Hook 自动化第 5 轮 | 已修复，普通门禁已覆盖 |
| ASR-REG-010 | ASR-002、ASR-008 | Session create/resume/fork/summary/close、opening 并发、Prompt/Run 未结算、生命周期链中途崩溃 | SessionStart 可能晚于模型或命令；SessionEnd 可能越过未结算 Hook；旧 context 可能重复进入后续 Step；断连或停机可能伪造 End | Hook 自动化第 7 轮 | 已修复，普通门禁已覆盖 |
| ASR-REG-011 | ASR-002、ASR-008 | 同次恢复包含 lifecycle 未决、active Run prepared、Post terminal/context pending，或多入口读取诊断 | 各边界重复全表扫描和提交；旧命令/context 可能重放；History 可能被误写；Session/Run/page/Audit 状态可能漂移且 journal 大小不可观测 | Hook 自动化第 8 轮 | 已修复，普通门禁已覆盖 |

## 确定性复现

### ASR-REG-001

```bash
go test ./session \
  -run TestKnownFailureToolCallIdentitySurvivesEquivalentJSONRepresentation \
  -count=1
```

预期：通过；同义 JSON 可完成 Journal 状态迁移，重复对象成员在 admission 前被拒绝。

fixture：

- `session/testdata/reliability/tool-arguments-pretty.json`
- `session/testdata/reliability/tool-arguments-restored.json`

### ASR-REG-002

```bash
go test ./standardagent \
  -run TestKnownFailureRuntimeDoesNotIdleOnRunningRecoveredSnapshot \
  -count=1
```

预期：通过；只有恢复出 idle 且原 Run 有唯一终态时 Runtime 才能回到 idle，其余快照 fail closed。

### ASR-REG-003

```bash
go test ./model ./model/modeltest ./model/openaicompat ./standardagent \
  -run 'TestStreamState|TestRun|TestExecutorPassesPortableModelConformance|TestRuntimeRejectsModelStream' \
  -count=1
```

预期：通过；非法或无终态流被拒绝，临时输出不会进入 History，参考 Executor 通过 `model.executor/v1`。

### ASR-REG-004

```bash
go test ./session ./standardagent \
  -run 'TestRunTermination|TestNewNonSuccessfulRunCommitRequiresTermination|TestFileStorePersistsRunTermination|TestRuntime.*Termination' \
  -count=1
```

预期：通过；新非成功 Run 必须保存通用 termination，旧数据仍可读取，未分类原始错误不会持久化，FileStore 重开后事实不丢失。

### ASR-REG-005

```bash
go test ./session -run TestFileStoreFaultInjection -count=1
```

预期：通过。短写以及 rename 前的 sync、close、取消和 rename 故障均不得改变旧 revision，临时文件必须清理；rename 已完成但目录 sync 失败时返回 unavailable，重试同一幂等请求可以观察已经发布的 commit，而不会重复追加事实。注入点只存在于 `session` 包私有文件边界，不成为公共 Slot 或生产配置。

### ASR-REG-006

```bash
go test ./hook ./session ./standardagent ./interaction/grpcchannel \
  -run 'TestTypedInputFingerprint|TestExtension|TestFileStore.*V2|TestGatewayProjectsBoundedExtension|TestRemoteProfileDispatchesEveryGatewayOperation' \
  -count=1
```

预期：通过。语义相同 typed input 得到同一 digest；journal 身份和 sequence 不变；状态、effect、context 分别单向推进；pending 恢复为 outcome_unknown 且第二次恢复不再变化；History/message 不被改写；空 journal 保持 v1，首次 entry 以原子 rename 升级 v2，损坏或混淆格式 fail closed；Gateway 只返回有界安全诊断。

### ASR-REG-007

```bash
go test -race ./hook ./session ./standardagent ./interaction/grpcchannel \
  -run 'TestInputGate|TestQueueClaimCanWin|TestDeleteCanWin|TestRunPendingDoesNotTrigger|TestResumeRetainsApplied|TestRemoteChannelPreservesInputGate' \
  -count=1
```

预期：通过。Send/Steer/EditQueued 共享持久 CAS gate；拒绝和失败不追加输入；Queue claim 只追加一次原消息与
独立 context；Delete/claim 获胜后 stale edit 不复活或改写消息；同 Session 输入串行但 active Run 和其他
Session 不被阻塞；ContextContribution 只投影到精确 Run/Step；prepared/pending 不重放；typed error 的
SessionID、当前 revision 和安全 diagnostics 可经 gRPC 往返。

### ASR-REG-008

```bash
go test -race ./hook ./session ./standardagent \
  -run 'TestToolPreflight|TestToolCallAndAllPreflight|TestInvalidToolArguments|TestPreparedToolPreflight|TestPendingToolPreflight|TestNoToolPreflight|TestPipelinedToolPreflight' \
  -count=1
```

预期：通过。ToolCall 与完整 Preflight reservation 同 commit；schema 失败不调用 component；deny 不执行原
Tool 且不终止同批其他合法 Tool；require approval 与 Guard reason 合并且仍经唯一 ApprovalService；基础设施
失败在任何 Tool 执行前收口并以 extension 归因中断；prepared 精确恢复一次，pending 变 unknown 后不重放；
History 前缀不改写；相邻 finished/pending 合并提交且提交失败后没有 idle + pending effect；零 contribution
不产生 ExtensionJournal，ParallelSafe Tool 仍并行。

### ASR-REG-009

```bash
go test -race ./hook ./session ./standardagent \
  -run 'TestToolResult|TestPostHook|TestCancelWinsWhileToolResultHook|TestPipelinedToolResult' \
  -count=1
```

预期：通过。只有真正执行并形成 succeeded/failed 的 Tool 触发匹配 Post；ToolResult、terminal Journal、next
Step 与完整 reservation 集合原子提交；Post context 按 ToolCall/Chain 顺序一次性进入精确 next Step。四个
持久切点失败均保留原 ToolResult 并收口全部 entry；相邻 terminal/pending 合并后故障仍不调用下一 Hook；
pending/commit outcome unknown 不重放 command；中途
失败和 Cancel 丢弃半套 context；History 前缀不改写；零 contribution 保持原结果提交快路径。

### ASR-REG-010

```bash
go test -race ./hook ./session ./standardagent ./interaction/grpcchannel ./interaction/acpchannel \
  -run 'TestSessionLifecycle|TestRemoteProfileDispatchesEveryGatewayOperation|TestACPWire' \
  -count=1
```

预期：通过。唯一 opening Runtime 在注册后、任何 Gateway/structured command 执行前完成 SessionStart；
create/resume/fork/summary 的 open kind 可审计，fork/summary 不复制父 Session 的 ExtensionJournal。Start
context 只追加到首个精确 Run/Step，同 component 的新未消费 context 替代旧值，后续 Step 不重复投影。
明确 CloseSession 在 caller CAS 上一次性预约完整 End chain，取消并结算 Run/Prompt 后才执行 End；组件失败进入
open/close receipt 的安全诊断但不阻断安全开关，持久化失败仍返回错误。prepared/pending/部分 terminal 链恢复
后不重放、不泄漏半套 context；应用停机、断连和崩溃不伪造 End。零 contribution 不增加 Session commit。

### ASR-REG-011

```bash
go test -race ./observe ./interaction/grpcchannel ./standardagent \
  -run 'TestExtensionObservation|TestExtensionDiagnosticsUseOne|TestRunDiagnostics|TestRemoteChannelPreservesRun|TestResumeUsesOnePlanned|TestResumeConvergesLifecycle' \
  -count=1
```

预期：通过。Runtime open 一次分类 ExtensionJournal；旧 lifecycle 在当前 Start 前收口，Prompt/Post 在
Start 后共用一个恢复 commit，prepared Pre/Completion 只按持久 Run 证据恢复。组合恢复不重放旧命令，
不把旧 context 送入模型，Run 形成终态，恢复前 History 保持为恢复后 History 的逐项相同前缀。
SessionView 最近 32 条、独立 page、RunResult 当前 Run 最多 100 条、Audit transition 使用同一个 detached
diagnosis；gRPC 不丢字段。Metric 只发布 entry/serialized-byte gauge，不发布 journal payload。

## 转绿要求

| 编号 | 普通门禁中的最终测试 |
|---|---|
| ASR-REG-001 | JSON 值语义测试、Journal 完整迁移以及 memory/file store conformance 全部通过 |
| ASR-REG-002 | Recover 返回 idle 才允许 Runtime idle；running/inconsistent 快照 fail closed；显式 Resume 恢复路径通过 |
| ASR-REG-003 | StreamState、Runtime 持久化安全和 `model.executor/v1` 全部通过 |
| ASR-REG-004 | RunTermination 合同、旧数据读取、FileStore 往返和 Runtime 分类全部通过 |
| ASR-REG-005 | FileStore 短写、rename 前失败、取消、rename 后模糊结果和幂等观察矩阵全部通过 |
| ASR-REG-006 | ExtensionJournal 状态机、append-only History、memory/file parity、v1/v2、恢复与 Gateway 分页全部通过 |
| ASR-REG-007 | InputGate CAS、append-only message、一次 context、并发竞态、取消/panic、恢复和 typed transport 全部通过 |
| ASR-REG-008 | ToolPreflight 原子预约、静态 scope、Policy/Approval 合并、批次失败、append-only、恢复和零开销快路径全部通过 |
| ASR-REG-009 | ToolResult/Post 原子预约、status scope、一次 context、四切点故障、Cancel、append-only、恢复和零开销快路径全部通过 |
| ASR-REG-010 | opening barrier、四种 open、明确 close、receipt、Run/Prompt 排空、一次 context、部分链恢复、transport 和零开销快路径全部通过 |
| ASR-REG-011 | 单次分类恢复、组合 recovery、append-only History、Session/Run/page/Audit 同投影、transport 和 entry/byte gauge 全部通过 |

## 账本纪律

- 红条目不能因为测试被跳过而写成“已修复”。
- 修复轮次必须先运行本页命令确认旧实现失败，再实施改动。
- 转绿后保留原 fixture 和测试名，删除 opt-in 条件，并把状态改为“普通门禁已覆盖”。
- 新事故必须给出最小事件序列；日志、真实凭据和用户源码不能作为仓库 fixture。
- `scripts/reliability-gate.sh` 是无外网、无 Provider 凭据的确定性发布入口；`--list` 只展示固定阶段，不执行门禁。
