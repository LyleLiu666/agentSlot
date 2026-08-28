# AgentSlot 可靠性回归账本

## 文档角色

本账本记录已经有代码证据的框架故障、最小复现入口、责任轮次和转绿条件。它不是产品缺陷清单，也不登记具名 Provider 的 wire 差异。

正常测试默认跳过仍待修复的红回归；显式设置 `AGENTSLOT_RUN_KNOWN_FAILURES=1` 才运行。责任轮次完成时必须移除对应跳过条件，让回归进入普通门禁。

## 当前条目

| 编号 | 设计要求 | 触发条件 | 当前错误 | 责任轮次 | 状态 |
|---|---|---|---|---|---|
| ASR-REG-001 | ASR-001、ASR-002 | 同一 ToolCall 的 JSON 参数经等价重排、压缩和转义变化后推进 Journal | `sameToolCall` 使用原始字节比较，误报 prepared identity 被修改 | 第 2 轮 | 已稳定复现，红 |
| ASR-REG-002 | ASR-003 | Run 收尾提交失败后，`Store.Recover` 返回仍为 running 的快照 | Runtime 只检查 Recover error，随后错误进入 idle | 第 2 轮 | 已稳定复现，红 |

## 确定性复现

### ASR-REG-001

```bash
AGENTSLOT_RUN_KNOWN_FAILURES=1 go test ./session \
  -run TestKnownFailureToolCallIdentitySurvivesEquivalentJSONRepresentation \
  -count=1
```

预期当前失败签名：`equivalent JSON representation changed the prepared ToolCall identity`。

fixture：

- `session/testdata/reliability/tool-arguments-pretty.json`
- `session/testdata/reliability/tool-arguments-restored.json`

### ASR-REG-002

```bash
AGENTSLOT_RUN_KNOWN_FAILURES=1 go test ./standardagent \
  -run TestKnownFailureRuntimeDoesNotIdleOnRunningRecoveredSnapshot \
  -count=1
```

预期当前失败签名：Runtime state 为 `idle`，期望 `closed`。

## 转绿要求

| 编号 | 普通门禁中的最终测试 |
|---|---|
| ASR-REG-001 | JSON 值语义测试、Journal 完整迁移以及 memory/file store conformance 全部通过 |
| ASR-REG-002 | Recover 返回 idle 才允许 Runtime idle；running/inconsistent 快照 fail closed；显式 Resume 恢复路径通过 |

## 账本纪律

- 红条目不能因为测试被跳过而写成“已修复”。
- 修复轮次必须先运行本页命令确认旧实现失败，再实施改动。
- 转绿后保留原 fixture 和测试名，删除 opt-in 条件，并把状态改为“普通门禁已覆盖”。
- 新事故必须给出最小事件序列；日志、真实凭据和用户源码不能作为仓库 fixture。
