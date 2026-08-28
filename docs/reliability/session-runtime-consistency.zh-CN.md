# Session 与 Runtime 一致性设计

## 一句话结论

AgentSlot 必须按稳定身份和 JSON 值语义校验 ToolCall，并且只有在恢复后的持久快照已经证明可接收新 Run 时，Runtime 才能进入 idle。

## 文档角色

本文细化 [ASR-001、ASR-002 与 ASR-003](README.zh-CN.md)，定义 SessionStore、RunJournal 和固定 Runtime 必须共同遵守的行为。它不决定 LAS 的恢复界面，也不引入分布式执行协议。

## 故障基线（第 2 轮已修复）

### ToolCall 表示被误当成身份

旧 Journal 更新会逐字段比较 ToolCall，并对 `Arguments` 使用原始字节相等。合法 JSON 经 file-store 写入后可能被压缩空白或改变等价转义，因此同一 `ToolCallID` 曾被错误判定为“改变了 prepared tool identity”。

这不是 file store 的特殊兼容需求，而是 Session 聚合错误地把序列化表示当成了业务身份。

### Recover 成功被误当成 Runtime 可 idle

`Store.Recover` 的成功只表示恢复事务本身完成，不表示返回快照一定 idle。当活动 Run 仍有 `prepared` ToolCall 时，Store 会保留 running，等待新 Runtime 按原调用身份恢复。

旧活动 Runtime 在 Run 收尾提交失败后调用 `Recover`，只要调用没有返回错误，就可能直接把自身状态改成 idle，没有检查恢复快照仍是 running。这会产生“内存可接收新 Run、持久 Session 仍有活动 Run”的分裂状态。

## 设计决定

### 1. ToolCall 身份由稳定字段决定

以下字段使用精确相等：

- `ToolCallID`
- `CorrelationID`
- `MessageID`
- `SessionID`
- `RunID`
- `StepID`
- 内部工具名

`Arguments` 不是新的身份来源，但 Journal 在状态迁移时必须证明它仍表达最初 admission 的同一个 JSON 值。

禁止根据工具名和参数重新计算 `ToolCallID`。持久 ID 由 Runtime 分配；内容比较只用于阻止同一 ID 被替换成另一项调用。

### 2. Arguments 按 JSON 值语义比较

比较规则如下：

- 对象成员顺序不影响结果；
- JSON token 之间不影响值的空白不影响结果；
- Unicode 字符与其等价 JSON 转义表达相同；
- 数组顺序必须一致；
- string、number、boolean、null 类型不能互换；
- JSON number 按精确数学值比较，不通过 `float64` 造成精度损失；
- 重复对象成员名因语义不唯一，在进入 Journal 前拒绝；
- 非法 JSON 在生成持久 ToolCall 或执行工具之前拒绝。

实现采用 AgentSlot 私有的 JSON 值比较能力，不新增公共“canonical JSON”组件或 Slot。History 可以保留首次提交的原始参数字节；框架承诺语义稳定，不承诺不同 Store 读取后的字节完全相同。

### 3. 不通过改写历史修复旧 Session

旧 Session 文件继续原样保留。读取、Journal 比较和恢复路径同时使用新语义，因此不需要批量重写 ToolCall，也不改变已经发布给用户或模型的 History。

如果旧数据含有非法或重复成员 JSON，Session 加载可以保留原事实，但任何需要把它重新 admission、执行或推进 Journal 的路径必须 fail closed，并返回稳定的 Session 完整性错误。

### 4. Store.Recover 返回的是事实，不是 Runtime 指令

Runtime 在 `Recover` 后必须检查完整快照，不能只检查 `error == nil`。

| 恢复后持久状态 | 解释 | 要求的 Runtime 行为 |
|---|---|---|
| idle，上一 Run 已有唯一终态 | Session 已收敛 | 可以进入 idle |
| running，只有属于活动 Run 的 prepared ToolCall 未完成 | 副作用尚未开始，需按原身份恢复 | 当前 Runtime fail closed；显式 Close/Resume 建立新执行控制状态 |
| running，存在 pending ToolCall | 副作用可能发生，恢复事务尚未完成或快照不可信 | 禁止 idle；再次恢复或关闭 Runtime |
| running，但没有可恢复 prepared 证据 | Run 终态恢复未收敛 | 禁止 idle并报告 Session 完整性错误 |
| idle，但 started Run 没有终态 | Store 违反 Run 不变量 | 禁止 idle并报告 Session 完整性错误 |
| 任意不一致 ID、Journal 或 revision | 聚合事实冲突 | fail closed，不自动请求模型掩盖 |

本阶段不要求活动 Runtime 在原进程内自动重建丢失的 Loop 控制状态。最小安全行为是关闭当前 Runtime，让显式 Resume 从持久快照重新建立控制状态。

### 5. Run 终态与 idle 原子提交

正常收尾时，同一个 Session commit 必须同时包含：

1. 唯一 Run terminal fact；
2. `RunState running → idle`；
3. 与该收尾绑定的 Queue reclassification 或下一 Run claim（若存在）。

任何部分失败都不允许 Runtime 单独发布 idle。Store 必须拒绝以下状态：

- idle 且当前 Run 没有终态；
- 同一 Run 有多个终态；
- terminal fact 与 active Run ID 不一致；
- terminal fact 改变 Run 开始时冻结的配置；
- 新 Run 在旧 Run 尚未原子收尾前启动。

### 6. prepared 与 pending 的恢复边界保持不变

- `prepared`：工具副作用尚未开始，可以使用原 `ToolCallID` 恢复执行。
- `pending`：工具已获得执行权，结果可能丢失；恢复时不得自动重放。
- `pending` 且 History 没有结果：追加 unknown ToolResult，将 Journal 转为 `outcome_unknown`。
- 发现 `outcome_unknown` 后，Run 可以 interrupted 收敛，但产品是否提供对账由组装项目决定。

这条规则同时适用于串行与并行工具批次。并行批次不能因为其中一项 prepared 就重放其他已经 pending 的调用。

## Store 合同影响

`SessionStore` 公共方法不需要新增参数。需要加强的是已有合同的行为与 conformance：

- Store 必须保存合法 JSON 的值语义，不能要求调用方依赖其字节格式。
- CAS 与 idempotency key 继续保护提交事务；JSON 语义比较不替代 revision 检查。
- MemoryStore 与 FileStore 必须对相同 Change 序列产生相同 Snapshot 语义。
- 自定义 Store 可以使用不同编码，但恢复出的 ToolCall 必须满足同一 JSON 值语义。

## Runtime 合同影响

不增加新的公开 Runtime 状态。`runtimeClosed` 继续作为无法证明安全时的内部终态，Gateway 对关闭 Runtime 的后续写操作返回稳定 `runtime_closed` 或 `session_unrecoverable` 分类。

可增加一条不含敏感内容的 trace，说明关闭原因是“恢复后持久 Run 仍活动”或“恢复快照不满足终态不变量”；trace 不能取代持久事实。

## TDD 与 conformance 场景

### JSON 身份

- 空白不同但语义相同；
- 对象键顺序不同；
- `"中"` 与等价 Unicode escape；
- `/` 与 `\/` 等价转义；
- 数字普通形式与等价指数形式；
- 超出 `float64` 精确范围的大数；
- 数组元素换序应冲突；
- string `"1"` 与 number `1` 应冲突；
- 重复对象成员和非法 JSON 在 admission 前失败。

### Journal 与恢复

- file-store reopen 后 `prepared → pending → succeeded`；
- file-store reopen 后 `prepared → pending → failed`；
- prepared 中断后以相同 ToolCallID 恢复；
- pending 中断后得到 unknown result，不调用 Tool；
- 并行批次混合 prepared/pending 时只恢复安全项；
- 终态提交前注入 revision 冲突，Runtime 不进入 idle；
- `Recover` 返回 running+prepared 时，旧 Runtime closed，显式 Resume 能接管；
- 构造 idle+unterminated Run 的非法 Store，Runtime 拒绝开放。

### Store conformance

同一测试向 MemoryStore、FileStore 和未来实现提交完全相同的 Change 序列，比较：

- RunState 与 ActiveRunID；
- History fact 种类、身份和顺序；
- Journal 状态及 JSON 值语义；
- Queue claim/reclassification；
- revision 单调性；
- reopen/recover 后的最终快照。

不得使用直接比较完整序列化文件作为 conformance 判据。

## 兼容性

- 不改变既有 History 的追加语义。
- 不更换已持久 ToolCallID、RunID、StepID 或 MessageID。
- FileStore schema 无需仅因 JSON 语义比较升级版本。
- 自定义 Store 若此前改变了 JSON 的实际值而非表示，将在新 conformance 中失败；这属于修复合同违规，不提供兼容豁免。
- Runtime 从错误 idle 改为 closed 是有意的安全收紧，调用方必须处理既有的关闭错误，不增加静默回退。

## 非目标

- 不让 Store 自动查询外部工具副作用。
- 不实现跨进程 Tool lease 或 exactly-once。
- 不把整个 Session 文档转换成 canonical JSON。
- 不公开通用 JSON canonicalization API。
- 不在自动恢复中编辑或删除旧 History。

## 完成定义

- 历史 JSON 表示事故先红后绿，并进入 Store conformance。
- Runtime 可接受新 Run 时，持久 Session 必为 idle，最近 Run 必有唯一终态。
- 所有 pending 中断测试证明工具没有被自动重放。
- MemoryStore、FileStore 和消费方 LAS 的 file-session 回归同时通过。
- `COMPONENT_MAP.md` 与中文版同步说明加强后的恢复和 Store 语义。
