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

## 转绿要求

| 编号 | 普通门禁中的最终测试 |
|---|---|
| ASR-REG-001 | JSON 值语义测试、Journal 完整迁移以及 memory/file store conformance 全部通过 |
| ASR-REG-002 | Recover 返回 idle 才允许 Runtime idle；running/inconsistent 快照 fail closed；显式 Resume 恢复路径通过 |
| ASR-REG-003 | StreamState、Runtime 持久化安全和 `model.executor/v1` 全部通过 |
| ASR-REG-004 | RunTermination 合同、旧数据读取、FileStore 往返和 Runtime 分类全部通过 |

## 账本纪律

- 红条目不能因为测试被跳过而写成“已修复”。
- 修复轮次必须先运行本页命令确认旧实现失败，再实施改动。
- 转绿后保留原 fixture 和测试名，删除 opt-in 条件，并把状态改为“普通门禁已覆盖”。
- 新事故必须给出最小事件序列；日志、真实凭据和用户源码不能作为仓库 fixture。
