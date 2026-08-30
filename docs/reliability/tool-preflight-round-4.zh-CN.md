# ToolPreflight 框架验收记录

## 结论

AgentSlot 已把 `hook.ToolPreflight` 接入固定 Tool 事务：模型完成提交会原子写入 ToolCall、Tool RunJournal
prepared 和完整静态匹配的 Preflight reservation；component 只产生有限授权建议，Policy、Approval 和
Dispatcher 仍拥有最终执行权。

本轮同时固定了两个不能退让的非功能约束：Preflight 只追加新的持久事实和推进自己的 journal 状态，不能
改写任何已发布 History；未安装 contribution 时不增加 extension commit 或 goroutine，也不把
ParallelSafe Tool 变成串行 Tool。

## 已实现合同

1. `hook.ToolPreflightSlot` 是可选 Chain，每个 contribution 提供唯一 descriptor 和构建期冻结的
   `All` 或精确 Tool key scope。
2. view 是 detached DTO，包含 Session/Agent/Workspace、prepared revision、Run/Step/ToolCall、Tool key 和
   原始 JSON value 参数；result 只允许 allow、deny、require approval，不能返回替代 ToolCall。
3. ToolCall、Tool Journal prepared 和全部匹配的 ExtensionJournal prepared 在一个 Store commit 中形成；
   extension sequence 使用 O(n) 单调分配，不随批次重复扫描。
4. Tool 不存在或 schema 非法时不调用 Preflight，预约 canceled/discarded，既有 ToolResult code 不变。
5. 所有 Preflight 在任一 Tool 开始前，按 ToolCall 顺序和 Chain 顺序运行。deny 只短路当前 Tool；command、
   panic、非法结果或 unknown 是基础设施失败，整批 Tool 均不执行并产生 extension 归因的 RunInterrupted。
6. Hook deny 优先，require approval reason 与 Guard reason 合并；Guard deny 永远不能被 Hook allow 覆盖，
   ApprovalService 仍是唯一批准面。
7. prepared 只在 descriptor 与规范化 input digest 精确匹配时重放；pending 经 Store recovery 变为
   outcome_unknown 后只结算持久 effect，不再次调用 component 或原 Tool。
8. 旧版本 prepared ToolCall 完全没有 reservation 时，可以用当前冻结集合一次性补齐；任何 partial、重复或
   definition drift 都 fail closed。

## Append-only 与效率证据

- `TestToolPreflightNeverRewritesPreparedSessionHistory` 在外部 component 阻塞期间保存 History 前缀，完成后
  逐事实比较，证明 Message、ToolCall 和旧 Run facts 没有被回写、删除、插入或换位。
- ExtensionJournal 是恢复状态机，不是第二份模型 History；Preflight 不产生 RoleUser context，也不扩大后续
  模型请求。
- `TestNoToolPreflightKeepsParallelToolsAndCreatesNoExtensionState` 证明零 contribution 不产生 journal，两个
  ParallelSafe Tool 在彼此阻塞时仍能共同进入 Invoke。
- 有 contribution 时只串行授权建议；最终允许的 Tool batch 继续使用既有 ParallelSafety。预约 sequence 由
  一次游标递增分配，避免 ToolCall × Hook 数量增大时出现二次扫描。

## 恢复与失败证据

- prepared 重启后只调用一次 component 和一次原 Tool，原 ToolCall identity 不变；
- pending 重启后成为 outcome_unknown/effect applied，component 与 Tool 调用数均为零；
- 普通 deny 生成安全失败 ToolResult，但同批其他 Tool 继续；
- infrastructure failure 取消所有未开始 reservation，任何 Tool 都不进入 pending，Run 以
  `TerminationExtension` 中断；
- schema 失败保留 `invalid_arguments`，不把模型错误误归因成 Hook。

## 验证入口

```bash
go test -race ./hook ./session ./standardagent
go test -race ./...
go vet ./...
go build ./...
```

本记录只证明 AgentSlot 已达到 Contracted + Runtime-wired。具名 Profile、命令协议、用户诊断和真实进程
验收必须由 LAS 等组装产品在自己的仓库证明。
