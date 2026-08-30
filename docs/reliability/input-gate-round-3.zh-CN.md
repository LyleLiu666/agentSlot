# InputGate 框架验收记录

## 结论

AgentSlot 已具备 provider-neutral 的 `hook.InputGate` Chain，并把它接入固定 Gateway/Runtime 的 Send、Steer
和 EditQueued 事务。该能力只提供输入提交前的有限裁决和独立上下文，不包含 LAS 事件名、profile、命令
runner、Shell 协议或产品超时策略。

本轮最重要的不变量已经由自动化测试固定：用户消息进入 History 后仍严格 append-only；Hook context 不是
用户消息，只对认领它的精确 Run/Step 投影一次；同 Session 慢 Hook 不会阻塞 active Run，也不会阻塞其他
Session。

## 已实现合同

1. `hook.InputGateSlot` 是可选 Chain；每个 contribution 提供构建期冻结且同 Chain 唯一的 descriptor。
2. 一次输入 occurrence 在调用 component 前，把全部匹配 invocation 以一个 CAS commit 写为 prepared；该
   commit 是 caller `ExpectedRevision` 的线性化点。
3. component 只能返回 `accept`、`reject` 和有界 additional context，不能替换 `MessageInput`，也不能取得
   Runtime、Session 或 Store。
4. 每个 invocation 依次推进 prepared、pending、terminal，再由业务结果原子推进 effect/context disposition。
   panic、普通 error、声明式失败、caller cancellation 和 deadline 都形成稳定失败分类与安全诊断；如果
   component 在 cancellation 后明确声明外部副作用 `outcome_unknown`，Runtime 保留该更强结论，不把它
   降级成普通 canceled。
5. accept 后的 Send、Steer、EditQueued 使用内部最新 revision，并重新校验 active Run 或 Queue subject；
   Delete、claim 或其他 edit 获胜时，过期结果被拒绝和 discard。
6. Queue claim、用户 `AppendMessage`、Hook `AppendContextContribution` 和 context consumed 同 commit 完成。
   `historyInputs` 只投影当前 `RunID + StepID` 的 contribution，避免一次性上下文在后续模型请求中重复累积。
7. prepared 输入不会在重启后猜测重放；pending 外部结果不会重放；已入队且尚未 claim 的成功 context 可跨
   resume 保留，直到原 QueueItem 被 claim、edit 或 delete。
8. `interaction.InputGateError` 携带 SessionID、最终 CurrentRevision 和本 occurrence 的有界 diagnostics；gRPC
   往返保留类型、错误 code、revision 和 diagnostics。

## Append-only 与效率审计

| 检查项 | 结论 |
|---|---|
| 用户原文 | Gate 结果没有 replacement 字段；只允许编辑未 claim 的 QueueItem |
| History 消息 | 仅通过 `AppendMessage` 写入；Gate journal 推进不会编辑、删除或重排 History |
| Hook context | 通过独立 `ContextContributionFact` 追加，不伪装成用户发送消息 |
| context 消费 | 与 Queue claim 同 commit，仅对精确 Run/Step 投影一次 |
| 同 Session 并发 | 只串行 Send/Steer/EditQueued occurrence；active Run 不持有 submission mutex |
| 跨 Session 并发 | 每个 Runtime 自有 submission mutex，互不阻塞 |
| 无组件快路径 | 不增加 journal commit、goroutine、FileStore v2 升级或外部调用 |
| 有组件写放大 | 每条外部调用必须持久化 pending/terminal 以保证 unknown 不重放；这是恢复边界的必要成本，不另建 worker、重试器或 sidecar |

## 自动化证据

当前测试覆盖：

- accept/reject、上下文顺序、一次消费及跨 Step 不重复；
- Send、Steer、EditQueued 触发，RunPending、ReclassifyQueued、DeleteQueued 不误触发；
- ClientMessageID 重用仍产生不同 MessageID 和 invocation；
- 同 Session 串行、跨 Session 并行、active Run 与慢 Gate 并行；
- Edit/Delete 和 Edit/claim 竞态，不复活 Queue、不改写已追加 History、不串用 context；
- component error、panic、caller cancellation、短路后未启动 invocation 收口；
- descriptor 重复、typed nil、build 后变更冻结；
- prepared crash 不重放，以及已入队 pending context 在 resume 后由 RunPending 正确消费；
- MemoryStore/FileStore ExtensionJournal transition，以及 gRPC typed error 往返。

提交前必须通过：

```text
gofmt -w .
go test -race ./...
go vet ./...
```

## 明确不属于本轮

- 具名产品事件和命令执行 adapter；
- ToolPreflight、ToolResultHook、CompletionGate、SessionLifecycle；
- 产品级 occurrence 墙钟上限、输出限制与协议 DTO；
- 分布式 Session lease、横向扩展、后台重试或 journal 归档。

这些能力不能因为 `InputGate` 已实现就被描述为 AgentSlot 或组装产品已经交付。
