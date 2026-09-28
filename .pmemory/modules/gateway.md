# gateway

## 职责
库内唯一接触 HTTP 的包:全部 REST/SSE 端点实现为纯标准库 `http.Handler`;
SSE 多路器(一个 session 多个前端连接)、断线重放(Last-Event-ID / ?after=seq)、心跳保活;
prompt/stop/confirm/切模型 handler;会话执行锁与 steering 队列(内存 channel + 落库恢复)。
**不做**:JWT/Casbin(集成方在 Go 后端完成后再挂 gateway)、存储细节(session.Store)。
库内仍校验会话归属:创建只信 `ContextWithUserID`,操作时 `session.UserID == 当前用户`,管理员经 `WithAuthorizer` 放行。

## 对外接口(计划,以 DESIGN §8 为准)
- `New(Store, RunLoop 依赖组, opts...) *Gateway`;`*Gateway` 本身即 http.Handler,
  集成方 `router.Handle(POST, "/api/agent/sessions/:id/prompts", gw)` 式挂接
- 端点(已实现):`POST .../sessions`(user_id 只来自 context,忽略 body;
  有 `WithAgentRunner` 时 body `agent` 必填且须已注册,否则 400)、
  `GET .../sessions`(`?agent=` `?limit=`,默认 50 硬顶 200;无身份 401)、
  `GET .../turns`(按 prompt_id 现场投影,不物化;user/事件/轮级带 Entry.CreatedAt)、
  `POST .../prompts`(运行中 → 202 queued 入
  steering 队列;响应带 `prompt_id`,开跑时带 `run_id`;会话 agent 未登记 Runner
  → 503)、`GET .../events`(SSE:重放 + 续流 + 15s `: ping`)、
  `GET .../messages`(首屏非流式,seq + createdAt 标注)、`POST .../stop`、
  `POST .../model`(切模型 body `{provider,model}`:404 未知 session /
  400 缺字段或 provider 未注册 / 200 `{provider,model}`)、
  `POST .../confirmations`(答确认 body `{id, decision:"allow"|"deny"}`:
  404 未知 session / 非 pending、400 缺 id 或坏 decision、409 已答、
  200 `{status:"allowed"|"denied"}`,DESIGN §5.3)、
  `POST .../resume`(重启后续跑,DESIGN §6.5:仅对 `interrupted` 的 session 生效,
  404 未知 / 409 非 interrupted / 200 `{"status":"resumed","run_id":"..."}`;无新 prompt,
  从最后完整消息重建上下文续跑)、
  `GET .../usage`(阶段 5:per-session `{tokensIn,tokensOut,cost,provider,model}`,
  404 未知)、`GET .../context-stats`(阶段 5:`{currentTokens,contextWindow,
  usedPercent,compactions}`,404 未知)
- **进程内** `InjectTurn(ctx, sessionID, InjectTurnInput) (promptID, error)` —
  不调 LLM 合成一轮:同 prompt_id 写 EntryUser + EntryAssistant + output
  ui_event。鉴权同 sessionFor;running → `ErrSessionRunning`;不造 run_id/
  agent_end。欢迎文案/resultId 留宿主(OutputData 不透明)。暂无 HTTP 路由
- **request_id 幂等**(阶段 5,DESIGN §7.3):prompt/confirm body 带 `request_id`
  时,重复提交返回 202/200 `{"status":"duplicate"}` 且不重跑/不重复作答
- `Shutdown()` — 优雅停机:置 `shuttingDown` 原子标志 → 取消全部 in-flight run
  ctx → `runWG.Wait()` 等排空(finishRun 把取消结束的 run 落 `interrupted`),
  供集成方 SIGTERM 时调用;此后 startRun/startContinue 拒绝新 run
- `WithRateLimit(maxConcurrent, maxPromptsPerMin int)`(阶段 5,DESIGN §7.5):
  per-user 并发 run 上限 + prompts/分钟滑动窗口;超限 → HTTP 429。
  0/nil = 关闭对应维度(默认 lenient-off)
- `ContextWithUserID(ctx, userID)` / `UserIDFromContext(ctx)`:鉴权中间件注入
  真实租户 id。创建会话写入 `sessions.user_id`;限流与归属校验同源;
  不注入则 fail-closed 401(本地/测试经 `WithAnonymousUser` 显式降级)
- `WithAuthorizer(func(ctx, SessionMeta) error)`:**只能放宽**的升级钩子,
  在 owner 检查未通过后才运行(放行 admin);永远无法替换/削弱 owner 检查
- `WithAnonymousUser(user)`:dev/test 专用降级,context 无 user 时以该
  身份继续;生产必须省略
- `WithAuthMiddleware(func(http.Handler) http.Handler)` 可选包装口,不解析 JWT;
  New 里先收集 wrapper,等 mux 按最终 base 注册完再按 Option 顺序包一层
  (先注册的最内层)。与 WithBasePath 组合时 Option 先后不影响路由生效
- `WithBasePath` 覆盖默认 `/api/agent` 前缀;必须在 mux 注册前生效
- `WithAgentRunner(agent, *core.Runner)` — 按 `SessionMeta.Agent` 分发;
  登记后创建必须带已注册类型(禁止静默落到默认 Runner);`New` 对默认 + map
  内每一个 `wireRunner`(steering 队列 + ConfirmTool),按指针去重
- `WithEventMiddleware(...EventMiddleware)` — Runner channel 落库/SSE 前的可选有序
  转换入口;用于接 `output.Transform`,nil 忽略,返回 nil 时保留上一层 channel

## 依赖关系
- 依赖:core(RunLoop/事件)、session(Store)、仅标准库(零外部依赖);
  不直接 import output,由函数签名接入以保持可选
- 被依赖:example、集成方项目

## 关键文件路径
- `gateway/turns.go` — GET /turns 现场投影
- `gateway/inject.go` — InjectTurn 合成一轮(不调 LLM)
- `gateway/gateway.go` — Gateway(http.Handler)、路由、create/prompt/stop handler、SSE 端点
- `gateway/sse.go` — hub(每 session 多订阅者广播,慢消费者丢帧)
- `gateway/model_switch_test.go` — 阶段 2 验收:双 httptest provider 中途切模型
- `gateway/recover_test.go` — 阶段 4 验收:断线续跑 / 重连追平 / 优雅停机
  interrupted / resume 续跑 / resume 校验(5 例,真实 HTTP)
- `gateway/limits.go` — 阶段 5:limiter(内存限流)/ requestDup(幂等)/
  ContextWithUserID / UserIDFromContext / errRateLimited
- `gateway/auth_test.go` — 归属校验 / 忽略 body user_id / Authorizer / prompt+run id
- `gateway/limits_test.go` + `gateway/usage_test.go` — 阶段 5 单测(限流 429 /
  幂等 / usage / context-stats)
- 后续会把 handlers 拆细(当前全在 gateway.go)

## 已知约束与坑
- SSE 自研(ADR-003):多路器 = 每 session 一个 hub,连接注册/注销 + 广播;
  断线客户端不取消 RunLoop(只退订);心跳防中间层掐连接
- 重放顺序:先查 Store 里 `?after=seq` 之后的 ui_event 补发,再订阅实时流;
  前端按 seq 去重(localStorage 存最后 seq)
- 确认等待:钩子内阻塞 channel(默认 120s 超时按拒绝);确认决策落库,刷新/断线期间确认照样有效
- 停止:POST stop → context 全链路取消;保留已产出 partial;PTY 进程组 kill
- 优雅停机:库暴露 Shutdown() 供集成方 SIGTERM 时调用,运行中 session 落 interrupted
- 限额:单轮时长/单工具时长/token 预算(超 → budget_exhausted)/maxTurns;每用户并发 run 数与每分钟 prompt 数
- **限流是进程内内存计数**(非 Redis/DB):单进程够用,多副本部署需换分布式
  计数(与 ADR-014 会话锁同边界)。nil limiter / 维度 0 = 关闭,lenient-off
- **并发计数在 startLoop 同步自增**(handler 返回前可见)→ 测试确定性好;
  释放严格配对(finishRun + begin 失败各一次),不可漏/多
- **request_id 幂等 best-effort**:per-session 窗口溢出整体清空最老,极端高
  并发下可能漏判;不追求强一致(事件已落可重放流,重复提交代价低)
- **WithBasePath 必须先于 mux 注册**:路径字符串在 HandleFunc 时固化。
  New 先跑全部 Option 再建 mux;Auth 只存 wrapper,注册完再包,避免
  再出现"改了 base 字段、路由仍是默认前缀"

- **默认 fail-closed**:context 无 user 且未配 `WithAnonymousUser` → 401,
  不跳过归属。**身份检查先于查库**:未登录时存在与不存在的 id 同为 401,
  状态码不泄露 id 存在性。body `user_id` 永远忽略。归属不符按 404(与不存在
  同码,防探测);旧测试/示例统一加 `WithAnonymousUser("u"/"local")` 显式降级
- **WithAuthorizer 只放宽**:owner 检查通过即放行,钩子只在 owner 不匹配时
  运行;自定义函数漏写 owner 判断也无法打开别人的会话
- **prompt_id / run_id 不进 Store 列**:写在 Message / AgentEvent JSON 里;
  request_id 仍只做 HTTP 幂等。排队先发新 P、R 在 startLoop 时才绑。
  stampRunIDs 会读 `ev.Message.PromptID`,循环不得在 emit 之后回写同一指针

- **prompt 限流租户 = currentUser(调用者)**;并发 run 上限仍按 session 主人
  (`sessionUserID`,startLoop 无 request context)。删掉第四套 `userIDFor`

- **setStatus 必须 Get 再改**:MemoryStore.UpdateSession 整份覆盖,新造缺
  Agent 的 meta 会把类型抹掉;GORM Updates map 不含 agent,列本身不可被更新
- `/turns` 与 `/messages` 的时间必须来自 Entry.CreatedAt,禁止前端 new Date();
  投影漏带则气泡时间只能空或错用看板 created_at
- **turns 不靠「属于该 prompt 的 agent_end」**:一次 run 只发一条 agent_end 且
  stamp 成最后一个 prompt_id;一轮在下一条不同 prompt_id 的 user 或本 run
  结束时收束。cancelled 只标尚未收束的最后一轮
- **合成轮不靠 /turns 回退 assistant**(ADR-029):结果区只认 ui_event;宿主用
  `InjectTurn` 写齐 output。InjectTurn 不广播 SSE(调用方再 GET /turns)

## 实现细节记录
### 2026-09-17 GET /turns 带上 Entry.CreatedAt
- user.createdAt / 活动与结果事件 createdAt / 轮级 createdAt(首条结果,否则末条活动,否则用户消息)
- GET /messages 同步带 createdAt
- 涉及文件:gateway/turns.go、gateway/gateway.go、gateway/turns_test.go、gateway/inject_test.go、DESIGN.md

### 2026-09-16 InjectTurn 进程内合成一轮
- `InjectTurnInput`:UserText 必填;AssistantText 与 OutputText 至少一个;
  OutputText 空则用 AssistantText,反之亦然(保证 buildContext 与 results 都有)。
  OutputKind 默认 `inject`;OutputData 原样写入。顺序:user → assistant
  (`end_turn`) → `EvOutput` ui_event,同一 prompt_id。running 拒
  (`ErrSessionRunning`)。不写 run_id/agent_end
- 单测:turns 有 output complete、OutputData 透传、二轮两 promptId、running/
  鉴权、仅 OutputText 时 assistant 回退
- 涉及文件:gateway/inject.go、gateway/inject_test.go

### 2026-09-16 agent 分发、GET /sessions、GET /turns
- `SessionMeta.Agent` 创建写入后不可改。`WithAgentRunner` 登记 map;创建缺/
  未知类型 400;已有会话未登记 503;无 map 时 agent 可空走默认 Runner。
  `GET /sessions` 只列当前用户(先身份,UserID 绝不空着传 Store)。
  `GET /turns` 从 ui_event 聚活动/结果,EntryUser 只补原文;steering 第一轮
  因下一条 user 变 complete,不空等自己的 agent_end。resume 从最近 user
  挂回 prompt_id stamp,响应仍无新 prompt_id
- 涉及文件:gateway/gateway.go、gateway/turns.go、gateway/agent_test.go、
  gateway/turns_test.go

### 2026-09-16 prompt 限流改走 currentUser,删除 userIDFor
- **背景**:userIDFor 只看 ContextWithUserID,无 context user 时再 GetSession,
  与 currentUser 重复。限流在 sessionFor 之后,碰不上未登录;有
  WithAnonymousUser 时多一次查库
- **改法**:`allowPrompt(g.currentUser(ctx))`;sessionUserID 保留给 startLoop
  并发计数(无 request ctx,按会话主人)
- 涉及文件:gateway/gateway.go

### 2026-09-16 身份检查提前到查库之前,堵状态码探测
- **背景**:sessionFor 原先先 GetSession 再查身份,未登录时存在的 session
  得 401、不存在的得 404,等于给匿名调用者一个 id 存在性探测器
- **改法**:新增 `currentUser(ctx)`(context user → anonUser → "");sessionFor
  开头先判 `currentUser == ""` → errNoUser 401,**不查库**;有身份后未知与
  归属不符统一 404。handleCreate 同步改用 currentUser
- 回归:auth_test 新增 TestNoIdentityDoesNotLeakExistence(存在/不存在 id
  未登录均 401);race 全绿
- 涉及文件:gateway/gateway.go、gateway/auth_test.go、DECISIONS.md
  (ADR-025 表述修正 + ADR-026 补充)

### 2026-09-16 归属校验改 fail-closed,WithAuthorizer 改为只放宽
- **背景**:原默认 fail-open——无 user 时归属直接跳过、创建回退 "local";
  生产忘注入或误挂公开路由则全体会话裸奔。WithAuthorizer 原语义是整体替换,
  自定义函数漏写 owner 判断会把所有会话对某角色敞开
- **改法**:`checkAccess` 要求身份(context user 或 `WithAnonymousUser`),
  否则 errNoUser → 401;owner 检查恒先运行,authorizer 只在 owner 不匹配后
  被调用(只能放宽)。`sessionFor` 改返回 error,`writeSessionErr` 区分
  401(无身份)/404(未知或拒绝)。`handleCreate` 删除 "local" 回退
- 适配:全部测试夹具与 hello example 加 `WithAnonymousUser`;auth_test 重写
  (fail-closed 401 / anon 降级忽略 body user_id / authorizer 只放宽);
  `waitMessagesAs` 替代无用户轮询
- 涉及文件:gateway/gateway.go、gateway/auth_test.go、各 *_test.go 夹具、
  example/hello/main.go、DESIGN.md、.pmemory/*

### 2026-09-16 可信身份 + prompt/run 关联标识
- 创建会话只信 `ContextWithUserID`,忽略 body `user_id`;所有 `{id}` 端点经
  `sessionFor` 做归属校验;`WithAuthorizer` 供管理员放行
- `Message.promptId` 在 `POST /prompts` 接受时生成(含 queued);
  `AgentEvent.runId` 在 `startLoop` 生成,EventMiddleware 之后 stamp;
  同轮后续事件回填 `turn`;queued 响应不带 `run_id`;resume 新 R 无新 P
- 涉及文件:gateway/gateway.go、gateway/limits.go、gateway/auth_test.go、
  core/message.go、core/event.go、example/gin-integration/main.go、DESIGN.md、README.md

### 2026-09-16 WithBasePath 在 mux 注册前生效
- **背景**:New 先按默认 `/api/agent` 注册 ServeMux,再跑 Option;`WithBasePath`
  只改 `g.base` 字段,路由仍钉在旧前缀上。`WithAuthMiddleware` 当时直接
  `g.handler = mw(g.handler)`,必须在 mux 赋给 handler 之后才能跑
- **改法**:Option 一律先应用到 Gateway 字段;WithAuthMiddleware 改为 append
  wrapper;mux 用最终 `g.base` 注册后再按 Option 顺序包 auth。构造 Option
  与 handler wrapping 拆开,顺序任意组合都有效
- 单测:`TestWithBasePathRegistersRoutes`、`TestWithBasePathAndAuthOptionOrder`
  (故意把 Auth 写在 BasePath 前面)
- 涉及文件:gateway/gateway.go、gateway/gateway_test.go

### 2026-09-16 可选 EventMiddleware 接入输出处理
- startLoop 从 Runner 取得 channel 后、持久化/广播前依注册顺序包装 middleware;
  因此 output/output_error 与普通 AgentEvent 一样 append ui_event 并支持 SSE 重放
- gateway 不直接依赖 output 包;集成方闭包内调用 output.Transform。middleware
  返回 nil 时 fail-open 保留上一层 channel,避免整条 run 永久阻塞
- 集成测试用 uppercase Processor 验证 output 被持久化/重放且原 text_delta 被替换
- 涉及文件:gateway/gateway.go、gateway/output_test.go

### 2026-09-15 幂等失败路径回滚:429/500/503 后重试不再被吞(#7 修复)
- **背景**:质量验证发现 `requestDup` 打标在限流检查与 startRun **之前**,
  429 / 500 / 503 / confirm 404 后客户端拿同一 request_id 重试会命中
  duplicate,请求实际从未被受理——"标记先于提交"的幂等漏洞
- **改法**:`limits.go` 新增 `requestRollback(sessionID, key)`(dedupMu 下
  delete key);handlePrompt / handleConfirm 打标后注册
  `defer { if !accepted { rollback } }`,`accepted` 只在成功路径
  (started / queued / duplicate / confirm 送达)置 true;所有失败路径
  自动回滚,重试可再入
- **回归测试**:`gateway/idempotency_test.go` 2 例——限流 1/min 下 A 收下
  (r1 打标)→ B 429 → B 同 id 重试仍是 429(回滚生效)→ A 同 id 重试
  duplicate(成功标保留);confirm 404 后同 id 重试仍 404
- 涉及文件:gateway/gateway.go(handlePrompt/handleConfirm)、
  gateway/limits.go(requestRollback)、gateway/idempotency_test.go(新)
### 2026-09-10 阶段 5:usage/context-stats 端点 + request_id 幂等 + 内存限流(DESIGN §7.3/§7.5/§7.6)
- **`GET .../usage`**(handleUsage):读 session meta →
  `{tokensIn, tokensOut, cost, provider, model}`;未知 session 回 404。
  数据由 core 在每次 run 结束 `persistRunUsage` 累加进 meta,端点只读
- **`GET .../context-stats`**(handleContextStats):未知 session 404,否则
  调 `runner.ContextStats(...)` → `{currentTokens, contextWindow, usedPercent,
  compactions}`(前端"上下文 N% 已用"进度条)
- **request_id 幂等**(DESIGN §7.3,`gateway/limits.go` `requestDup`):
  per-(session, key) 原子"查并打标";key = 客户端 request_id 加命名空间前缀
  (`"p:"` prompt / `"c:"` confirm,两命名空间共享同一 per-session store 但不撞)。
  handlePrompt 在**校验文本后、限流前**查 `"p:"+request_id`,命中 → 202
  `{"status":"duplicate"}` 不重跑(该 request 触发的事件已在可重放 SSE 流里);
  handleConfirm 同样查 `"c:"+request_id` → 200 `{"status":"duplicate"}`。
  request_id 为空则跳过幂等(不强制)。per-session 窗口溢出
  (`maxDedupPerSession=1000`)整体清空最老,**best-effort**(不无限增长)
  **2026-09-15 修正(#7)**:此前限流/startRun 失败(429/500/503)发生在打标
  **之后**,重试同 request_id 被吞成 duplicate。现改为:打标后任一非
  accepted 路径(429/503/500/确认 404/400/409)统一 `requestRollback` 删 key
  (`defer + accepted` 布尔),重试不再被吞
- **多租户限流**(DESIGN §7.5,`gateway/limits.go` `limiter`):`WithRateLimit
  (maxConcurrent, maxPromptsPerMin)` Option。两个维度都 per-user:
  - **prompts/min**:60s 滑动窗口(`allowPrompt` 保留窗口内时间戳,≥ 上限 → 429)。
    在 handlePrompt **同步**检查(限流前不占并发槽)
  - **并发 run**:`allowConcurrent` 在 **startLoop 拿 session 锁后同步**计数
    (handler 返回前已可见,测试确定性好);`finishRun` 与 begin 失败路径各
    `releaseConcurrent` 一次(配对,不多减不少减)
  - user 来源:`userIDFor`(优先 `ContextWithUserID` 注入的真实租户,否则
    session.UserID,再否则 `"default"`)。集成方鉴权中间件调 `ContextWithUserID`
    打标即按真租户限流
  - **nil limiter / 维度 0 = 关闭**(lenient-off 默认,单租户部署不受影响)
- **单测**:`gateway/limits_test.go`(TestPromptRateLimit 3 连发第 3 个 429 /
  TestConcurrentSessionLimit 同用户第 2 个并发 429、首个结束释放后可起 /
  TestPromptIdempotency 重复 request_id 不重跑消息数不变 / TestRequestDupNamespace
  p:/c: 不撞 + 跨 session 独立)、`gateway/usage_test.go`(TestUsageEndpoint 成本
  核算 / TestContextStatsEndpoint 占用百分比 / compaction 计数 / 404)全绿
- 涉及文件:gateway/gateway.go(WithRateLimit/dedup 字段/两新路由/handleUsage/
  handleContextStats/handlePrompt 幂等+限流+errRateLimited→429/startLoop 并发
  计数/finishRun 释放/userIDFor/sessionUserID)、gateway/limits.go(errRateLimited/
  ctxUserIDKey/ContextWithUserID/limiter/requestDup)、gateway/limits_test.go、
  gateway/usage_test.go
### 2026-09-10 阶段 4:优雅停机 Shutdown + interrupted + resume 端点(DESIGN §6.5)
- **`Shutdown()`**:幂等(`shuttingDown.CompareAndSwap(false,true)` 只有首个
  调用者做事)→ 取消 `cancels` 里全部 in-flight run 的 ctx → `runWG.Wait()`
  等排空。目的:让每个 run 的 finishRun 有机会把 running 的 session 落成
  `interrupted` 并刷盘 partial,进程再退出。集成方 SIGTERM 时调它(example 已接)
- **`runWG sync.WaitGroup`**:startRun/startContinue 各 `Add(1)`,run goroutine
  结束 `Done()`。startRun/startContinue 入口 `if g.shuttingDown.Load()` 直接
  拒绝(停机中不再起新 run)
- **`finishRun` 区分停机取消 vs 正常结束**(关键判定):
  `g.shuttingDown.Load() && runCtx.Err() == context.Canceled` → `release()` +
  落 `interrupted` + **不起** follow-up(直接 return);否则走原逻辑
  (drain steering 队列 → 有则 follow-up run,无则 idle)。用 `context.Background()`
  落 status,避免用已被取消的 runCtx 写库
- **`handleResume`(POST /resume)**:`GetSession` 404 校验 → status 非
  `interrupted` 回 409 "session is not interrupted" → `startContinue(id)`
  → 200 `{"status":"resumed"}`。`startContinue` = `startLoop(id, func(ctx){
  return g.runner.RunContinue(ctx, id) })`——复用 startLoop 的锁/cancel/
  running 置位/SSE 广播全套,只是起 run 的闭包换成 RunContinue(无 prompt)
- **落库容错**(startLoop 的 persist 循环):`AppendEntry(ui_event)` 失败时
  `if runCtx.Err() == nil { log.Warn }`(只在**非**取消时才告警)然后
  `continue` **继续 drain** 事件 channel——绝不 break,否则生产者(runner)
  阻塞在 send 上死锁;代价是该帧未落库、不可重放,但停机场景下可接受
- **阶段 4 验收测试** `recover_test.go`(5 例,真实 HTTP + faux + riskyTool):
  - TestRunContinuesWithoutClient:无 SSE 客户端时 run 照常跑完并落库
    (断线不停跑)
  - TestDisconnectReconnect:确认暂停 → 断 SSE#1 → 重连 SSE#2( goroutine)
    → 作答确认 → SSE#2 精确追平(重放的 ui_event + 实时流)
  - TestGracefulShutdown:Shutdown 后 session 落 `interrupted`
  - TestResume:interrupted + 悬空 deploy(tc9)→ POST /resume → 修复合成
    error tool_result + 循环续跑到 idle
  - TestResumeValidation:404 未知 / 409 非 interrupted
- 涉及文件:gateway/gateway.go(Shutdown/runWG/shuttingDown/startContinue/
  finishRun/handleResume/persist 容错 + 路由)、gateway/recover_test.go
### 2026-09-10 阶段 3:POST /confirmations 端点 + Web 确认等待器(DESIGN §5.3)
- `pendingConfirm{sessionID string, decision chan core.Confirmation}`(无缓冲,
  一次一问一答);Gateway 加 `confirms map[string]*pendingConfirm`(按 call.ID
  索引)+ `confirmT time.Duration`(默认 120s);`New` 初始化 map 并在
  `runner.Hooks.ConfirmTool == nil` 时安装 `g.waitConfirm` 作为默认等待器
  (集成方自己设了 ConfirmTool 就不覆盖)
- `waitConfirm(ctx, sessionID, call)`(core.ConfirmTool 的 Web 实现):注册
  `g.confirms[call.ID]` → select 三向:`p.decision`(用户作答)/`timer.C`
  (confirmT 超时 → 拒绝 "confirmation timed out")/`ctx.Done()`(stop →
  拒绝 "run stopped")。defer 清理只在仍是当前条目(`cur == p`)时删除,
  避免误删同 call.ID 的新 pending
- `handleConfirm`(POST /confirmations):解 `{id, decision}` → id 空或
  decision 非 allow/deny 回 400 → 查 `g.confirms[id]` 且 sessionID 匹配,
  否则 404 "confirmation not pending" → 非阻塞 send 到 `p.decision`,
  默认分支(已有人先答)回 409 "confirmation already decided" → 否则 200
  `{status:"allowed"|"denied"}`。非阻塞 send + 先查后发保证并发两个作答
  只有一个成功
- `WithConfirmTimeout(d)` Option 覆盖默认 120s(测试用 300ms)
- **阶段 3 验收测试** `confirm_test.go`(4 例,真实 HTTP 阻塞+唤醒,run 阻塞期间
  走完整 SSE 重放):TestConfirmationsFlow(allow→执行 1 次+response 非 error+
  工具输出落库)、TestConfirmationsDeny(deny→执行 0 次+error toolResult+
  循环继续到 final)、TestConfirmationsTimeout(300ms 无作答→超时拒绝+reason
  "confirmation timed out"+执行 0 次)、TestConfirmationsValidation(404 未知
  session/400 坏 decision/404 非 pending)。`riskyTool` 用指针接收者避免 vet
  "passing lock by value"(struct 含 sync.Mutex)
- 涉及文件:gateway/gateway.go(waitConfirm/handleConfirm/pendingConfirm/
  confirms/confirmT/route/WithConfirmTimeout)、gateway/confirm_test.go
### 2026-09-10 阶段 2:POST /model 端点 + Claude→DeepSeek 验收
- `handleModel`(DESIGN §3.5):只改 session meta 的 (provider, model),
  不碰运行中的 run——切换在下一轮边界由 core 的 resolveSessionModel 生效;
  provider 必须在 `runner.Providers` 注册(未注册回 400,防拼写错误
  静默落回默认 provider);handleCreate 的 TODO(phase2) 注释移除
  (session meta 现在真的驱动路由)
- **阶段 2 验收 e2e** `TestMidRunModelSwitchE2E`:Anthropic wire(httptest,
  发 event:/data: 帧)与 OpenAI 兼容 wire(httptest)双 provider 注册;
  session 起于 claude/claude-opus,turn 1 的 `switch_model` 工具在 Execute
  里真实 HTTP 调 `POST /model`(集成方会这么暴露切换能力,时机完全确定,
  不用 sleep 竞态);断言:①model_changed 事件经 SSE 可见且指向
  deepseek/gpt-4o-mini ②最终历史 4 条(user/assistant/tool/assistant),
  末条是 DeepSeek 的回答 ③OpenAI 侧收到的 wire 体 model=gpt-4o-mini 且
  历史已转写成 OpenAI 格式(role=tool+tool_call_id、assistant.tool_calls)
  ④两家 provider 各只被调一次 ⑤session meta 已更新
- `TestModelEndpointValidation`:404/400 缺字段/400 未注册 provider
- readSSEUntil helper:消费 SSE 直到标记帧(agent_end),用于断言事件时序

### 2026-09-09 阶段 1 实现
- steering 队列(DESIGN §2.5,替换原 409 语义):
  - 每 session 一个内存 `steeringQueue`(mutex + 切片,push/drain)
  - `New()` 时包装 `runner.Hooks.GetSteeringMessages`:先调集成方自己的钩子,
    再 drain 本网关队列,两者合并在同一轮边界注入(包装不覆盖集成方钩子)
  - handlePrompt 去掉 isRunning 预判:直接 startRun,拿 `ErrSessionBusy` 才
    push 队列并回 `202 {"status":"queued"}`(锁是唯一真相,避免 TOCTOU)
  - prompt handler 重构为 startRun / finishRun:startRun 拿锁+注册 cancel+
    置 running,失败全回滚;finishRun 在事件流关闭时 drain 队列——有排队
    消息则起 follow-up run,否则置 idle
  - **finishRun 顺序坑**:必须先 release() 再 startRun(新 run 要抢锁);
    若竞态中另一个 prompt 抢走锁,把消息重新 push 回队列,让那个 run 收走
- messages 首屏端点:过滤 EntryUser/Assistant/ToolResult(ui_event/compaction
  不进首屏),每条带 seq,JSON 内嵌 core.Message 字段
- 包注释更新:列明当前端点与后续阶段
- 单测 3 例:`TestSteeringMidRun`(首轮流式期间发 steer → 202 queued →
  下一轮边界注入,断言持久化历史 4 条顺序 + **第二次 LLM 调用的上下文里
  真的看到 steer 消息**)、`TestMessagesEndpoint`(ui_event 被滤掉、seq 递增、
  user/assistant/tool/assistant 角色序)、`TestPromptValidation`(404/400)
- 端到端验证:example/gin-integration 真实 HTTP 下 28 帧 SSE(含 tool 往返)

### 2026-09-09 阶段 0 实现
- 路由用 Go 1.22 ServeMux method+pattern(`POST /api/agent/sessions/{id}/prompts`),
  零框架;WithAuthMiddleware 包在 mux 外层
- prompt handler:Run 用 context.Background() 派生 ctx(断线不停跑),
  cancel 存 cancels map 供 stop 端点;事件先 AppendEntry(ui_event)再 broadcast,
  落库优先保证重放精确
- SSE 端点:Last-Event-ID / ?after= 重放 → 订阅实时流 → 15s `: ping` 心跳
- 冒烟验证:event 帧 30 条顺序正确;?after=5 与 Last-Event-ID 重放均正确;
  完成后 stop 返回 "not running"

### 2026-09-09 设计定稿(未实现)
- 从初稿的"Gin handler"改为纯 http.Handler(通用库定位调整)

## 当前状态 / TODO
阶段 5 完成,并补齐会话类型与一轮投影:`GET /sessions`、`GET /turns`、
`WithAgentRunner` 已落地。九个原端点仍在(sessions/prompts/events/messages/
stop/model/confirmations/resume + usage/context-stats)+ request_id 幂等 +
内存多租户限流 + Shutdown。handlers 仍多在 gateway.go,turns 已拆到
turns.go。限流若要多副本需换分布式计数(当前内存)。
