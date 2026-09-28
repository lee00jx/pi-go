# pi-go 项目索引

## 一句话简介
自研通用 Go Agent 库(独立 module,供本人后端项目集成):ReAct 循环 + 事件流 + 两级压缩,
架构参照 pi(earendil-works)。设计细节以 `../DESIGN.md` 为准,本目录只记项目状态与踩坑。

## 模块清单
| 模块 | 文档 | 职责 |
|---|---|---|
| core | modules/core.md | ReAct 主循环、AgentEvent、Tool 接口与钩子、salvage、token 估算 + 两级压缩 + 资源治理 |
| provider | modules/provider.md | Anthropic + OpenAI 兼容客户端、统一流式事件、重试/降级/切模型 |
| session | modules/session.md | Store 契约 + MemoryStore(压缩/token 在 core,非此处) |
| tools | modules/tools.md | 注册表 + 内置 bash + fs(read/write/edit,零新依赖 1:1 贴 pi);sql 不做;recall_event 在 core |
| output | modules/output.md | 可选模型输出 Processor + 有序校验/清洗 Stage + AgentEvent 转换管道 |
| gateway | modules/gateway.md | 纯标准库 SSE/REST 端点、断线重放、心跳、会话归属(不解析 JWT) |
| adapter | modules/adapter.md | 官方 GORM Store 实现(覆盖 MySQL/PG/SQLite)+ 迁移 SQL |
| example | modules/example.md | 最小 Gin 集成示例(活文档,集成验收标准载体) |

## 依赖关系
- core ← provider(core 只依赖 provider 的统一流式事件 StreamFn)
- core ← tools(工具经 registry 实现 core.Tool 接口)
- output → core(包装 AgentEvent channel;不反向影响循环/消息落库)
- session ← core(循环经 Store 读上下文、落盘)
- gateway → core/session,可经 EventMiddleware 接 output.Transform;
  adapter → session(实现 Store)
- 方向纪律:gateway/adapter/example 依赖 core/provider/session/tools/output,反向禁止;
  核心五包不 import gin/gorm/zap/viper

## 当前开发阶段
**全部阶段完成——DESIGN §11 阶段 1-5 达成,库功能闭环,可接入后端项目。**
`agent` 会话类型、`WithAgentRunner`、`GET /sessions`、`GET /turns`、
`InjectTurn`(合成一轮)已落地。
阶段 5:压缩 + 治理。2026-09-15 质量验证两缺陷已修(buildContext fail-closed /
幂等失败回滚)。接入方式见根 README.md(本地 replace + 自定义 Tool +
SystemPrompt + gin.WrapH)。module 路径仍是占位 `github.com/lee/pi-go`
(ADR-008);发布到 GitHub 后去掉 example 的 replace。
刻意不做:sql 工具、fs 路径沙箱、Casbin 进库、FromViper(填 Runner 字段即可)。
