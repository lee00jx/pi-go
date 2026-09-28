# CHANGELOG(最新在最上面)

- [2026-09-20 14:55] -- [core+provider+tools] 可选 DescribedTool：用途写在工具上并下发给 OpenAI/Anthropic（对照 pi description）

- [2026-09-17 11:03] -- [gateway] GET /turns 与 /messages 带出 Entry.CreatedAt,气泡时间不再缺字段
- [2026-09-16 21:20] -- [provider] OpenAI 兼容流把 vLLM/Qwen 思考打成
  thinking_delta:reasoning_content 与 `<think>` 标签,终态消息带 thinking 块

- [2026-09-16 17:17] -- [gateway] 新增 InjectTurn:不调 LLM 写入 user+assistant+
  output ui_event,供宿主合成一轮;running 拒绝;/turns 不回退读 assistant

- [2026-09-16 15:30] -- [gateway+session+core] 会话类型 agent、WithAgentRunner
  分发、GET /sessions 与 GET /turns 投影;assistant/tool 补 prompt_id

- [2026-09-16 13:52] -- [gateway] prompt 限流租户改走 currentUser,删除
  第四套身份解析 userIDFor(不再无 user 时多查一次 store)

- [2026-09-16 13:40] -- [gateway] 身份检查提前到查库之前:未登录时存在与
  不存在的 session id 同为 401,堵状态码探测;ADR-025 表述修正

- [2026-09-16 11:55] -- [gateway] 归属校验改 fail-closed(无身份 401),
  WithAnonymousUser 显式降级;WithAuthorizer 改为只放宽的升级钩子

- [2026-09-16 11:30] -- [gateway+core] 创建会话只信认证上下文,统一 session
  归属校验与 WithAuthorizer;prompt_id/run_id 关联标识(queued 先 P 后绑 R)

- [2026-09-16 10:36] -- [gateway] 修复 WithBasePath:Option 先于 mux 注册,
  Auth 改为事后包一层;自定义前缀与鉴权组合不再失效。

- [2026-09-16 00:28] -- [provider+output+gateway] 新增 OpenAI ExtraBody/单客户端
  TLS options、协议无关 Processor→Stage→Transform 输出管道及落库/SSE 中间件;
  默认校验失败抑制内容,不内置 JSON repair 或业务协议。

- [2026-09-15 22:30] -- [core+provider] Sampling 默认策略定稿:零值 = 线上省略,
  不提供 DefaultSampling() 硬编码数字;公共字段只保留 temperature/top_p/
  max_tokens/seed。README 接入示例补"不填走服务端默认、偶尔覆盖"的写法。

- [2026-09-15 22:10] -- [全局] 发布前确认:race 全量测试 + 两 example 编译全绿;
  新增 README.md(接入路径:replace + 自定义 Tool + SystemPrompt + WrapH);
  新增 .gitignore;删除 example 编译产物与 .DS_Store;registry.go/tool.go
  过期"phase 0 / sql 将落地"注释改为现状(sql 不做)。尚未 git init,
  建仓与远端由集成方决定。

- [2026-09-15 13:40] -- [gateway] startLoop 头部重构遗留注释清理:删掉
  startRun 旧 doc comment 残段(含重复的"断线不停跑"句)。注释级修改,
  gateway 测试复跑通过。

- [2026-09-15 12:30] -- [core+gateway] 质量验证两缺陷修复:①buildContext 读失败
  fail-closed(签名加 error 返回,run 轮边界读失败中止 turn 不再空上下文喂
  LLM,RunContinue 透传 err,ContextStats 回零值,4 个测试文件适配,
  loop_failclosed_test.go 2 例回归);②幂等失败路径回滚(limits.go 加
  requestRollback,handlePrompt/handleConfirm 打标后任一失败路径统一回滚,
  idempotency_test.go 2 例:429 后同 id 重试仍是 429 非 duplicate)。
  附带:AfterToolCall 钩子补并发安全约束注释。race 全量测试全绿,
  两 example 编译通过。

- [2026-09-15 10:40] -- [全局] 代码质量验证通过:race 全量测试全绿、全源码精读复审、
  覆盖率 core 81.2% / gateway 78.5% / adapter 86.1% / provider 70.2% / tools 69.4%。
  修正两处机械问题:gofmt 统一(22 个文件因注释对齐未格式化,已 -w 归一)、
  core/loop.go 过期 TODO 注释删除(注入边界标记已在 compaction.go
  wrapToolData 实现,注释与代码不符)。两 example 编译通过。

- [2026-09-14 16:10] -- [core] buildContext 签名重构:摘要独立返回
  (string, []*Message),不再嵌入 msgs[0]。此前摘要作为 RoleUser 消息塞在
  msgs 头部,调用方无法区分"摘要"与"真实 user 消息",system prompt 也拿不到
  原文。改为 `(summary string, msgs []*Message)`:summary 喂 system prompt,
  msgs 纯尾部。同步修 maybeCompact 内 `summary, err :=` → `=`(两变量已在
  上游声明,`:=` 报 no new variables)、`AppendEntry` 同理;applyL1 也已改为
  返回 `([]*Message, map[int]bool)`(归档 seq 集),测试全适配。涉及 7 文件:
  compaction.go(buildContext/buildContextFromEntries/maybeCompact)、
  loop.go(3 调用点已适配)、compaction_test.go(3 例)、compaction_l2_test.go
  (2 例断言改为查 summary 返回值)、injection_test.go、recall_event_test.go。

- [2026-09-11 10:05] -- [tools] 内置 bash + fs(read/write/edit)工具 + 工具输出
  注入标记(1:1 贴 pi,零新依赖)。tools 加 truncate.go(tail/head 截断,UTF-8
  多字节安全,2000 行/50KB)、bash.go(普通管道非 PTY、进程组 SIGKILL、tail
  截断、128+sig 退出码、无命令拦截)、fs.go(read head 截断+offset/limit 分页、
  write mkdir -p 无确认、edit 多段精确替换对原始内容唯一+不重叠+CRLF/BOM 保持);
  纯标准库 os/exec+syscall,不引 creack/pty/go-difflib。core/compaction.go
  buildContext 出口对 role==tool 消息包"数据非指令"边界标记(只改喂 LLM 的
  派生上下文,不动 store,recall_event 取回仍是原文——存≠喂)。sql 明确不做
  (pi 无)。example 注册 bash/read/write/edit(相对路径基准 workDir,env
  PI_WORK_DIR)。单测 bash 5 例 + fs 6 例 + core 注入 1 例全绿,全库 build/vet/
  test + 两 example 编译通过。ADR-020 记录 1:1 无沙箱 + 零新依赖 + 注入标记落
  buildContext 出口 + v1 简化点。

- [2026-09-10 17:09] -- [core] recall_event 工具 + SessionTool 非破坏接口:兑现
  L1 占位符"[…可用 recall_event 取回]"。core 加可选接口 SessionTool{Tool;
  ExecuteIn(ctx,sessionID,call,update)},runOneTool 类型断言分叉(3 行,sessionID
  本就握着),现有工具零改动;RecallEventTool{Store} 放 core/compaction(紧挨
  archivePlaceholder,零新依赖边),按 seq 用 ListEntries(seq-1,1) 读回原文,
  坏参/查无/nil store 均回 IsError 不中断循环。example 注册 + system prompt
  告知。单测 3 例(派发拿到正确 sessionID / L1→recall 往返取回原文 / 错误分支)
  全绿,全库 build/vet/test + 两 example 编译通过。ADR-019 记录"扩展用新增不
  改 Tool.Execute 签名"。

- [2026-09-10 15:47] -- [全局] 阶段 5 完成:压缩 + 治理(DESIGN §6.3/§6.4/§7.3/
  §7.5/§7.6)——全库最后一个功能阶段,DESIGN §11 阶段 1-5 全部达成。core 加
  token 估算(中文加权,锚真实 usage)+ 两级压缩(L1 驱逐 + L2 迭代 LLM 摘要,
  fail-safe + 安全切点永不落 tool_result,FirstKeptSeq 用 >=)+ 资源治理
  (Turn/ToolTimeout、TokenBudget 含基线跨重启、CostPerMillion 成本核算 + usage
  落库,defer 闭包规避参数提前求值)+ ContextStats;gateway 加 GET /usage +
  GET /context-stats + request_id 幂等(p:/c: 命名空间,重复不重跑)+ 内存多租户
  限流(WithRateLimit 并发/prompts-min → 429,ContextWithUserID 按真租户);
  session.md 文档校正(压缩/token 实际在 core 非 session,以代码为准);example
  全接线(env 可调)+ 7 项 e2e 全过。单测全绿,全库 build/vet/test 通过。

- [2026-09-10 11:47] -- [全局] 阶段 4 完成:持久化 + 恢复(DESIGN §6.5/§11)。
  adapter/gorm 新建 GORMStore(实现 session.Store 全量 + Migrate,7 例单测含
  跨重启持久化;seq 事务内 MAX+1 + 复合唯一索引,SQLite 档需 SetMaxOpenConns(1)
  根治 "database is locked");core 加 RunContinue 续跑入口 + repairDanglingToolCalls
  (为悬空 tool call 合成 error 结果)+ isContextStop 日志守卫;gateway 加
  Shutdown() 优雅停机(取消 in-flight run + runWG 排空,finishRun 据
  shuttingDown&&ctx.Canceled 落 interrupted)+ POST /resume 端点 + 落库容错
  (persist 循环 ctx 取消不刷噪声且继续 drain 防死锁,recover_test.go 5 例真实
  HTTP 验收);example/gin-integration 接 GORM SQLite store(PI_DB_PATH 三态
  选库)+ SIGTERM 优雅停机 + resume banner,真实 e2e 冒烟 3 验收全过
  (kill→interrupted 落库 / 重启历史存活 / resume 续跑,日志 0 次 database locked)。
  新增 ADR-013/014/015;全库 build/vet/test 全绿
- [2026-09-10 03:20] -- [全局] 阶段 3 完成:危险命令确认流(DESIGN §5.3)。
  core 确认钩子接线(BeforeToolCall `Decision.Confirm` → `ConfirmTool` 等待器,
  未接则 fail-closed 拒绝;`tool_confirmation_request`/`response` 事件,决定作为
  ui_event 落库可重放;tool.go 加 Confirmation、event.go 加 Reason+构造器、
  loop.go 三处 execute* 带 sessionID,4 例单测);gateway 新增
  `POST .../confirmations` 端点(pendingConfirm 按 call.ID 索引,waitConfirm
  三向 select:作答/120s 超时=拒绝/stop=拒绝,handleConfirm 404/400/409 校验,
  WithConfirmTimeout 可覆盖,4 例真实 HTTP 阻塞+唤醒测试);example 挂
  `deployTool` 危险工具演示(BeforeToolCall 路由 Confirm,faux 扩 3 轮 turn2
  触发暂停)。全库 build/vet/test 全绿
- [2026-09-10 02:06] -- [全局] 阶段 2 完成:provider 新增 Anthropic Messages API
  手写客户端(SSE event:/data: 解析、thinking+signature 往返、usage 两段合并、
  stop_reason 1:1 映射、tool_result 历史合并/无 sig thinking 降级,7 例单测);
  core 多 provider 支持(Runner.Providers/resolveSessionModel/streamFnFor、
  轮边界切模型 hook 优先、Block.Signature、Request.MaxTokens,3 例单测);
  gateway 新增 POST /model 端点 + DESIGN §11 验收 e2e(Anthropic→OpenAI 兼容
  双 httptest provider 中途切换,model_changed 事件 + 转写历史断言);
  example/gin-integration 双 provider 注册(ANTHROPIC_API_KEY 启用 anthropic)
  + 业务 API 工具模式(quoteTool);全库 build/vet/test 全绿
- [2026-09-09 23:45] -- [全局] 阶段 1 完成:core 截断保护(length/error 整批拒执行)
  + 并行工具执行(sequential 降级/原序落盘)+ salvage 解析器(4 级降级)+
  steering/followUp 钩子加 sessionID(ADR-010);provider 新增 OpenAI 兼容客户端
  (SSE 解析/工具调用累积/截断参数抢救/APIError 重试分类)+ WithRetry
  (指数退避全 jitter/出内容不重试/fallback 打标签发 model_changed);
  gateway steering 队列(运行中 prompt → 202 queued,轮边界注入)+ messages
  首屏端点 + startRun/finishRun 重构;example 新增 gin-integration
  (gin.WrapH 挂载 + PI_* env 真实 provider 档 + Faux 兜底);
  全库 build/vet/test 全绿 + Gin 真实 HTTP 冒烟(SSE 28 帧含工具往返)通过
- [2026-09-09 22:32] -- [全局] 阶段 0 完成:go.mod(module 占位 github.com/lee/pi-go)+ 五包骨架
  (core 主循环/事件/工具接口/钩子、session Store 契约+MemoryStore、provider Faux、
  tools Registry、gateway 纯标准库 SSE/prompt/stop)+ core 单测 3 例全绿 +
  example/hello 端到端冒烟通过(事件流顺序、重放 after=/Last-Event-ID、stop 均验证)
- [2026-09-09 18:02] -- [全局] 初始化 .pmemory;DESIGN.md 按"通用库定位"改版:新增 §0.1 库边界,
  核心零框架依赖,日志改 slog,gateway 改纯标准库 http.Handler,Casbin 移出(Case 进钩子),
  GORM 降为官方 adapter(覆盖 MySQL/PG/SQLite),补 transformContext/getFollowUpMessages 钩子,
  实施阶段新增阶段 0
- [2026-09-09 17:30] -- [全局] DESIGN.md 初稿完成(参照 pi 的 ReAct 循环 + 事件流模型)
