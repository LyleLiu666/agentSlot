# Agent 架构复核迁移核对单

## 1. 用途与生命周期

2026-08-20 的架构复核已经结束，全部结论已进入权威的
[架构讨论](agent-architecture-discussion.zh-CN.md)、
[全景架构](agent-framework-architecture.zh-CN.md)和
[实施计划](agent-runtime-standard-slots-implementation-plan.zh-CN.md)。

本文不再保存待决问题，也不是第二份规范；它只在第 0～6 轮迁移期间防止漏项。每个
结论是否真正完成，以代码、测试和中英文组件地图同时一致为准。第 6 轮收口后删除本文
以及架构讨论中的链接。

## 2. 已定案内容

| 编号 | 最终决定 | 迁移轮次 |
| --- | --- | --- |
| D-001 | SessionManager 固定；SessionStore 是可替换 One Slot | 2 |
| D-002 | chunk/reset 永不持久化；完整消息和 Attempt 才成为事实 | 1、3、4 |
| P-001 | 不新增 Ledger；完整 Session History 是唯一 append-only 事实序列 | 1 |
| P-002 | 所有外部写命令共用严格 SessionRevision CAS | 1、4 |
| P-003 | ContextSource 只为新 Step 追加，先写 ContextContributionFact | 3 |
| P-004 | ToolKeys 是显式严格白名单，nil/空/未配均为无工具 | 5 |
| P-005 | 临时流可丢；持久提交只通知 SessionID+Revision，UI 再读 View | 4 |
| P-006 | ActorIdentity 记录来源；Channel 负责远程认证，不保存凭据 | 1、4 |
| P-007 | Hook 只留 BeforeRunComplete；提交观察拆成独立 Chain | 5 |
| P-008 | ContextVersion 可解释；LatestOnly/RetainAll 决定保留范围 | 1、3 |
| P-009 | 只提供 MaxTokensPerRun；不设其他标准 Run/Queue 上限 | 3 |
| F-001 | Fork 支持完整历史与合法 HistorySequence 检查点 | 2 |
| G-001 | 固定 Gateway 不监听网络；GatewayChannel 是唯一接入 Slot | 4 |

## 3. 迁移核对

- [x] FileStore 已升级到 `agentslot.session-file/v1` 并明确拒绝 v0；
- [x] HistoryFact、Attempt、TokenUsage、ContextVersion 和分页合同已落地；
- [x] 公共 `session.manager` Slot 与接口已删除；
- [x] 完整 Fork、检查点 Fork、协议边界和 usage 来源已验证；
- [ ] AttemptRecorder、Context retention 和 MaxTokensPerRun 已落地；
- [ ] `gateway.channel` 已替代旧 Entrypoint 和重复 Gateway 子 Slot；
- [ ] 所有外部写命令均执行严格 CAS，View 与历史分页已验证；
- [ ] Hook、CommitObserver、运维事实和 ToolKeys 已按新语义收敛；
- [ ] 参考 Agent 和端到端测试覆盖完整目标链路；
- [ ] `COMPONENT_MAP.md` 与 `COMPONENT_MAP.zh-CN.md` 最终为 37 个生态位、16 个 Contracted；
- [ ] README、ROADMAP 和实现状态已同步；
- [ ] 本文及所有指向本文的链接已删除。

## 4. 不构成开发门禁

具体远程 wire protocol、生产认证平台、分布式 Session 所有权、部署拓扑、运维告警和
发布流程不阻塞本迁移。它们不能被用来拖延本地公共合同、确定性实现、race、vet 和
崩溃安全验证。
