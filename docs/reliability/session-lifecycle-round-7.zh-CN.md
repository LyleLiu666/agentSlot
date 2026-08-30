# SessionLifecycle 第 7 轮实现合同

## 结论

`hook.SessionLifecycle` 已进入固定 Runtime，负责一次 Runtime instance 的 SessionStart 与明确
`GatewayAccess.CloseSession` 的 SessionEnd。它是通用框架事务边界，不包含任何具名产品命令、配置或文案。

## 公开合同

- Slot：`hook.SessionLifecycleSlot`，有序可选 Chain。
- 构建期元数据：唯一 `ExtensionDescriptor` 与有限 `LifecycleScope{open, close}`；构建后冻结。
- Open kind：`create | resume | fork | summary`；close 不携带 open kind。
- 输入：Invocation、Session、Agent、Workspace、prepared revision、phase/open kind 的只读深拷贝。
- 输出：open 可返回受统一 Hook 上限约束的 context；close 必须返回空 context。
- `SessionOpened` 与 `CloseSessionReceipt` 返回最终 revision 和本次 occurrence 的安全 diagnostics。

## 固定执行顺序

### Open

1. Manager 创建或恢复 Session，并构造 `opening` Runtime。
2. Registry 先登记唯一 Runtime；并发 Gateway 操作等待同一个 open barrier。
3. Runtime 恢复旧 lifecycle entry，再以一个 commit 预约全部匹配 Start component。
4. Chain 以 terminal + next-pending 流水推进，最终 aggregate apply；正常 N 条链为 N+2 次 commit。
5. Start 收口后才恢复 InputGate/ToolResultHook 与旧 prepared Run；最后开放 barrier。
6. Open context 与首个 RunStarted、用户输入在同一 Run 启动事务追加，绑定精确 Run/Step。

### Close

1. caller 的 `ExpectedRevision` 在 Runtime mutex 下校验。
2. 同一 revision 预约全部匹配 End component，然后设置 closing；没有 component 时没有空 commit。
3. Runtime 禁止新操作，取消 active Run 和已登记 Prompt Hook；允许这些旧工作提交终态。
4. Run/Prompt 全部结算后执行 End，形成 final receipt，最后停止事件和观察设施。
5. End component error/panic/cancel 只形成安全诊断；Store/恢复收敛失败才使 CloseSession 返回 error。

应用 Stop、transport 断开、订阅结束和进程崩溃都只释放 Runtime，不执行 End。

## Append-only 与效率边界

- Lifecycle 从不修改、删除、重排旧 History 或 Message。
- Context 以独立 `ContextContributionFact` 追加一次；Journal 只推进审计状态。
- Context 仅进入首个精确 Run/Step，后续模型 Step 不再次携带。
- 同 descriptor 在新 open 产生新 context 时，尚未消费的旧 context 标为 discarded，避免反复 resume
  在首次模型请求前堆积重复输入；新 open 没有 context 时不丢弃仍有效的旧值。
- 无匹配 lifecycle 时，不创建 ExtensionJournal、不增加 Session commit，也不启动 lifecycle goroutine。

## 恢复矩阵

| 旧状态 | 恢复结果 | 是否重放 |
|---|---|---:|
| prepared | canceled + effect discarded | 否 |
| pending | outcome_unknown + effect discarded | 否 |
| terminal + effect pending | effect/context discarded，防止半条 Chain 生效 | 否 |
| succeeded + effect applied + context pending | 保留到首个 Run，或被同 component 新 context 替代 | 否 |
| effect/context 已结算 | 保持不变 | 否 |

新 Runtime 的 Start 是新的 occurrence，不是对旧 invocation 的重放。

## 验收入口

```bash
go test -race ./hook ./session ./standardagent ./interaction/grpcchannel ./interaction/acpchannel \
  -run 'TestSessionLifecycle|TestRemoteProfileDispatchesEveryGatewayOperation|TestACPWire' \
  -count=1
```

完整发布门禁仍为 `gofmt -w .`、`go test -race ./...`、`go vet ./...` 和全仓 build。
