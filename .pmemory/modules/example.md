# example

## 职责
最小集成示例(独立 module):几十行胶水(选 provider、建 Runner、挂 gateway)→
完整跑通 agent。**活文档**:它同时是"陌生项目 100 行集成"验收标准的载体,
集成方式有变必须同步改它。
**不做**:生产级功能(限流细节、多租户面板等),只演示主链路;
配置只用 env(os.Getenv),不引 Viper/zap——库零依赖,示例尽量贴着库走。

## 验收标准(集成标准)
发 prompt → SSE 看流式(text/thinking/toolcall)→ 危险操作弹确认 → 断线重连精确追平 →
kill -TERM 重启后从 interrupted 续跑。

## 依赖关系
- 依赖:库全部包 + Gin(仅 gin-integration);无 ORM/日志/配置框架
- 被依赖:无(纯示例);CI 里作为 e2e 冒烟

## 关键文件路径
- `example/hello/{go.mod,main.go}` — 最小示例:内存 store + Faux + echo 工具 +
  裸 net/http 挂 gateway(独立 module,replace 指向 `../..`)
- `example/gin-integration/{go.mod,main.go}` — Gin 挂载示例:`gin.WrapH(gw)`
  一条路由挂全部 agent API + env 驱动双 provider(默认 OpenAI 兼容
  PI_BASE_URL/PI_API_KEY/PI_MODEL/PI_PROVIDER,无 key 自动落 Faux;
  ANTHROPIC_API_KEY 存在时追加注册 anthropic,PI_ANTHROPIC_MODEL 可选)。
  `WithAgentRunner("assistant"|"coder", ...)` 演示按类型创建/列表;创建须带
  `{"agent":"assistant"}`。
  演示工具:now / quote(业务 API)/ deploy(危险操作,经 BeforeToolCall
  钩子 `Decision{Confirm:true}` 路由到确认流,阶段 3 演示弹确认)/
  内置 bash + read/write/edit(2026-09-11,相对路径基准 `PI_WORK_DIR`,
  空→Getwd);recall_event(SessionTool,L1 归档取回)

## 已知约束与坑
- example 的 go.mod 必须 replace 到本地库路径,发布时替换为版本号
- 演示内容保持最小:业务 API 工具只挂一个 hello 级别的,不堆功能
- **gin 通配符必须命名**:`r.Any("/api/agent/*", ...)` 会 panic
  ("wildcards must be named"),要写 `/api/agent/*catch`;WrapH 包出的 handler
  看到的是完整请求路径,库内 ServeMux pattern 原样匹配,无需改
- `provider.NewOpenAI` 返回 `(*OpenAI, error)` 且把 Provider 字段硬编码为
  "openai"——非 OpenAI 兼容端点要事后 `oa.Provider = name` 覆盖
- **SQLite 持久化档要 `SetMaxOpenConns(1)` + WAL**:否则 core(落 message)与
  gateway(落 ui_event)两个 goroutine 并发写同一 session 报 "database is
  locked"(mattn 多连接池 + busy_timeout/WAL 都压不住)。详见 adapter.md

## 实现细节记录
### 2026-09-16 gin-integration 演示两个 agent
- 默认 Runner 复制一份改 SystemPrompt,经 `WithAgentRunner("assistant")` /
  `WithAgentRunner("coder")` 挂上。创建 curl 改为 `{"agent":"assistant"}`,
  banner 补 `GET /sessions?agent=` 与 `GET /turns`
- 涉及文件:example/gin-integration/main.go

### 2026-09-16 gin-integration 在 protected 组注入 ContextWithUserID
- JWT/Casbin 不进库。示例在 `/api/agent` 前用 `X-User-ID`(默认 demo) stamp
  已验证用户,创建会话不再读 body user_id
- 涉及文件:example/gin-integration/main.go

### 2026-09-11 注册内置 bash + fs(read/write/edit)工具
- 库新落地了 bash/read/write/edit 内置工具(见 tools.md 同日条目),example
  的 registry 追加注册:
  ```go
  workDir := os.Getenv("PI_WORK_DIR") // 空 → os.Getwd()
  ...
  tools.NewBash(workDir),
  tools.NewRead(workDir),
  tools.NewWrite(workDir),
  tools.NewEdit(workDir),
  ```
  workDir 是所有工具相对路径的解析基准(bash 的 Cwd、fs 的 RootDir),1:1 贴
  pi 的"cwd=会话工作目录",env 可调便于演示/测试
- system prompt 补一句:有 `bash` 工具跑 shell 命令、`read`/`write`/`edit` 工具
  做文件操作(路径相对工作目录)
- 注:脚本化 faux provider 不会**自发**调 bash/fs(它只重放固定脚本),注册
  到位即可(同 recall_event 模式)——接真 LLM(设 PI_API_KEY)后,发"跑 echo
  hi"触发 bash、"读某文件/写文件"触发 read/write/edit 才真正走工具链
- 涉及文件:example/gin-integration/main.go(imports + workDir + registry 4 工具
  + system prompt)
### 2026-09-10 注册 recall_event 工具(L1 归档取回演示)
- registry 追加 `core.RecallEventTool{Store: store}`(与 runner 落盘同一个
  store):`tools.NewRegistry(nowTool{}, quoteTool{...}, deployTool{},
  core.RecallEventTool{Store: store})`。它是 SessionTool,循环执行时自动带
  sessionID 走 ExecuteIn,集成方这侧只是注册,无需额外接线
- system prompt 补一句:见到 `[结果已归档,seq=N,可用 recall_event 取回]` 时,
  调 `recall_event` 传该 seq 取回原文——让真实 LLM 知道有这个逃生口
- 说明:脚本化 faux provider 无法让模型**自发**调 recall_event(它只会重放
  固定脚本),所以这条链路在 example 里是"注册到位 + prompt 告知",机制由
  core 单测 `recall_event_test.go` 的 L1→recall 往返覆盖;接真 LLM 后模型
  遇到占位符才会真正调用
- 涉及文件:example/gin-integration/main.go(registry + system prompt)
### 2026-09-10 阶段 5:压缩/治理/限流/幂等接线 + Phase 5 banner(DESIGN §6.4/§7.x)
- **Runner 接线治理**(全 env 可调,免重编译):
  - `Compaction = DefaultCompaction()` 且 `Summarize = core.SummarizeWithStream(streamFn)`
    ——L2 摘要器绑到**默认 provider**(独立无工具调用,fail-safe)。L1 默认开
    (EvictOlderThan=5),L2 靠窗口压力触发
  - `TurnTimeout`(PI_TURN_TIMEOUT,默认 120s)/ `ToolTimeout`(PI_TOOL_TIMEOUT,
    默认 60s)/ `TokenBudget`(PI_TOKEN_BUDGET,默认 0=不限)
  - `CostPerMillion = costPerMillion`:小价目表(gpt-4o-mini $0.15/$0.60、
    claude-sonnet $3/$15,未知回退 $3/$15),让 /usage 永远有非零成本可看
- **限流**:`gateway.New(store, runner, gateway.WithRateLimit(PI_MAX_CONCURRENT=4,
  PI_MAX_PROMPTS_PER_MIN=30))`。默认值宽松,curl 演示不触发 429;多租户部署收紧。
  幂等无需额外接线——客户端发 request_id 即自动生效
- **banner** 增补 phase-5 段:`GET .../context-stats` / `GET .../usage` 的 curl、
  两级压缩自动在轮间跑的说明、request_id 幂等(重复回 202 duplicate 不重跑)、
  限流 env(超 → 429)
- **helpers**:`piDuration(name,def)` / `piInt(name,def)` 读 env(time.Duration/int,
  坏值回退默认);`costPerMillion(m core.Model)` 价目表
- **真实 e2e 冒烟**(faux + 内存 store,`PI_MAX_PROMPTS_PER_MIN=5` 压低上限便于
  演示限流)全过:①完整 run 3 轮 3 工具(now/quote/deploy 含 confirm)到 agent_end;
  ②`GET /usage` 成本核算正确(3 轮 × faux 默认 usage,cost=0.0000225);
  ③`GET /context-stats` currentTokens/contextWindow/usedPercent/compactions=0;
  ④`GET /messages` toolCall/toolResult 配对完整(user/assistant/tool/tool/
  assistant/tool/assistant);⑤prompt 幂等(重复 request_id → duplicate,消息数
  不变);⑥confirm 幂等(duplicate);⑦第 6 个 prompt → **429 prompt rate limit
  exceeded**(正好 = 上限)
- 涉及文件:example/gin-integration/main.go(imports +strconv、helpers、Runner
  治理字段、WithRateLimit、banner)
### 2026-09-10 阶段 4:GORM SQLite 持久化 + SIGTERM 优雅停机 + resume 演示
- **store 选择**(DESIGN §6.5):`PI_DB_PATH` 三态——未设 → 默认文件
  `./pi-agent.db`;设成非空 → 该文件;设成空串 → 内存 store(无持久化)。
  `openGormStore(dbPath)` 打开 SQLite + `AutoMigrate`,返回 (store, closeFn, err)
- **SQLite 调参(阶段 4 关键坑)**:DSN `?_journal_mode=wal` + 打开后
  `db.DB().SetMaxOpenConns(1)`。原因:一次 run 两个 goroutine 并发写
  (core 落 message + gateway 落 ui_event),mattn 多连接池报
  "database is locked",busy_timeout(默认 5000ms)+WAL 都压不住,
  单连接串行化才根治。`Config{Logger:silent, TranslateError:true}`
  把唯一键冲突映射成 `gorm.ErrDuplicatedKey`
- **优雅停机**(DESIGN §6.5):`signal.NotifyContext(ctx, os.Interrupt, SIGTERM)`;
  收到信号 → `gw.Shutdown()`(取消全部 in-flight run、等 runWG 排空、把
  running 的落 `interrupted`)→ `srv.Shutdown(5s ctx)`(停止收新连接,等在途)
  → `closeDB()` → "bye"。banner 增补 phase-4 段(持久化说明 + resume curl)
- **banner**:phase-4 段说明 store 来源 / SSE 断线续跑(`?after=<seq>`) /
  `kill -TERM` 优雅停机 / 重启后 `POST .../resume`
- **真实 e2e 冒烟**(`/tmp/pi_e2e.sh`,faux 档,3 个招牌验收全过):
  ①kill -TERM → 进程优雅退出,`session_models.status=interrupted` 已落 SQLite;
  ②重启第二进程 → 同 DB,status 仍 interrupted、34 条 entries 全在、
  `GET /messages` 返回完整历史;③`POST /resume` → 200 `resumed`、
  RunContinue 补上悬空 deploy 的 error tool_result(seq 续 35)、循环继续
  (status 回 running)。日志 "database is locked" 计数 = 0(修复后)
- 涉及文件:example/gin-integration/main.go、example/gin-integration/go.mod
  (gorm + driver/sqlite)
### 2026-09-10 阶段 3:确认演示挂接(危险工具 deploy + BeforeToolCall 钩子)
- 新增 `deployTool`(危险操作演示,DESIGN §5.3/§11):schema 带 `env` 参数,
  Execute 回 "deployed to <env> (demo — nothing real happened)";本身无副作用,
  危险语义由钩子赋予
- Runner 挂 `Hooks.BeforeToolCall`:`call.Name == "deploy"` 时回
  `Decision{Confirm:true, Reason:"deploy is irreversible"}`(Reason 即弹层里的
  人类风险文案);其余工具回零值放行。gateway.New 自动装 Web ConfirmTool 等待器,
  所以 Confirm 会暂停 run 等 `POST .../confirmations`(默认 120s 超时=拒绝)
- faux 脚本扩成 3 轮:turn1 now+quote(并行批,无确认)→ turn2 调 deploy
  (触发 tool_confirmation_request,run 在此暂停)→ turn3 收尾文案
- registry 注册 deployTool;system prompt 加 `deploy` 工具说明;
  banner 新增第 4 步:流里看到 tool_confirmation_request 后
  `POST .../confirmations {"id":"<confirmationId>","decision":"allow"}`
  (原 messages 步顺延为第 5 步,anthropic 切模型步顺延为第 6 步)
- 集成方视角:确认流对业务代码零侵入——只需 BeforeToolCall 里对危险工具回
  Confirm,其余(事件下发/阻塞等待/作答端点/超时拒绝/决定落库)全在库内
- 涉及文件:example/gin-integration/main.go
### 2026-09-10 阶段 2:双 provider 注册 + 业务 API 工具
- provider 注册改为 `Runner.Providers` map(默认 provider 也注册进去,
  名字与 Runner.Model.Provider 一致):默认 OpenAI 兼容/Faux +
  ANTHROPIC_API_KEY 存在时 `provider.NewAnthropic` + WithRetry,
  model 取 PI_ANTHROPIC_MODEL(缺省 claude-sonnet-4-20250514)
- 新增 `quoteTool` 演示**业务 API 工具挂接**模式:调集成方自己的 HTTP 服务
  (PI_QUOTE_URL/{symbol} → {"price":n});未配置 URL 时回 stub 报价,
  演示流照常有工具往返;错误(网络/非 200/坏 JSON)回 IsError 结果让
  模型自纠,不中断循环
- faux 脚本 turn1 同时调 now + quote 两个工具,顺带演示并行工具批
- banner 增加第 5 步 `POST .../model` 切模型 curl(仅注册了 anthropic
  时打印);/health 现在报 default + 全部已注册 provider 名单
- `Runner.StreamFn/Model` 保留为回退绑定(session 指向未注册 provider 时
  用),注册表才是路由真相

### 2026-09-09 阶段 1 实现
- 新增 `example/gin-integration/`(独立 module):
  - `r.Any("/api/agent/*catch", gin.WrapH(gw))` 一条路由挂全部 agent API,
    另挂一个 gin 原生 `/health` 展示"非 agent 路由留在框架侧"的边界
  - provider 选择:有 `PI_API_KEY` → `provider.NewOpenAI` + `WithRetry(nil cfg)`
    (DESIGN 默认重试参数);没有 → Faux 两档脚本(文本+now 工具调用/收尾),
    开箱不花 token
  - demo 工具用 `now`(返回 UTC RFC3339 时间):真实 LLM 遇到"现在几点"会
    自然发起工具调用,比 echo 更能演示 ReAct 往返
  - 端到端验证(faux 档):/health OK;create → SSE → prompt 全流程 28 帧,
    tool_execution_start/end(now 工具返回真实时间)、两轮 turn、agent_end
    均正确;messages 端点 4 条 seq 标注(user/assistant/tool/assistant)
- 清理:误提交的 `example/hello/hello` 编译产物二进制已删

### 2026-09-09 阶段 0 实现
- hello 示例 ~90 行胶水,验证"陌生项目 100 行集成"标准的第一档
- 脚本化流程:turn1 文本 + echo 工具调用,turn2 收尾;启动时打印三步 curl

### 2026-09-09 设计定稿(未实现)
- 阶段 0 建骨架目录,阶段 1 起逐步补全到验收标准

## 当前状态 / TODO
阶段 5 完成(全库最后一个功能阶段):gin-integration 在阶段 4 基础上追加
两级压缩(SummarizeWithStream 绑默认 provider)、资源治理(Turn/ToolTimeout、
TokenBudget)、成本核算(CostPerMillion)、内存限流(WithRateLimit)、request_id
幂等(自动),全 env 可调;真实 e2e 冒烟 7 项全过(完整 run / usage 成本 /
context-stats / 历史配对 / prompt 幂等 / confirm 幂等 / 限流 429)。
hello + gin-integration 均编译通过,DESIGN §11 阶段 1-5 全部达成。
**recall_event 已注册**(见本文件顶部条目):L1 占位符的取回逃生口在 example
里接线到位 + system prompt 告知;脚本 faux 不自发调用,机制由 core 单测覆盖,
接真 LLM 后模型遇占位符才会真正调。**bash/read/write/edit 已注册**(2026-09-11,
见本文件顶部条目):相对路径基准 `PI_WORK_DIR`,faux 不自发调用,接真 LLM 后
发命令/文件操作才真正触发。
