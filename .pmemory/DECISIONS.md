# 关键技术决策(ADR)

## ADR-001 定位为通用库,但集成方 = 本人项目
- **背景**:2026-09-09 用户明确:agent 要做成通用 Go 框架,方便集成到自己的后端项目;随后补充"集成方也是我"
- **决策**:独立 Go module;core/provider/session/gateway 零框架依赖;
  但**不做开源级通用化**——Viper 可做官方配置 loader,GORM 可做官方 Store 实现(即本人主力栈)
- **放弃的备选**:① 直接写进后端项目当功能模块(不可复用)② 开源级纯净(零 adapter、纯接口),
  对唯一集成方是过度工程

## ADR-002 日志接口用 log/slog 而非 zap
- **理由**:标准库,Go 1.21+;集成方 zap 经 `zap.NewSlogHandler` 一行桥接,库对 zap 零依赖
- **约束**:结构化字段(session_id / turn / tool / duration / usage)统一定义为 slog attr,
  由 gateway / core 层注入,不散落各包

## ADR-003 gateway 用纯标准库 http.Handler
- **理由**:Gin / 裸 net-http / Echo 均可挂载,库不绑框架;SSE 多路器、重放、心跳自研(标准库足够)
- **后果**:无现成 SSE 库可用,心跳(15~30s `: ping`)与 Last-Event-ID 重放需自写并单测

## ADR-004 权限不进库,收敛到 BeforeToolCall 钩子
- **理由**:权限体系因项目而异;agent 语义只需要"工具执行前决策点"
- **边界**:Web 危险操作确认流程(事件 + 阻塞等待 + confirm 端点)在**库内**——
  那是 agent 语义;调 Casbin 做策略判定在**集成方**钩子里

## ADR-005 单一 GORM Store 实现覆盖三种库
- **理由**:GORM 方言覆盖 MySQL / Postgres / SQLite,无需多 adapter
- **注意**:迁移 SQL 以 MySQL 为基准;SQLite 集成时需验证
  DECIMAL/TIMESTAMP 类型差异(在 adapter 文档记录验证结果)

## ADR-006 压缩存 first_kept_seq 指针,与 pi 不同
- **pi 做法**:CompactionEntry 直接内嵌保留的近期消息
- **本库做法**:compaction 条目只存 `{summary, first_kept_seq, tokens_before, model}`,
  保留消息仍从原表按 seq 推导
- **理由**:append-only 更一致、省存储;代价是 buildContext 推导逻辑不能复用 pi 的,自写

## ADR-008 module 路径占位 github.com/lee/pi-go
- **背景**:2026-09-09 用户拍板"纯本地路径起步",不先建 GitHub 仓库
- **决策**:go.mod 用 `github.com/lee/pi-go` 占位;example/hello 用 `replace` 指向本地
- **后果**:发布时改名 = 全局替换 import 前缀 + 去掉 example 的 replace,成本低;
  go.mod 头部已留注释说明

## ADR-009 统一流式类型(StreamFn/StreamEvent/Model)放在 core
- **背景**:DESIGN §1 写"core ← provider(core 依赖 provider 的 StreamFn)",
  但 Message 类型在 core,provider 又要产 Message → 直接照写会循环依赖
- **决策**:StreamFn/StreamEvent/Request/Model 定义在 core,provider 是实现方
  (import core)。依赖箭头与 DESIGN 文字相反,但"core 只认统一流式事件"的
  实质不变。对照 pi:Message/StreamFn 都在更低的 pi-ai 包里,同构
- **记录位置**:core 包 doc comment 已注明这是唯一一处与 DESIGN 箭头的刻意偏差

## ADR-010 steering/followUp 钩子携带 sessionID
- **背景**:原签名 `GetSteeringMessages() []*Message` 没有会话标识,但一个
  Runner 服务全部 session——gateway 的 steering 队列天然是 per-session 的,
  钩子不传 sessionID 就无法定位队列
- **决策**:`GetSteeringMessages` / `GetFollowUpMessages` 签名改为
  `func(ctx context.Context, sessionID string) []*Message`;语义约定为
  **DRAIN**(循环每轮边界只调一次,实现方必须取空队列)
- **时机**:在 gateway 接线前(阶段 1)改契约。当时全库只有自家测试消费,
  零外部成本;若发布后再改就是 breaking change
- **影响文件**:core/loop.go(签名+访问器)、gateway/gateway.go(包装注入)

## ADR-011 切模型走 session meta,轮边界解析,不碰在途 run
- **背景**:DESIGN §3.5 要求 `POST /model` 中途切模型且不打断在途响应;
  同时 PrepareNextTurn 钩子也能指定模型,两者会冲突
- **决策**:
  1. POST /model 只更新 session meta 的 (provider, model),gateway 不接触
     运行中的 run;Runner 在 **run 开始** 与 **每个轮边界** 读 meta 查
     `Runner.Providers`(provider 名 → StreamFn + Model 元数据)
  2. 优先级:PrepareNextTurn 显式返回 Model **压过** session meta
     (业务规则赢存储头),且钩子的选择不回写 meta
  3. 只有 `Model.String()` 真变才 emit model_changed(防每轮噪声);
     session 指向未注册 provider 时 WARN 回退默认绑定,不失败
  4. thinking 块带 Anthropic signature 才能回传同 provider;**无 signature
     降级为 text**(1:1 pi allowEmptySignature=false);跨 provider 直接丢弃
     ——core 为此加 `Block.Signature` 字段(provider 特定不透明数据的标准载体)
  5. 连续 tool 结果在 Anthropic wire 上**合并进一条 user 消息**
     (API 要求角色交替);OpenAI wire 保持一条一 tool 消息
- **放弃的替代**:让 gateway 直接指挥 Runner 换 StreamFn——破坏 core 的
  "Runner 自主循环"边界,且与钩子机制重复;meta+轮边界解析让工具(如
  switch_model)也能走同一通道切模型,HTTP 端点只是其中一种入口
- **时机**:阶段 2,库未发布,改 Runner 结构(StreamFn/Model 降为默认绑定)
  无外部成本
- **影响文件**:core/loop.go、core/message.go(Signature)、core/stream.go
  (MaxTokens)、gateway/gateway.go(handleModel)、provider/anthropic.go

## ADR-007 事件粒度:delta 平铺为一级 AgentEvent
- **pi 做法**:text/thinking/toolcall delta 包在 `message_update.assistantMessageEvent` 二级事件里
- **本库做法**:按 DESIGN §2.2 平铺为一级事件(text_delta / thinking_delta / toolcall_delta)
- **注意**:两种格式的映射关系要写进 core 模块文档,前端只认本库格式

## ADR-012 确认等待放 core 钩子,Web 作答端点在 gateway
- **背景**:DESIGN §5.3 危险命令确认——"暂停 run → 等用户作答 → 允许执行/拒绝回喂 error"
- **本库做法**:
  1. **阻塞点放在 core 的 `ConfirmTool` 钩子**:`BeforeToolCall` 回
     `Decision{Confirm}` → loop emit request 事件 → 调 `ConfirmTool`(阻塞等决定)→
     emit response 事件 → 仅允许才执行。core 只定义钩子契约,不关心决定从哪来
  2. **gateway 装默认等待器**:`New` 里若 `Hooks.ConfirmTool == nil` 就装
     `g.waitConfirm`(注册 pendingConfirm、select 作答/超时/stop);集成方自己设了
     ConfirmTool(如 CLI 交互确认)就不覆盖
  3. **fail-closed**:core 的 `confirmTool` helper 在钩子为 nil 时返回拒绝,
     未接确认器的 Confirm 绝不静默放行(安全默认)
  4. **作答端点** `POST /confirmations {id, decision}`:非阻塞 send 到无缓冲
     channel,先查后发,并发两个作答只有一个 200、另一个 409;超时(默认 120s)
     与 stop 都按拒绝;决定作为 ui_event 落库,刷新/断线后仍可见且作答仍有效
- **放弃的替代**:
  - 让 gateway 在 prompt handler 里直接拦截危险工具再转发——破坏 core 自主循环,
    且无法覆盖非 HTTP 集成(如 CLI),钩子才是 provider-agnostic 的注入点
  - 确认状态持久化到独立表——ui_event 已持久化且可重放,决策信息足够,不额外建表
- **时机**:阶段 3,库未发布
- **影响文件**:core/tool.go(Confirmation)、core/event.go、core/loop.go、
  gateway/gateway.go(waitConfirm/handleConfirm/pendingConfirm)、example 演示挂接

## ADR-013 GORM Store 的 seq 并发写保护:事务内 MAX+1 + SQLite 单连接
- **背景**:阶段 4 持久化。一次 run 有**两个 goroutine** 并发 append 同一
  session(core runner 落 message,gateway 落 ui_event),seq 必须单调不重
- **本库做法**:
  1. **seq 分配**:`AppendEntry` 在**单事务**里 `Row().Scan(COALESCE(MAX(seq),0))`
     → seq = max+1 → `Create`;复合唯一索引 `uk(idx_session_seq)` 兼做兜底
     (两并发同 max → 一个成功一个撞唯一键回 `ErrDuplicatedKey`)
  2. **SQLite 单连接**:mattn/go-sqlite3 默认多连接池 + 并发写报
     `SQLITE_BUSY`,且 `PRAGMA busy_timeout`(默认 5000ms)+ WAL 都压不住
     (busy handler 在池内部态下不可靠)。正解 `db.DB().SetMaxOpenConns(1)`:
     SQLite 本就单写者,单连接串行化彻底消除连接间锁竞争。**这是集成方
     职责**(GORMStore 接收现成 *gorm.DB,不碰连接池),example 示范
- **放弃的替代**:
  - 乐观锁 + 唯一键冲突重试:多一次往返,且重试在单连接下多余
  - 每 session 一个 DB 写锁:seq 已全局单调,再叠会话锁是重复
- **时机**:阶段 4,库未发布
- **影响文件**:adapter/gorm/gorm.go(AppendEntry)、example/gin-integration/main.go
  (openGormStore 的 SetMaxOpenConns + WAL)

## ADR-014 会话执行锁用进程内内存锁,不落库
- **背景**:`session.Store.AcquireLock` 要防同 session 并发 run。若用 DB 行锁/
  状态字段 CAS 落库,进程崩溃后锁可能残留,挡住重启后的 resume
- **本库做法**:GORMStore 的执行锁是**进程内** `sync.Mutex` +
  `map[string]struct{}`(纯内存)。崩溃 → 进程消失 → 锁自然释放,不残留、
  不阻塞 resume;`AcquireLock` 只在锁前 `Count` 校 session 存在(→ErrNotFound)
- **代价与边界**:**锁不跨进程/多实例**——多副本部署同一 session 落到不同
  实例时此锁失效,需换 DB 级锁(行锁 + owner+lease,或 Redis)。当前定位
  (单进程宿主后端)下内存锁够用且最简单
- **MemoryStore 同理**:也是进程内 map,语义一致
- **时机**:阶段 4,库未发布
- **影响文件**:adapter/gorm/gorm.go(locks map)、session/session.go(MemoryStore)

## ADR-015 优雅停机的 interrupted 判定:shuttingDown 标志 + runCtx 已取消
- **背景**:DESIGN §6.5 要求 SIGTERM 后 running 的 session 落 `interrupted`
  (可 resume),但要和"正常跑完 → idle""用户主动 stop → idle"区分开
- **本库做法**:`Gateway.Shutdown()` 置 `shuttingDown` 原子标志 + 取消全部 run
  ctx + `runWG.Wait()`。`finishRun` 在事件流关闭时判
  `shuttingDown.Load() && runCtx.Err() == context.Canceled`:
  - 命中 → `release()` + `setStatus(interrupted)`(用 `context.Background()`,
    不能用已取消的 runCtx 写库)+ 不起 follow-up
  - 未命中 → 原逻辑(drain steering 队列 → follow-up 或 idle)
- **为什么两个条件都要**:仅看 `shuttingDown` 会把停机**后**才结束的 run
  误标;仅看 `runCtx.Err()==Canceled` 会把用户 stop 也误标成 interrupted。
  两者叠加才是"停机导致的取消"
- **配套(core)**:`RunContinue` + `repairDanglingToolCalls`——续跑前为悬空
  tool call 合成 error 结果(provider 拒悬空调用);`isContextStop` 守卫让
  收尾 store 写不因 ctx 取消刷噪声日志
- **时机**:阶段 4,库未发布
- **影响文件**:gateway/gateway.go(Shutdown/finishRun/handleResume/startContinue)、
  core/loop.go(RunContinue/repairDanglingToolCalls/isContextStop)

## ADR-016 FirstKeptSeq 约定用 `>=` 保留,DESIGN 文字 `>` 是笔误
- **背景**:两级压缩用 `first_kept_seq` 指针标记"从哪条消息开始保留"(ADR-006)。
  DESIGN 文字写"保留 `seq > first_kept_seq` 的消息"
- **决策**:读端 `buildContext` 保留 **`seq >= FirstKeptSeq`**;写端 `maybeCompact`
  设 `FirstKeptSeq = seqs[cutIdx]`(cutIdx 是**第一条保留消息**的索引)。二者配对
  语义自洽:FirstKeptSeq 就是"第一条保留消息"的 seq,该消息本身要保留,故 `>=`
- **理由**:`>` 会把 FirstKeptSeq 那条消息丢掉,与"它是第一条保留的"矛盾。
  代码与 core.md 已统一以 `>=` 为准,DESIGN 文字按笔误处理
- **影响文件**:core/compaction.go(buildContext 读、maybeCompact 写)

## ADR-017 资源治理:TokenBudget 含基线跨重启 + defer 闭包落 usage + 成本注入
- **背景**:DESIGN §7.4/§7.6 要每轮/每工具超时、token 预算、成本核算;
  一次 run 结束要把本次 tokens/cost 累加进 session
- **本库做法**:
  1. **TokenBudget 计累计 in+out 且含基线**:run 开始先读 session meta 的
     `TokensIn/TokensOut` 作 `base`,`base + 本次 run 累计 > TokenBudget` 才停。
     含基线 → 预算跨进程重启仍有效(否则重启后从 0 算,预算形同虚设)
  2. **超预算那轮工具不执行**:`budgetHit` 置位后跳过 tool 执行(避免白花钱),
     但 turn 仍干净收尾(发 turn_end)再发 `budget_exhausted` 并停——事件流保持
     平衡,绝不在半截断
  3. **usage 落库用 defer 闭包**:`defer func(){ persistRunUsage(..., runIn,
     runOut, runCost) }()`。Go 的 `defer f(args)` 在**注册时**立即求值 args,
     而 runIn/runOut 是 run 内才累加的局部变量,直接 defer 会固化 0(单测全 0
     由此踩出)。必须包一层闭包,结束时再读当前值
  4. **成本经 CostPerMillion 注入**:Runner 不内置价目表,集成方注入
     `func(Model)(in,out)`;nil 则 cost 恒 0(纯计数)
  5. **超时统一 scopedContext**:`TurnTimeout` 包 streamAssistant、`ToolTimeout`
     包 tool.Execute;d<=0 用 WithCancel(有统一取消点),>0 用 WithTimeout。
     ToolTimeout 超时且 DeadlineExceeded → 回 IsError 结果让模型自纠,不中断循环
- **放弃的替代**:每轮单独算预算(不累加)→ 无法跨轮控总量;成本写死在库 →
  价目随市场变,耦合;persistRunUsage 用 runCtx(已取消的硬停 run 会写不进去,
  改 best-effort,per-entry usage 仍在可重算)
- **时机**:阶段 5,库未发布
- **影响文件**:core/loop.go(Turn/ToolTimeout、TokenBudget、persistRunUsage defer)、
  core/limits.go(scopedContext/costOf/persistRunUsage)

## ADR-018 多租户限流:进程内内存计数(并发同步自增)+ request_id 幂等命名空间
- **背景**:DESIGN §7.5 要 per-user 并发 run 上限 + prompts/分钟窗口(超限 429);
  DESIGN §7.3 要 request_id 幂等(重复不重跑)。用户拍板"内置内存计数"
- **本库做法**:
  1. **限流器是进程内内存**(`limiter`,mutex + map):与 ADR-014 会话锁同边界——
     单进程够用,多副本部署需换分布式(Redis)。`nil` limiter 或维度 0 = 关闭,
     **lenient-off 默认**,单租户部署与旧测试零影响
  2. **并发自增放 startLoop 同步执行**(拿 session 锁后):handler 返回前计数已
     可见 → 测试确定性好(异步自增会有 TOCTOU 窗口)。释放严格配对:
     finishRun + begin 失败路径各一次,不多减不少减
  3. **user 来源优先级**:`ContextWithUserID` 注入的真租户 > session.UserID >
     "default"。集成方鉴权中间件打标即按真租户限流,不强行绑 session
  4. **幂等 per-(session, 命名空间 key)**:key = request_id 加前缀 `"p:"`(prompt)/
     `"c:"`(confirm),两命名空间共享同一 per-session store 但**不撞**;原子
     查并打标。重复 → prompt 202 / confirm 200 `duplicate`,不重跑/不重复作答
     (该 request 触发的事件已在可重放 SSE 流里)。request_id 为空则跳过(不强制)
  5. **幂等 best-effort**:per-session 窗口 `maxDedupPerSession=1000` 溢出整体清空
     最老,极端高并发可能漏判;不追强一致(重复代价低,事件已落库)
- **放弃的替代**:DB 行做幂等(多一次往返,且与 append-only store 混用别扭);
  限流在 handler 里用 session 锁代理(锁是 per-session,限流是 per-user,粒度不同);
  Redis 限流(当前单进程定位下过度,留扩展位)
- **时机**:阶段 5,库未发布
- **影响文件**:gateway/limits.go(limiter/requestDup/ContextWithUserID/errRateLimited)、
  gateway/gateway.go(WithRateLimit/handlePrompt+handleConfirm 幂等与限流/
  startLoop 并发计数/userIDFor)

## ADR-019 session 感知工具用可选接口 SessionTool,不改 Tool.Execute 签名
- **背景**:L1 压缩把老 tool_result 换成占位符 `[结果已归档,seq=N,可用
  recall_event 取回]`(原文没删)。要让"取回"真的可调,需一个 `recall_event`
  工具按 seq 读回原文。但 `Tool.Execute(ctx, call, update)` **拿不到 sessionID**
  ——一个 Runner 服务所有 session,而 recall_event 必须定位到具体 session 的
  store。
- **本库做法**:
  1. **加可选接口而非改签名**:`SessionTool { Tool; ExecuteIn(ctx, sessionID,
     call, update) }`。只有实现它的工具走带 sessionID 的路径
  2. **派发点在 `runOneTool`**(本就握着 sessionID,阶段 3 给确认等待器按会话
     路由就是靠它):`if st, ok := tool.(SessionTool); ok { st.ExecuteIn(...) }
     else { tool.Execute(...) }`。类型断言分叉,3 行搞定,ToolTimeout/超时判定不动
  3. **RecallEventTool 放 core/compaction**(紧挨 archivePlaceholder):
     占位符与解析器是同一 L1 特性的两半;core 本就 import session(Runner.Store
     是 session.Store),**零新依赖边**。读取用 `ListEntries(sessionID, seq-1, 1)`
     凑出"取单条"(Store 无 get-by-seq,不为此加 Store 方法)
  4. **Execute 兜底**:RecallEventTool 仍实现 Execute(内嵌 Tool 要求),但循环
     总会走 ExecuteIn;Execute 只在脱离 run 直调时命中,回 IsError 不猜 session
- **为什么不改 Tool.Execute 直接加 sessionID 参数**:那会**破坏所有现存工具**
  (now/quote/deploy + 集成方工具)的契约,每个都要跟着改;可选接口是**纯新增**,
  普通工具零感知。库虽未发布,但"扩展用新增、不重写既有契约"是可复用库的基本
  纪律(对照 ADR-011 也是"降级默认绑定"而非"改必填")
- **放弃的替代**:把 sessionID 塞 context(隐式,不可发现,且污染 ctx 语义);
  per-session Runner(改 gateway 架构,为单一工具不值);让 recall_event 扫全
  库所有 session(读放大 + 语义错)
- **时机**:阶段 5 后,库未发布
- **影响文件**:core/tool.go(SessionTool)、core/loop.go(runOneTool 派发)、
  core/compaction.go(RecallEventTool)、core/recall_event_test.go、
  example/gin-integration/main.go(注册)

## ADR-020 内置 bash/fs 工具:1:1 贴 pi 无沙箱 + 零新依赖 + 注入标记落 buildContext 出口
- **背景**:tools 包长期只有注册表,bash/fs"待创建"。按大师兄要求落地(也许以后
  要用、免得返工)。参照源 pi 挖清后发现它的真实实现与 DESIGN.md 初稿**偏差很大**:
  pi 的 bash 用**普通管道(非 PTY)**、**无命令白/黑名单**、进程组 SIGKILL、tail
  截断 2000 行/50KB、128+sig 退出码;fs 的 read/write/edit **无路径沙箱、无写
  确认、无 gitignore**;edit 是**多段精确替换**(对原始内容,唯一+不重叠,
  CRLF/BOM 保持)。pi **根本没有** sql 工具,也**没有**提示注入标记。
- **范围(AskUserQuestion 定死)**:
  1. **fs = 1:1 贴 pi**——不做沙箱/写确认/gitignore(pi 本就没有这些,硬加反而
     偏离"语义 1:1"的项目基调)
  2. **sql = 不做**——pi 无此工具,1:1 就不做(DESIGN 初稿的"只读+表白名单"
     是库自拟,pi 无对应物,不凭空造)
  3. **注入标记 = 加**——但这是 pi **没有**的,属库自加的基线防护(见下第 4 点)
- **本库做法**:
  1. **零新依赖(亮点)**:bash 纯 `os/exec`+`syscall.SysProcAttr{Setpgid:true}`+
     `os.Pipe`;fs 纯 `os`/`path/filepath`/`strings`。**不引** creack/pty(pi 不用
     PTY)、go-difflib(edit 不做 diff 预览,pi-go 无 TUI 消费者,回喂 LLM 只需一
     句确认)、go-gitignore(不做)。tools 包 `GOPROXY=off` 离线构建零摩擦
  2. **无沙箱是刻意的**:fsPath 只做"相对路径 → rootDir 解析基准",`..`/符号链接
     **不检查**;RootDir 零值 → os.Getwd()。危险命令/危险路径由**集成方**
     BeforeToolCall 钩子把关(与 pi 一致的"thin-core + 外置防护"哲学)
  3. **v1 简化(均记 TODO)**:bash 无 spill 落盘/背压(只有 tail 缓冲,超 50KB 中
     间输出不保留)、无流式 update、无工具级 timeout(用 core 的 ToolTimeout);
     edit 无 NFKC fuzzy、无 diff 预览;bash 仅 POSIX(进程组杀需 Windows build-tag)
  4. **注入标记落 `core/buildContext` 出口而非 tools 包**:buildContext 是唯一
     "读 store → 构造喂 LLM" 的路径,在此给 role==tool 消息包"数据非指令"边界;
     落库路径(appendMsg/NewToolResultMessage)**完全不动**,store 里永远是工具原
     文,recall_event 取回才拿得到原样。**存什么 ≠ 喂什么**。放 core(而非 tools)
     的理由:①标记是"喂 LLM 的上下文整形",归 core 的 buildContext 职责;②tools
     不该为每个工具各包一次(易漏、易不一致);③保持 tools 纯标准库、零依赖
     core 之外的语义(标记措辞是库级策略)
- **为什么不给 bash/fs 加沙箱/写确认**:偏离 1:1;且"沙箱/确认"是**集成策略**
  而非工具固有语义——不同集成方对"危险"的定义不同,库硬加一套会逼集成方再绕一
  层。pi 的做法(工具中性 + 集成方 BeforeToolCall 把关)更通用,本库跟随
- **放弃的替代**:①bash 用 PTY(creack/pty)——pi 不用,且 PTY 引入平台依赖/
  伪终端控制,纯管道足够 ②edit 引 go-difflib 出 diff 预览——无消费者,回喂一
  句确认即可 ③注入标记放 tools 包每个工具 Output 里——会让 store 也带上标记、
  recall_event 取不回原文,且每个工具都要重复包
- **时机**:阶段 5 后,库未发布
- **影响文件**:tools/{truncate,bash,fs}.go(新)、tools/{bash,fs}_test.go(新)、
  core/compaction.go(buildContext 出口 + wrapToolData)、core/injection_test.go(新)、
  example/gin-integration/main.go(注册 + PI_WORK_DIR)
## ADR-021 两处正确性修复:幂等失败路径回滚 + buildContext 读失败 fail-closed

- **背景**:2026-09-15 全库质量验证发现两处边界缺陷,均属"失败路径语义"类
- **决策 1(request_id 幂等,ADR-018 追补)**:打标(requestDup)之后到
  run 受理之间的**任一失败路径**(限流 429 / 停机 503 / startRun 500 /
  confirm 404/400/409)必须回滚标记(requestRollback)。"标记先于提交"
  会把客户端的正常重试吞成 duplicate——幂等语义应当是
  "受理过的请求才防重",不是"到过的请求都防重"
- **决策 2(buildContext 读失败)**:store 读失败必须**中止轮次**
  (emit TurnEnd + AgentEnd 后 return),绝不能当"空上下文"继续调 LLM。
  理由:store 是历史真相;读失败时喂空上下文 = 让模型在失忆状态下编造,
  产出的回答会污染会话且用户不可见地错。fail-closed 的代价(一次轮次中断)
  远小于 fail-open 的代价(静默丢历史)
- **测试**:core/loop_failclosed_test.go 2 例 + gateway/idempotency_test.go 2 例
- **影响文件**:core/compaction.go(buildContext 三返回值)、core/loop.go
  (run 轮边界中止路径 + RunContinue 透传)、core/compaction.go ContextStats、
  gateway/gateway.go(handlePrompt/handleConfirm)、gateway/limits.go
  (requestRollback)、4 个既有 core 测试适配

## ADR-022 Sampling 零值省略,不在库里写死温度/max_tokens

- **背景**:生成旋钮(temperature / top_p / max_tokens)必须能传到 vLLM;
  集成方"一般用默认、偶尔覆盖"。两种默认候选:①库提供 `DefaultSampling()`
  写死 0.7 / 8192 ②零值不上线,让服务端自己的默认生效
- **决策**:选 ②。`Sampling` 零值 = 请求体省略全部旋钮;`Ptr` 区分
  temperature=0(贪心)与未设置。Anthropic 的 `max_tokens` 仍是 API 必填,
  仅那一家客户端在 0 时补 8192,不回流到公共默认
- **不进公共结构的旋钮**:`presence_penalty` / `frequency_penalty` /
  `top_k` / `min_p` / `stop` / `logit_bias`。它们不是 OpenAI 兼容面与
  Anthropic 的交集,`stop` 还会截断工具 JSON。以后要加,按字段加指针即可,
  不破坏现有零值语义
- **为什么不提供 DefaultSampling()**:压缩的 `DefaultCompaction()` 存在是
  因为零值 = 关闭子系统,需要另一套"开且用 DESIGN 数字"。Sampling 的零值
  已经是"用服务端默认",再包一层只会让人以为库替模型选了温度。写死
  max_tokens 更危险:Agent 工具调用被 length 截断会整批拒绝执行
- **覆盖入口**:`Runner.Sampling` 一次填、每轮拷到 `Request`。L2 摘要请求
  故意保持零值。不按 session / prompt 暴露 HTTP 旋钮
- **时机**:库未发布
- **影响文件**:`core/stream.go`、`core/loop.go`、`provider/{openai,anthropic}.go`、
  README 接入示例

## ADR-023 Provider 私有参数走 ExtraBody,TLS 不按 URL 自动降级

- **背景**:vLLM/Qwen 用 `chat_template_kwargs.enable_thinking=false` 等非公共
  Chat Completions 字段;旧 Django 还按 URL 特征自动 `verify_ssl=false`
- **决策**:OpenAI provider 提供构造 option:`WithExtraBody(map[string]any)`、
  `WithHTTPClient(*http.Client)`、`WithInsecureSkipTLSVerify()`。ExtraBody 只容纳
  provider 私有顶层字段,Typed model/messages/tools/stream/sampling 字段禁止覆盖
- **TLS 边界**:默认严格校验证书;不猜内网域名、不因下划线自动关闭。显式 insecure
  只 clone 当前 http.Client / Transport / TLSConfig,不修改 `http.DefaultTransport`;
  生产私有 CA 优先经 WithHTTPClient 注入
- **理由**:私有参数不污染 core.Request 的跨 provider 公共契约;TLS 安全降级必须
  可审计、局部、显式。ExtraBody 构造时 JSON snapshot,避免 caller map 并发修改
- **时机**:库未发布;NewOpenAI variadic options 保持旧两参数调用源码兼容
- **影响文件**:`provider/openai.go`、`provider/openai_test.go`

## ADR-024 输出协议作为可选 AgentEvent 后处理,不进入 ReAct core

- **背景**:集成项目要求 `<|text|>/<|table|>/<|echart|>` 等可变协议,还要 JSON
  解析、业务校验与安全清洗;不同项目的协议和规则都不相同
- **决策**:新增独立 `output` 包。项目实现有状态 `Processor`,把 text delta
  变为不透明 Chunk;零到多个 `Stage` 依次校验/改写;`Transform` 统一管理每条
  assistant 消息的 Processor 生命周期、context、事件顺序和失败策略
- **事件边界**:新增协议无关 `output`/`output_error` AgentEvent。Transform 只替换
  对外 text_delta,不修改 core 原始 Message、append-only 消息或下一轮上下文。
  Gateway 以不直接 import output 的 EventMiddleware 在落库/SSE 前接入
- **失败策略**:`EmitError` 是安全默认(抑制被拒内容);FallbackToRaw/DropInvalid
  必须显式选择。安全校验失败若默认回退原文会绕过校验,因此不采用
- **明确不做**:pi-go 不内置医院标签/table/echart Processor,本次也不占位
  jsonutil。`core.ParseSalvage` 的工具参数语义不复用到展示 JSON
- **影响文件**:`output/*.go`、`core/event.go`、`gateway/gateway.go`、README、
  DESIGN

## ADR-025 鉴权在集成方,库只接收身份并校验会话归属
- **背景**:JWT/Casbin 属于宿主后端;pi-go 若信任 body `user_id` 会越权。Casbin
  通常只控路由,不会判断 session 是否属于当前用户
- **决策**:库不解析 Token。创建会话只写 `ContextWithUserID`;操作会话默认
  `session.UserID == ctx user`(无 user 则 401,fail-closed 语义后续由 ADR-026
  收紧);`WithAuthorizer` 放行管理员(ADR-026 起改为只放宽)。
  `prompt_id` 标识一次提交(含 queued),`run_id` 标识一次 startLoop;`request_id`
  仍只做 HTTP 幂等。标识进 Message/AgentEvent JSON,不加 Store 列
- **放弃的替代**:把 JWT/Casbin 做进库;用 request_id 当 prompt_id;给 Entry 加列
- **时机**:库未发布
- **影响文件**:gateway/gateway.go、gateway/limits.go、core/message.go、core/event.go

## ADR-026 归属校验 fail-closed 默认,WithAuthorizer 只能放宽
- **背景**:ADR-025 落地后默认 fail-open:无 user 跳过归属、创建回退 "local"。
  医院项目 JWT 必打 user 不会踩坑,但作为通用库,集成方忘注入或误挂公开
  路由,所有 session 即裸奔;WithAuthorizer 整体替换语义下,自定义函数漏写
  owner 判断也会越权
- **决策**:checkAccess 要求身份,缺身份 → 401;**身份检查先于 store 读取**——
  未登录时存在与不存在的 id 同为 401,状态码不泄露 id 存在性。owner 检查恒先
  运行,authorizer 仅在 owner 不匹配时被调用(只可放宽,如放行 admin)。
  本地/测试经 `WithAnonymousUser(user)` 显式降级。401 与 404 分离:
  无身份 401,未知会话/归属不符统一 404 防探测
- **放弃的替代**:保持 fail-open + 文档提醒(靠记忆不靠代码);
  默认配 "local" 匿名(等价裸奔)
- **时机**:库未发布,破坏性变更零成本
- **影响文件**:gateway/gateway.go、gateway/auth_test.go、example/hello/main.go

## ADR-027 会话类型是不透明 `agent` 字符串,Runner 用 map 分发
- **背景**:同一 gateway 要挂多套 SystemPrompt/Tool(如测算/对比/耗材),但不能
  把业务枚举做进库,也不能静默落到默认 Runner(漏传类型会串提示词)
- **决策**:`SessionMeta.Agent` 为不透明字符串,创建后不可改(GORM
  `UpdateSession` 的 map 不含 agent;MemoryStore 整份覆盖,调用方必须 Get 再改)。
  `WithAgentRunner(agent, runner)` 登记映射;`New` 对默认 Runner 与 map 内每一
  个各 `wireRunner` 一次(按指针去重)。有映射时创建必须带已注册 agent(400);
  已有会话类型未登记 → 503。无映射时 agent 可空,走默认 Runner
- **放弃的替代**:三套 gateway 进程;库内枚举 calc/compare;缺省静默用默认
  Runner
- **时机**:库未发布
- **影响文件**:session/store.go、session/memory.go、adapter/gorm/gorm.go、
  gateway/gateway.go、example/gin-integration/main.go

## ADR-028 GET /turns 是现场投影,不靠「属于该 prompt 的 agent_end」
- **背景**:一轮 UI = 一次 `prompt_id`。`agent_end` 每次 run 只发一条,且
  stampRunIDs 会标成最后一个 prompt_id。中途 steering 时,第一轮永远等不到
  「自己的 agent_end」
- **决策**:`GET /sessions/{id}/turns` 扫 entries 聚合,不物化表。活动/结果只
  来自 `ui_event`;`EntryUser` 只补用户原文。一轮收束:出现下一条不同
  `prompt_id` 的 user,或本 run 结束(`agent_end` / session interrupted|ended)。
  收束后有 `output_error`/`budget_exhausted` → failed,否则 complete;
  cancelled 只标尚未收束的最后一轮。结构帧不进 activity/results;
  compaction/model_changed/context_full/budget_exhausted 进会话级 hints
- **放弃的替代**:按 prompt 等自己的 agent_end;把 turns 落成第三张表
- **时机**:库未发布
- **影响文件**:gateway/turns.go、core/loop.go(assistant/tool 补 prompt_id)

## ADR-029 合成轮靠 InjectTurn 写齐 output,不让 /turns 回退读 assistant
- **背景**:`/turns` 结果区只认 ui_event 的 text_delta/output。宿主若只写
  EntryUser+EntryAssistant,模型上下文齐但 results 空,重开对话画摘要会缺一块。
  「results 空再回退 assistant」会与真跑暂空、失败轮、多段 assistant 误判
- **决策**:提供进程内 `Gateway.InjectTurn`(暂无 HTTP)。同一 prompt_id 顺序写
  user、assistant(`end_turn`)、`output` ui_event。不造 run_id/agent_end;
  idle 下现有投影自然 complete。running → ErrSessionRunning。欢迎/测算文案
  与 resultId 指针留在宿主(OutputData 不透明)
- **放弃的替代**:/turns 回退读 EntryAssistant;把 welcome 语义做进库;HTTP 同步上
  (同进程够用,跨进程再加)
- **时机**:库未发布
- **影响文件**:gateway/inject.go、gateway/inject_test.go

## ADR-030 工具用途用可选接口 DescribedTool,不改 Tool 必填方法
- **背景**:原 pi 的 `ToolDefinition.description` 写在工具对象上,随 tools 数组
  发给模型(`function.description` / `tools[].description`)。pi-go 的 `Tool`
  只有 Name + Schema,Anthropic 代码里还留着「无 Description 是 semver 决策」。
  医院侧只能把用途堆进系统提示词,并误加「先用工具再回答」。
- **本库做法**:
  1. **可选接口** `DescribedTool { Tool; Description() string }`,同 ADR-019
     的 SessionTool,不强迫现有工具改编译
  2. `ToolDescription(t)` trim 后给 OpenAI / Anthropic 序列化;`omitempty` 使
     未实现的工具不出现空 description
  3. 内置 bash/read/write/edit/recall_event 补 `Description()`,对照 pi 把
     用途写在工具函数旁
- **为什么不加到 Tool 接口**:会破坏所有现存实现(测试 fake、example、集成方);
  可选接口是纯新增。集成方按 Python/pi 习惯在工具类型上写 `Description()` 即可
- **时机**:库未发布
- **影响文件**:core/tool.go、provider/openai.go、provider/anthropic.go、
  tools/bash.go、tools/fs.go、core/compaction.go、example、README、DESIGN.md
