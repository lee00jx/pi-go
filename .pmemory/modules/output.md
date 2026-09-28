# output

## 职责
可选地把 `core.AgentEvent` 中的模型 `text_delta` 交给项目自定义 Processor,
再依次执行项目自定义 Stage(校验或清洗),产出协议无关 `output` 事件。
负责 channel 生命周期、context 取消、Processor 每 assistant 消息隔离和失败策略。
**不做**:具体标签解析、table/echart、JSON repair、业务校验或模型消息落库。

## 对外接口
- `Processor { Write(ctx, delta) ([]Chunk,error); Finish(ctx) ([]Chunk,error) }`
- `Factory func() Processor` — 每条 assistant 消息新建一个有状态解析器
- `Stage { Process(ctx, Chunk) (Chunk,error) }` / `StageFunc` — 有序后处理;
  校验返回原 Chunk,清洗返回改写 Chunk
- `Transform(ctx, <-chan core.AgentEvent, ...Option) <-chan core.AgentEvent`
- options:`WithProcessor` / `WithStages` / `WithFailurePolicy`
- 失败策略:`EmitError`(安全默认)/`FallbackToRaw`(显式 opt-in)/`DropInvalid`

## 依赖关系
- 依赖:core(AgentEvent/Event 构造器)、标准库
- 被依赖:gateway 可经 `WithEventMiddleware` 接入;集成方也可直接包装 Runner channel

## 关键文件路径
- `output/processor.go` — Chunk/Processor/Factory/Stage/StageFunc
- `output/transform.go` — 配置、失败策略、事件管道
- `output/transform_test.go` — 直通/事件顺序/Stage 顺序/失败策略测试
- `core/event.go` — 通用 `output` / `output_error` 线事件
- `gateway/output_test.go` — 处理后事件落库与 SSE 重放集成验收

## 已知约束与坑
- Processor 是有状态流解析器,**绝不能跨 assistant 消息/会话复用实例**;
  Factory 每条 assistant 消息创建新实例
- Transform 只消费 text_delta;thinking/tool/lifecycle 原样转发,避免思考内容污染格式协议
- 默认 `EmitError` 抑制被拒内容。安全 Stage 拒绝的内容不能默认 fallback 原文,
  否则校验等于失效;`FallbackToRaw` 必须由集成方显式选择
- Output 只改对外事件,core 内部原始 Message、append-only store、下一轮上下文
  均不改。需要保存业务 sections 的项目从 output 事件自行投影
- `Chunk.Data` 非空时必须是合法 JSON(`json.RawMessage`),否则下游事件序列化会失败;
  当前框架不替 Processor 修复或猜测 JSON
- 当前不含 JSON 修复。现有 `core.ParseSalvage` 是截断工具参数安全语义,
  失败兜底 `{}`,不能拿来当展示输出的通用 JSON 修复器

## 实现细节记录
### 2026-09-16 通用输出处理管道
- 新增 Processor/Factory:项目只实现协议解析,框架负责 delta 分发与 Finish
- 新增有序 Stage 链:同一接口同时容纳“只判断”的校验和“返回改写值”的清洗
- 新增 `core.Output` / `core.OutputError` 事件及不透明 kind/text/data 字段
- `Transform` 未配置 Processor 时直接返回输入 channel;配置后每条 assistant
  消息独立实例,text delta 被 output 事件替换,其余事件保持顺序
- gateway 新增 `WithEventMiddleware`,处理后事件照常先落 ui_event 再 SSE,
  断线重放契约不变
- 本次刻意不实现 jsonutil、具体标签 Processor、具体 Validator/Sanitizer
- 涉及文件:`output/*.go`、`core/event.go`、`gateway/gateway.go`、
  `gateway/output_test.go`

## 当前状态 / TODO
通用骨架与 gateway 接入完成。后续真实项目按自己的提示词协议实现 Processor/Stage;
如需移植 Python `json-repair`,应另做通用 jsonutil 设计与测试,不要先占位公共 API。
