# pi-go 0.3.0 事件契约重构待办

> 状态：计划稿。0.2.x 是扁平事件兼容线，冻结功能；0.3.0 是新的 breaking line。
> 目标：仿照官方 pi 的 agent/AI 事件模型与前端消费方式，而不是只给现有事件补 start/end。

## 1. 目标与非目标

**目标**

- `core.AgentEvent` 的流式部分对齐官方 pi：`message_update` 包 `assistantMessageEvent`。
- `assistantMessageEvent` 提供块级事件：`start`、`thinking_start/delta/end`、
  `text_start/delta/end`、`toolcall_start/delta/end`、`done/error`，带
  `contentIndex` 与 partial assistant message。
- provider 层（OpenAI chat、OpenAI Responses、Anthropic、Faux）统一映射到这套事件。
- gateway 的 SSE / `ui_event` 落新形状，同时给 0.2.x 旧事件留回放适配。
- 前端可以按官方 pi 的方式消费事件；业务输出管道（医院标签/echart/source）继续可用。

**非目标**

- 不搬官方 pi 的 TUI、扩展系统、remote protocol、CLI。
- 不把 `reasoning.summary` 作为公共契约；vLLM 只有原始 reasoning_text，也不需要摘要。
- 不在 0.3.0 重写业务模块；0.2.x 项目继续留在 0.2.x。
- 不追求 provider 侧 1:1 的 `partial AssistantMessage` 全量快照（见 §6 决策）。

## 2. 事件模型

### 2.1 目标形状（对齐官方 pi）

```text
AgentEvent
  agent_start / agent_end
  turn_start / turn_end
  message_start / message_update / message_end
  tool_execution_start / update / end

message_update:
  message: AgentMessage
  assistantMessageEvent:
    start
    text_start | text_delta | text_end
    thinking_start | thinking_delta | thinking_end
    toolcall_start | toolcall_delta | toolcall_end
    done | error
```

每个 block 事件带 `contentIndex`；delta 带 `delta`；end 带完整 `content`；
`partial AssistantMessage` 是否每个 delta 都带，见 §6。

### 2.2 待办

- [ ] `core/event.go` 增加 `message_update` + `AssistantMessageEvent` 类型与构造器。
- [ ] 保留现有生命周期/工具事件，确认 JSON 字段与官方 pi 语义一致。
- [ ] `core/loop.go` 把 provider 流翻译成新事件，不再只发平铺 delta。
- [ ] `core/message.go` 的 `Block` 与 official pi 的 `ThinkingContent` /
      `TextContent` / `ToolCall` 对齐字段（`contentIndex` 可放事件，不必落到消息）。
- [ ] 兼容策略：0.2.x 的 `text_delta` / `thinking_delta` / `toolcall_delta`
      在 0.3.0 中是否保留为 deprecated 别名，需在实现前定（建议保留一个 minor 周期）。

## 3. Provider 适配

- [ ] OpenAI chat：文本块开始/结束、thinking（reasoning/reasoning_content/<think>）、
      toolcall 累积都发 start/delta/end。
- [ ] OpenAI Responses：`output_item.added/done` → block start/end；
      `reasoning_text.delta` / `thinking_delta` → thinking；`output_text.delta` → text；
      `function_call_arguments.delta` → toolcall。summary 可选，不作为契约。
- [ ] Anthropic：`content_block_start/stop` → block start/end；thinking signature
      继续放 `Block.Signature`。
- [ ] Faux：脚本化 provider 同步产出块级事件，供 CI/集成方测试。
- [ ] 跨 provider 历史的 signature/itemId 处理保持现有语义（Responses 的
      reasoning item / function_call item；Anthropic signature 不互通）。

## 4. Output 后处理（医院标签协议）

现有医院侧依赖 `gateway.WithEventMiddleware` + `output.Transform`：
`<|text|>` / `<|table|>` / `<|echart|>` / `<|source|>` → Processor → Stage
（JSONRepair、EchartValidate、SourceValidate）。0.3.0 不能把这条链路弄丢。

- [ ] 抽出事件模型无关的文本流入口（例如 `TextDeltaSink` / `OutputPipeline`）：
      无论事件是扁平还是 `message_update`，都先解出 `text_delta` 再喂 Processor。
- [ ] `output.Transform` 适配新事件：消费 text delta，产出业务 `output` 事件；
      `OutputKind/Text/Data` 契约保持不变。
- [ ] 一条 assistant 消息多个 text block 时，按顺序喂同一个 Processor，
      `Finish` 在该消息结束时调一次（禁止每个 block 新建）。
- [ ] 继续遵守“存原始、喂处理后的”：`ui_event`/`message` 里的 assistant 原文
      不被加工结果覆盖；`output` 事件只用于展示。
- [ ] `source` 白名单继续 tap `tool_execution_end`，事件换形状不改业务逻辑。
- [ ] 前端继续消费业务 `output` 事件；新前端按官方 pi 消费时，需把
      `output` 当作 pi-go 扩展事件处理，不能假设只有官方事件。

## 5. Gateway / 存储 / 回放

- [ ] SSE 线格式升级；事件 JSON 形状变化。
- [ ] `ui_event` 落新形状；旧 0.2.x 行在回放时做适配（读时转换，或按版本解析）。
- [ ] `/turns` 的 activity/result 分类适配新事件：
      thinking/工具 → activity；text/output → result。
- [ ] `/messages` 的 assistant message 内容块不变（thinking/text/toolCall），
      仅流事件形状变化。
- [ ] 可选：`WithEventFormat("flat"|"pi")` 或路径/头版本，给老前端一个过渡期。
- [ ] 0.2.x 的断线重放、`Last-Event-ID`、`Shutdown`/`resume` 回归测试保持通过。

## 6. 关键决策（实现前拍板）

1. **partial 快照粒度**：官方 pi 每个 delta 都带完整 partial AssistantMessage。
   Web SSE 下这会放大包体。建议：`start`/`end` 带 partial，delta 只带
   `contentIndex + delta`；若坚持 1:1，再全量带。
2. **0.2.x 兼容**：0.3.0 是否保留扁平 delta 别名字段一个 minor 周期。
   建议保留，降低旧前端迁移成本。
3. **业务 output 事件**：继续作为 pi-go 扩展事件，不塞进官方 pi 的 core。
4. **summary**：不进入公共契约；OpenAI 官方模型如不请求 summary，就没有可见
   思考文本；vLLM 的 reasoning_text 正常映射。
5. **0.3.0 版本策略**：0.2.x 只修 bug；0.3.0 是新契约；旧项目不强制升级。

## 7. 验收标准

- [ ] 四个 provider 的块级事件 golden test（顺序、contentIndex、end content）。
- [ ] 医院标签协议的回归：text/table/echart/source、JSON repair、echart 校验、
      source 白名单全部通过。
- [ ] 0.2.x 旧 `ui_event` 会话可以回放，不报错、不丢正文。
- [ ] 前端按官方 pi 的 `message_update` 消费可以渲染：思考中/已思考、正文、
      工具调用、错误。
- [ ] `go test -race ./...` 全绿；example 编译；README 记录事件版本。
