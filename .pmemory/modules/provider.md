# provider

## 职责
LLM 客户端层:Anthropic 手写客户端(官方无 Go SDK)+ OpenAI 兼容客户端
(盖 OpenAI/DeepSeek/通义/vLLM),归一为统一流式事件;重试/退避/fallback;支撑会话中途切模型。
**不做**:工具、循环逻辑(core)、上下文组装(session)。

## 对外接口(计划,以 DESIGN §3 为准)
- `StreamFn` — 统一流式事件:`start → (text_delta | thinking_delta | toolcall_delta)* → done | error`;
  done 带 usage(含 provider 原生 totalTokens)
- OpenAI 兼容:baseURL + apiKey + model 均可配
- OpenAI 构造 options:`WithExtraBody(map[string]any)` 传 vLLM 等私有顶层字段;
  `WithHTTPClient(*http.Client)` 注入 timeout/代理/私有 CA;
  `WithInsecureSkipTLSVerify()` 仅显式关闭当前 client 的证书校验
- fallback 模型:主模型重试耗尽后切换,发 `model_changed` 事件
- **Sampling 上线**(ADR-022):`core.Sampling` 零值 = OpenAI 兼容请求体省略
  temperature / top_p / max_tokens / seed;填了才上线(temperature=0 走指针)。
  Anthropic 仍必填 max_tokens,客户端在 0 时补 8192,seed 永不出现在 Anthropic
  请求体。presence_penalty / frequency_penalty / top_k / stop 不进公共结构
- **faux provider 导出**:脚本化事件流的假实现,CI 与集成方测试都用它(公共契约)

## 依赖关系
- 依赖:仅标准库(net/http + SSE 解析);两家格式各自解析后归一
- 被依赖:core(只认 StreamFn)、session(L2 摘要压缩走独立 LLM 请求)

## 关键文件路径
- `provider/faux.go` — Faux 脚本化 provider(公共契约;FauxTurn 支持 StopReason 覆盖)
- `provider/openai.go` — OpenAI 兼容客户端(盖 OpenAI/DeepSeek/通义/vLLM)
- `provider/retry.go` — WithRetry 重试/退避/fallback 包装
- `provider/anthropic.go` — Anthropic Messages API 手写客户端(阶段 2)
- `provider/openai_test.go`、`provider/retry_test.go`、`provider/anthropic_test.go` — httptest SSE 服务端单测
- 统一流式类型在 `core/stream.go`(ADR-009,不在本包)

## 已知约束与坑
- Anthropic SSE 事件格式、tool call 结构、thinking 块、prompt caching 字段均为其特有,手写解析
- **Anthropic 特有**:`max_tokens` 必填(客户端默认 8192,可被 Sampling.MaxTokens 覆盖);
  响应无原生 total,usage.total = input+output+cacheRead+cacheWrite;
  SSE 每帧带 `event:` 行(OpenAI 只有 `data:`),解析必须认
  `content_block_start` 里的 tool_use 初始 `input:{}` 是空对象,**不能**写进参数累积器(会和
  partial_json delta 混出 `{}{"text":...}` 非法 JSON)
- `strings.Builder` 累积器(tcAccum/anAccum.args):含 Builder 的结构体**不能**在
  Builder 非空后按值拷贝(append 进 slice 前先写完所有非 Builder 字段,append 后走
  slice 下标写入)
- 工具用途走可选 `core.DescribedTool`：有则写入 OpenAI `function.description` /
  Anthropic `tools[].description`（omitempty）；无则只传 name + schema
- 重试:429/5xx/网络错误指数退避(带 jitter,最多 3 次);400 类参数错误**不重试**直接报错
- 流式中途断开:**未发出任何 delta** 才可整轮重试;已发出 delta 则不重试
  (消费者已见内容,重试会重复吐字),partial 经 StreamError.Message 保留为历史
- **跨 provider 切模型**:thinking 块切走时丢弃(各家 signature 不互通);
  工具调用/结果历史两家格式不同,由 convertToLlm 转写;上下文预算按新模型 contextWindow 重算
- ExtraBody 在 NewOpenAI 时 JSON snapshot,调用方之后改原 map 不会影响请求;
  model/stream/messages/tools/sampling 等 typed 字段禁止覆盖。多次 WithExtraBody 合并
- OpenAI 兼容思考:vLLM/Qwen 经 ExtraBody `chat_template_kwargs.enable_thinking`;
  流里 `reasoning_content`/`reasoning` 以及正文中的 `<think>…</think>` 归一成
  `thinking_delta`(落盘 thinking block)。回传 OpenAI 历史仍丢弃 thinking 块
- Insecure TLS 会 clone `http.Client` 和 `*http.Transport`,绝不能改
  `http.DefaultTransport`;自定义非 `*http.Transport` RoundTripper 无法安全 clone,
  组合该 option 时构造直接报错。生产优先注入私有 CA,不要关闭校验
- 参照源:pi `packages/ai/src/api/{anthropic-messages,openai-responses}.ts` + `utils/json-parse.ts`
  (pi 用 npm partial-json 库 + repairJson 修复层;Go 无对等库,解析器自研,
  repairJson 的控制字符转义/无效转义修复逻辑要一并翻译)

## 实现细节记录
### 2026-09-20 下发工具 Description
- `buildToolDefs` / `buildAnTools` 调 `core.ToolDescription`；oaToolSchema 加 description omitempty
- 单测：实现 DescribedTool 的 echo 带描述，未实现的 echo 字段省略
- 涉及文件：`provider/openai.go`、`provider/anthropic.go`、对应 `_test.go`

### 2026-09-16 OpenAI 兼容流拆出 thinking_delta
- 认 vLLM/Qwen `delta.reasoning_content`(及 `reasoning`)；正文里的 `<think>` 标签跨 chunk 切开,不泄漏进 text
- 最终 assistant 消息 thinking 块在 text/tool_call 之前;buildMessages 仍丢弃 thinking(无 OpenAI 等价物)
- 涉及文件:`provider/openai.go`、`provider/openai_think.go`、`provider/openai_think_test.go`

### 2026-09-16 OpenAI 私有请求字段与单客户端 TLS 配置
- `NewOpenAI` 改为兼容旧调用的 variadic options;新增 WithHTTPClient /
  WithExtraBody / WithInsecureSkipTLSVerify
- ExtraBody 支持 `chat_template_kwargs.enable_thinking=false`,但拒绝覆盖
  框架拥有的 typed wire 字段;构造时深 snapshot,多 option 合并
- insecure TLS 显式 opt-in,clone client/transport/TLSConfig,不污染全局;
  自签名 httptest 验证该 client 成功且普通 client 仍失败
- 涉及文件:provider/openai.go、provider/openai_test.go

### 2026-09-15 Sampling 映射到 OpenAI / Anthropic 请求体
- OpenAI 兼容:temperature/top_p/max_tokens/seed 走 `omitempty`;零值 Sampling
  四键都不出现(vLLM 用服务端默认)。temperature=0 靠指针上线
- Anthropic:max_tokens 仍必填,0 → 8192;temperature/top_p 有值才上线;seed
  永不出现(API 无此字段)
- 单测:`TestOpenAISamplingOmitted`/`Set`、`TestAnthropicSamplingOmitted`/`Set`
- 涉及文件:provider/openai.go、provider/anthropic.go、对应 _test.go

### 2026-09-10 阶段 2:Anthropic Messages API 客户端
- `provider/anthropic.go` 新文件,与 openai.go 同构(转写逐事件对照 pi
  `anthropic-messages.ts`):
  - 请求:`POST {base}/v1/messages`,头 `x-api-key` + `anthropic-version: 2023-06-01`;
    `max_tokens` 必填(req.Sampling.MaxTokens,缺省 8192);system 单独走 `[{type:text,text}]`
    参数不进 messages;ThinkingLevel 非空 → `thinking:{type:enabled,budget_tokens}`
    (low/minimal 1024 / medium 4096 / high 16384)
  - SSE 解析:认 `event:`+`data:` 行对;message_start 记 input usage(指针区分 null,
    避免 message_delta 的 null 覆盖);content_block_start 按 index 建累积器
    (text/thinking/redacted_thinking/tool_use);delta 四型:text_delta/thinking_delta/
    input_json_delta(原始片段透传 toolcall_delta 事件)/signature_delta(不发事件,
    仅存);message_delta 映射 stop_reason 并合并 output usage;error 事件分类
    (overloaded/rate_limit/api/timeout 可重试)
  - stop_reason 1:1 映射 pi:end_turn/pause_turn/stop_sequence→end_turn、
    max_tokens→length、tool_use→toolUse、refusal→error(取 stop_details.explanation)、
    sensitive→error、未知值→error(不猜)
  - 流尾无 stop_reason → 可重试 error;refusal/sensitive → StreamError 带 partial
    Message(core 保留为 StopError 历史)
  - usage.total = input+output+cacheRead+cacheWrite(API 无原生 total)
  - 历史转写 buildAnMessages(跨 provider 兼容的 Anthropic 半):
    连续 tool 结果**合并进一条 user 消息**的 tool_result 块(tool_use_id/content/
    is_error;API 要求角色交替);thinking 块**带 signature 才回传**,无 signature
    降级为 text 块(1:1 pi allowEmptySignature=false);tool call → tool_use 块
    (input 空则 `{}`)
  - 工具参数定稿同 OpenAI 走 core.ParseSalvage
- 踩坑:`content_block_start` 的 tool_use 总带 `input:{}`,首版把它写进累积器导致
  参数混成 `{}{"text":...}` 抢救失败 → 跳过空对象;anAccum 在 append 前被值拷贝
  触发 strings.Builder copy panic → append 提前、写 input 走 slice 下标
- 单测 anthropic_test.go 7 例:文本流(usage 两段合并 + 头 + wire 体断言)/
  工具调用流(双块顺序+参数解析)/截断工具调用(salvage+StopLength)/
  thinking+signature/400 分类/状态码矩阵(含 529)/混合历史 wire
  (tool_result 合并、无 sig thinking 降级、tool_use 形状)
- 阶段 2 验收见 gateway.md 的 TestMidRunModelSwitchE2E(Claude→DeepSeek 经 HTTP 切)

### 2026-09-09 阶段 1 实现
- OpenAI 兼容客户端(chat completions):
  - 请求体 `stream: true` + `stream_options.include_usage`(usage 单独在最后一条
    chunk 里,choices 为空);历史转写:assistant 带 tool_calls 时 text 保留在
    content、thinking 块丢弃;tool 结果 → role=tool + tool_call_id
  - SSE 解析:bufio.Scanner(初 64KB/上限 4MB),只认 `data:` 行,`[DONE]` 收尾;
    `delta.tool_calls[index]` 按 index 累积 id/name/arguments 片段;
    finish_reason 映射 length→StopLength、tool_calls→StopToolUse、
    content_filter→StopError;chunk 级 `error` 字段 → 不可重试
  - 中途断流(scanner.Err 或连接断)→ 可重试 error,partial 已通过 delta 发出
  - 工具参数定稿走 `core.ParseSalvage`(finish_reason=length 时参数是残 JSON)
  - `APIError{Provider,Status,Message,Retryable}`:429/408/≥500/传输错误可重试,
    4xx 与 ctx 取消不可重试;`IsRetryable` 导出供复用
- WithRetry(永不返回非 nil error,失败一律收敛为单个 StreamError 事件):
  - 指数退避 + 全 jitter(`base << attempt`,封顶 MaxDelay;默认 3 次/500ms/8s)
  - **已发出任何 delta 后不再重试**(消费者已见内容),error 事件带 partial Message
  - 重试抑制重复 start:第一次之后的 StreamStart 吞掉
  - fallback:主模型可重试失败耗尽且 Fallback 已设 → 切 FallbackModel 试一次
    (自身不再重试),done 事件打 Model 标签 → core 发 model_changed;
    OnFallback 回调供集成方记日志/改会话状态
  - channel 无故关闭(无 done 无 error)= 可重试的流中断(errStreamEndedUnexpectedly)
- FauxTurn 加 StopReason 字段,截断保护单测靠它脚本化 length 场景
- 单测:openai 6 例(文本流/工具调用流/截断参数抢救/400 分类/状态码重试矩阵/
  混合历史 wire 转写)+ retry 6 例(重试后成功/400 快速失败/出内容不重试/
  fallback 切换+标签/400 不 fallback/ctx 取消即停)

### 2026-09-09 阶段 0 实现
- Faux:每次 Stream 调用弹下一轮脚本,耗尽后停在最后一轮(保证循环可自然收敛);
  toolCall id 在发事件前统一生成,保证 message blocks 与 toolcall_delta 一致
- 文本按 12 字符切片发 text_delta,模拟真实流式(不加 sleep,测试不等待)

### 2026-09-09 设计定稿(未实现)
- 阶段 1 先做 OpenAI 兼容;阶段 2 做 Anthropic + convertToLlm + 中途切模型

## 当前状态 / TODO
阶段 2 完成:Faux + OpenAI 兼容 + Anthropic + WithRetry 全部可用,单测全绿;
跨 provider 历史转写两家齐备(convertToLlm 两端),example/gin-integration
双 provider 注册(faux/openai + anthropic 按 ANTHROPIC_API_KEY 启用)。
遗留:prompt caching 字段(Anthropic cache_control)未透传,阶段 3+。
工具 description 已由 DescribedTool 下发(ADR-030)。
