# session

## 职责
会话存储契约(Store 接口)+ MemoryStore 参考实现。
**不做**:具体存储实现(adapter/gorm 或集成方自实现)、HTTP(gateway)、
**工作上下文组装 / token 估算 / 两级压缩——这些落在 core**(见下"实现细节记录"
2026-09-10 校正,本文档早期把它们列在 session 下是误记)。

## 对外接口(以 DESIGN §6 为准)
- `Store` 接口(公共契约):create/get/update/delete/list sessions +
  append/list entries(按 seq range)+ `AcquireLock`(会话执行锁)
- `SessionMeta`(含 UserID/Provider/Model/Status/`Agent` 不透明类型 + 阶段 5
  的 TokensIn/TokensOut/Cost)、`SessionQuery`(ListSessions: UserID/Agent/Limit)、
  `Entry`/`EntryType`(含 EntryCompaction)、状态常量
  (idle/running/interrupted)、哨兵错误
- `NewMemoryStore()` — 进程内参考实现(测试 / example / 无持久化档)
- 工作上下文组装 `buildContext` / token 估算 `EstimateTokens` / 两级压缩
  (L1 `applyL1` + L2 `maybeCompact` + `SummarizeWithStream` + `ContextStats`)
  **均在 core 包**(core/compaction.go、core/token.go),不经 session

## 依赖关系
- 依赖:仅标准库
- 被依赖:core(取上下文/落盘)、adapter/gorm(实现 Store)、gateway(context-stats
  经 runner.ContextStats 间接用)、example

## 已知约束与坑
- **存什么 ≠ 喂什么**:全量 append-only 永不改写;压缩只追加 compaction 条目(ADR-006)
- 压缩绝不在工具执行中途触发(toolCall/toolResult 配对完整性)
- ui_event 与 messages 共享 seq 空间;ui_event 按天数清理,消息与 compaction 不清
- 断线恢复:RunLoop 不依赖连接;停机时运行中 session 落 `interrupted`,重连从最后完整消息续跑
- 存储实现差异:MySQL 为基准,SQLite 需验证 DECIMAL/TIMESTAMP(ADR-005)
- **模块归属校正**:token 估算/两级压缩实际实现于 **core**(每轮边界就地读派生
  上下文并决策压缩,放 core 最内聚),不在 session;session 只有 Store 契约 +
  MemoryStore。早期文档误列在 session 下,已按代码修正
- 参照源:pi `packages/agent/src/harness/compaction/compaction.ts`(865 行)+ `harness/session/`;
  注意 pi 的 CompactionEntry 内嵌保留消息,本库用 first_kept_seq 指针,buildContext 不能直接对照翻译

## 实现细节记录
### 2026-09-16 SessionMeta.Agent + ListSessions 改 SessionQuery
- 通用会话类型字段 `Agent`(不透明字符串,创建后不可改)。`ListSessions` 签名
  改为 `ListSessions(ctx, SessionQuery)`:按 UserID / Agent 过滤,Limit 0 =
  不封顶。HTTP 层另有默认 50 / 硬顶 200。MemoryStore 整份覆盖 UpdateSession,
  调用方必须 Get 再改 Status,否则会抹掉 Agent
- 涉及文件:session/store.go、session/memory.go、session/memory_test.go

### 2026-09-10 阶段 5:文档校正(压缩/token 归属 core,非 session)
- 核对代码发现:token 估算(`core/token.go`)与两级压缩(`core/compaction.go`)
  都实现在 **core** 包,session 包仍只有 store.go + memory.go(Store 契约 +
  内存实现)。本文档"职责/对外接口"早期把 buildContext/token 估算/两级压缩
  列在 session 下,是设计稿阶段的误记——以代码为准,已全部移到 core 描述。
  session 的 SessionMeta 在阶段 5 加了 TokensIn/TokensOut/Cost 字段(供 core
  的 persistRunUsage 累加、gateway /usage 读取)
- 涉及文件:仅本文档(无代码改动);core 侧改动见 core.md 阶段 5 条目
### 2026-09-09 阶段 0 实现
- Store 契约 9 个方法:create/get/update/delete/list sessions +
  append/list entries + AcquireLock;seq 由 store 分配、每会话单调
- MemoryStore:单 mutex,AcquireLock 用 map 标记(release 闭包删标记)
- **已知坑**:seq 空间由消息与 ui_event 共享(设计如此),所以 ui_event 的
  seq 不连续;SSE 重放按 seq 过滤后只发 ui_event,前端看到的 id 会跳号——
  这是预期行为,不是 bug(2026-09-09 冒烟时验证过)

### 2026-09-09 设计定稿(未实现)
- 阶段 4 做持久化(Store + GORM adapter + 落库 + 重放),阶段 5 做压缩全链路

## 当前状态 / TODO
Store 契约 + MemoryStore 稳定;GORM adapter 在 adapter/gorm(阶段 4 完成)。
压缩/token 估算在 core(阶段 5 完成)。session 包本身已无待办——DESIGN §11
阶段 1-5 全部达成。
