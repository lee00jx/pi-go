# 整体架构

## 一句话
pi-go 是一个**库,不是服务**:后端项目 `go get` 后,把纯标准库 `http.Handler` 网关挂进 Gin,
agent 以 ReAct 循环调 LLM,行为全量事件化,状态经 Store 接口落 append-only 表。

## 分层
```
集成方(库外)   Gin 路由 / Viper / zap / gin-jwt + Casbin / swaggo / golang-migrate
库(独立 module) core / provider / tools / output / session / gateway / adapter(gorm) / example
```
核心五包(core/provider/tools/output/session)零框架依赖,只接触标准库;
gateway 是唯一接触 HTTP 的包(纯 http.Handler,不绑 Gin);
adapter/gorm 是唯一允许依赖 gorm 的库内包(可选引入)。

## 数据流
1. 集成方 JWT+Casbin 通过后经 `ContextWithUserID` 注入身份 → `POST /sessions/{id}/prompts`
   → gateway 校验会话归属、取执行锁(运行中 → steering 队列,新 prompt_id)→ core.RunLoop
2. RunLoop 每轮:`PrepareNextTurn`(压缩/切模型/改 thinking)→ 流式调 LLM(provider SSE)
   → 工具执行(钩子:权限/确认)→ 结果按原序回填 → `ShouldStopAfterTurn` 检查
3. 可选 `output.Transform`:仅 text_delta 经项目 Processor → 有序 Stage 链,
   产出协议无关 output 事件;不启用则原 channel 直通
4. 事件双路:落库(ui_event,与消息共享 seq)+ SSE 多路推给已连前端
5. 前端重连:`Last-Event-ID` / `?after=seq` → 先重放缺失 ui_event 再续流;
   RunLoop 不依赖连接,断线照跑照落盘

## 存储模型
`sessions` + `session_entries`(append-only,每会话 seq 单调递增)。
原则:**存什么 ≠ 喂什么**——全量落盘永不改写,每轮从全量推导工作上下文
`[system] + [最新 compaction 摘要] + [seq > first_kept_seq 的消息(经 L1 驱逐 + convertToLlm)]`。
压缩只追加 `type=compaction` 条目,绝不改历史。表结构见 DESIGN §6.2。

## 部署形态
随宿主后端进程运行。优雅停机(阶段 4 已实现):集成方收到 SIGTERM 时调
`Gateway.Shutdown()`(取消全部 in-flight run、等排空),运行中 session 落
`interrupted`;重启后 `POST .../resume` 从最后完整消息处续跑(agentLoopContinue
语义,续跑前修复悬空 tool call)。状态经 GORM Store 落 SQLite/MySQL/PG 文件,
重启不丢(SQLite 档需 `SetMaxOpenConns(1)`,见 adapter.md / ADR-013)。

## 技术栈
- 库内依赖仅 4 个:go-playground/validator、creack/pty、go-difflib、go-gitignore
  (+ adapter/gorm 的 gorm;SQLite 档再带 gorm.io/driver/sqlite=mattn/go-sqlite3)
- 自研:Anthropic SSE 客户端、partial-json 抢救解析器、OpenAI 兼容客户端、SSE 多路器
- 集成方:Gin / GORM / Viper / zap / Casbin / gin-jwt / swaggo / golang-migrate / Air
- 参照源:pi(earendil-works)`/Users/lee/Documents/workspace/26.9/pi`
  (只取 packages/agent + packages/ai 语义,不抄 TUI/CLI/扩展/远程协议)
