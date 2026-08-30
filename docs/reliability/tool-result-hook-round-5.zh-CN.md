# ToolResultHook 框架验收记录

## 结论

AgentSlot 已把 `hook.ToolResultHook` 接入固定 Tool 结果事务。只有实际进入 pending 并返回合法
`succeeded | failed` 的 Tool 才触发；ToolResult、Tool Journal terminal、next Step identity 与完整静态
匹配 Post reservation 在一个 commit 中形成。component 只能提出有界下一 Step context，不能回滚或改写
ToolResult，也不能取得 Session/Runtime/Dispatcher 权力。

## 已实现合同

1. `hook.ToolResultHookSlot` 是可选 Chain。descriptor key 唯一；scope 在 build 时冻结为 exact/all Tool key
   与 succeeded/failed status 的有限组合，unknown 永不匹配。
2. detached view 包含 Session/Agent/Workspace、prepared revision、来源 Run/Step/Message/ToolCall、精确
   NextStepID、原始 JSON arguments 和深拷贝 ToolResult；digest 按 JSON 值语义规范化。
3. schema/Policy/Approval/Pre deny 从未进入 Tool pending，不产生 Post reservation；恢复生成的
   outcome_unknown 同样不触发外部 component。
4. Post 按 ToolCall order、再按 Chain order 串行。全部成功后，所有 context 在一个 commit 中按相同顺序
   追加到 NextStep，并清除 journal payload、保留 digest/bytes 与 consumed 状态。
5. 任一 component error、panic、非法 result 或 outcome unknown 保留原 ToolResult，discard 先前 context，
   cancel 尚未调用 entry，并以 `TerminationExtension` 阻止下一模型请求。
6. Cancel 在 Post 执行期间胜出；当前 invocation 仍持久化有限终态，其余预约全部收口，已放弃 Step 不消费
   context。

## Append-only 与效率

- 阻塞 component 回归在 ToolResult commit 后保存完整 History 前缀，结束后逐事实比较；Post 只在尾部追加
  ContextContribution/RunFact，不修改 Message、ToolCall、ToolResult 或旧 sequence。
- ExtensionJournal 是恢复状态，不是模型消息。ContextContribution 只投影到精确 NextStep 一次，不会随
  append-only History 在后续 Step 重复扩大 token。
- 无 contribution 时 reservation helper 立即返回，不增加 extension change、Store commit、goroutine 或
  外部等待；Tool 原 ParallelSafety 与结果顺序不变。
- 有 contribution 时只串行 Post component；此前 Tool batch 仍按原并行规则执行。scope 在 build 时冻结，
  运行期只做有界线性匹配。
- FileStore 的完整文档写入/fsync 是真实成本。对一批共 `N` 条成功 Post invocation，Runtime 将“上一条
  terminal + 下一条 pending”合并为一个原子 commit，使 result commit 之后的 journal 状态推进由
  `2N+1` 次降为 `N+2` 次（首次 pending、`N` 次 terminal、一次 aggregate context consume）；任何外部
  command 前仍有 durable pending，合并不会缩短 outcome-unknown 恢复边界。

## 故障与恢复证据

- `tool-results` 原子 commit 失败不会发布半个 ToolResult/Post 集合；
- pending commit 失败不调用 component，并取消 reservation；
- command 已执行而 terminal commit 返回失败时，Runtime 重读权威 entry；仍 pending 的 invocation 变为
  outcome_unknown/effect applied，绝不重放；
- context consume commit 失败时，已提交 ToolResult 保持原字节，context discard，Run 不进入下一模型请求；
- 进程恢复对 prepared、pending、succeeded/effect-pending 三种状态都只做结算，不调用 component，也不把
  context 投给已中断 Run 的 NextStep；
- 每条失败路径最终都没有 prepared/pending 或 effect/context pending 残留，避免“Session idle 但无终态”。

## 验证入口

```bash
go test -race ./hook ./session ./standardagent \
  -run 'TestToolResult|TestPostHook|TestCancelWinsWhileToolResultHook' \
  -count=1
go test -race ./...
go vet ./...
go build ./...
```

本记录只证明 AgentSlot 的通用 Contracted + Runtime-wired 能力。具名 PostToolUse/PostToolUseFailure 配置、
命令 DTO、超时和真实进程 E2E 必须由 LAS 等组装产品在自己的仓库证明。
