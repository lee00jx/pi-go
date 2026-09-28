# tools

## 职责
具体工具实现 + 注册表(Name/Schema 反射生成/注册):
bash(shell 命令,进程组 SIGKILL,无命令拦截)、fs(read/write/edit,无沙箱)、
业务 API 挂接框架(逐个挂)。
**不做**:权限策略判定(集成方 BeforeToolCall 钩子)、确认交互流程(core/gateway)、
**sql(pi 本就没有,1:1 不做)**;recall_event 在 core(见下)。

> **现状(2026-09-11)**:bash + fs(read/write/edit)**已落地**,纯标准库零新依赖
> (不引 creack/pty/go-difflib/go-gitignore);**1:1 贴 pi**——无路径沙箱、无写
> 确认、无 gitignore、无命令白/黑名单(见 ADR-020)。所有工具输出喂回 LLM 前由
> core 的 buildContext 包"数据非指令"边界标记(见 core.md)。
> **sql 明确不做**(pi 无此工具);**recall_event 落在 `core/compaction.go`**(实现
> `core.SessionTool`),不在 tools——它按 seq 读 store、是 L1 压缩的配套,放 core
> 更内聚且零新依赖边(见 core.md recall_event 条目 + ADR-019)。

## 对外接口(以 DESIGN §4 为准)
- 均实现 `core.Tool` + `core.DescribedTool`（用途写在 `Description()`，对照 pi）；`registry.go` 负责注册与 JSON Schema 生成
- **bash** `tools.NewBash(cwd string) Bash` — `Name()=="bash"`,`ModeSequential`;
  Schema `{command*}`。普通管道(**非 PTY**)合并 stdout+stderr,持续读进有界
  tail 缓冲(无背压),`SysProcAttr{Setpgid:true}` 使子进程自成进程组;ctx 取消/
  超时 → `syscall.Kill(-pid, SIGKILL)` 杀整组;退出码 128+sig(信号杀)/N(非零
  exit,IsError);tail 截断 2000 行/50KB。**无命令拦截**(1:1 pi,危险命令由
  集成方 BeforeToolCall 钩子把关)。POSIX only,Windows 需 build-tag 变体
- **fs** 三工具共享 `RootDir`(相对路径解析基准,**不做逃逸检查**;空→`os.Getwd()`):
  - `tools.NewRead(rootDir) ReadTool` — `"read"`,`ModeParallel`;`{path*, offset?,
    limit?}`。二进制(首 8KB 含 0x00)→ 拒读提示;offset/limit 1-based 行切片;
    head 截断 2000 行/50KB + `[Showing lines A-B of T]` 续页提示
  - `tools.NewWrite(rootDir) WriteTool` — `"write"`,`ModeSequential`;`{path*,
    content*}`。`MkdirAll` 父目录 + `WriteFile`,回 `Wrote N bytes to <path>`;
    **无确认**(1:1 pi)
  - `tools.NewEdit(rootDir) EditTool` — `"edit"`,`ModeSequential`;`{path*,
    edits:[{oldText,newText}]*}`(兼容单 `oldText/newText`)。对**原始内容**精确
    `strings` 匹配:每段 oldText 唯一(count==1)且段间不重叠,错误细分 not
    found / ambiguous / empty / overlap / no change;从后往前替换保序,探测并
    恢复 CRLF/BOM,回 `Successfully replaced N block(s) in <path>.`;**无 fuzzy、
    无 diff 预览**(v1 简化)
- **截断** `truncateTail`/`truncateHead(s, maxLines, maxBytes)(string, bool)`
  — 共享 helper,UTF-8 多字节安全(字节边界回退到 rune 边界);常量
  `defaultMaxLines=2000`、`defaultMaxBytes=50*1024`(贴 pi)
- **recall_event** 不在本包:`core.RecallEventTool`(实现 core.SessionTool + DescribedTool)
- 统一治理:每次执行经 slog 记结构化日志(tool/duration/字节数/成败)

## 依赖关系
- 依赖:**仅标准库**(os/os-exec/syscall/path/filepath/strings/log/slog)+
  core(Tool 接口)。**不依赖** creack/pty/go-difflib/go-gitignore/session
  (bash/fs 纯标准库;recall_event 读 store 的边在 core,不在本包)
- 被依赖:gateway / example / 集成方(经 registry 注入 RunLoop)

## 关键文件路径
- `tools/registry.go` — Registry(Register/Get/List,注册序稳定)
- `tools/bash.go` — Bash 工具(进程组 SIGKILL + tail 截断 + 128+sig 退出码)
- `tools/fs.go` — Read/Write/Edit 三工具 + fsPath(无沙箱)
- `tools/truncate.go` — truncateTail/truncateHead + 截断常量
- `tools/{bash,fs}_test.go` — bash 5 例 + fs 6 例单测(package tools)
- **recall_event 不在本包**:`core/compaction.go`(RecallEventTool,实现 core.SessionTool)

## 已知约束与坑
- **提示注入基线由 core 加,不在本包**:所有工具输出喂回 LLM 前,由
  `core/compaction.go` 的 `buildContext` 出口对 role==tool 消息包"数据非指令"
  边界标记(只改派生上下文,不动 store)。**本包绝不**把标记写进 ToolResult.
  Output(否则 recall_event 取回的就不是原文了)
- **bash 停止必须杀进程组**(`SysProcAttr{Setpgid:true}` + `syscall.Kill(-pid,
  SIGKILL)`),不能只杀 shell 本身——只杀 shell 会孤儿化它 spawn 的孙进程
- **bash 管道用 `os.Pipe()` 不用 `cmd.StdoutPipe()`**:要把 stdout+stderr 合并
  成一路,得自己持有写端 pipeW 同时赋给 cmd.Stdout 和 cmd.Stderr;Start 后立刻
  关我们的 pipeW(子进程持有自己的 dup),读端才会在整组退出时收到 EOF。
  踩过的坑:误把 `cmd.StdoutPipe()` 的**读端**赋给 cmd.Stderr 会编译不过
- **UTF-8 截断必须回退到 rune 边界**:50KB 字节边界可能落在多字节字符中间,
  truncateTail/Head 用 `utf8.RuneStart` 往前回退,否则输出出半个字符/非法字节
- **edit 排序要带 new 文本一起排**:`sortSpans` 若只排 span 不排对应的新文本,
  span 与替换文本会错位配对(踩过的 bug:"alpha beta"两段乱序替换后顺序错)。
  故 editSpan 内嵌 `new` 字段,排的是"region+新文本"整体
- **edit 对原始内容匹配**:所有 oldText 都对**归一化后(CRLF→LF)的原文**匹配,
  从后往前替换保序;先探测 BOM/CRLF、写完恢复,保证 CRLF 文件不被改行尾
- **无沙箱是刻意的**:RootDir 仅是相对路径解析基准,`..`/符号链接**不检查**
  (1:1 贴 pi,见 ADR-020);要沙箱/写确认由集成方 BeforeToolCall 钩子自加
- 参照源:pi `packages/agent/src/harness/tools/{bash,edit,write,read}.ts`(pi
  **本就无沙箱/无确认/无 gitignore/无注入标记**,本库 1:1 照抄,仅注入标记是自加)

## 实现细节记录
### 2026-09-20 内置 bash/fs 实现 DescribedTool
- `Description()` 写在工具类型上,对照 pi read/bash 的 description 字段;内容按本库实际行为(无图片、bash 无 timeout 参数)
- 涉及文件:`tools/bash.go`、`tools/fs.go`、`tools/bash_test.go`

### 2026-09-11 内置 bash + fs(read/write/edit)工具(1:1 贴 pi,零新依赖)
- **背景**:之前 tools 包只有注册表,bash/fs 一直"待创建"。本次按大师兄要求
  落地("也许以后要用,免得返工"),严格 1:1 贴 pi 的真实实现。挖清 pi 后发现
  它**没有**沙箱/写确认/gitignore/sql/注入标记,故 AskUserQuestion 定死范围:
  fs=1:1 贴 pi、sql=不做、注入标记=加(但落在 core 而非本包)
- **零新依赖(亮点)**:bash 纯 `os/exec`+`syscall.SysProcAttr{Setpgid:true}`+
  `os.Pipe`;fs 纯 `os`/`path/filepath`/`strings`。edit **不做** diff 预览(pi-go
  无 TUI 消费者,回喂 LLM 只需一句确认),故**不引 go-difflib**;不引 creack/pty
  (pi 用普通管道,非 PTY)。tools 包 `GOPROXY=off` 离线构建零摩擦
- **`tools/truncate.go`**:`truncateTail`(保尾, bash 用)/`truncateHead`(保头、
  不半行, read 用),常量 2000 行/50KB(贴 pi),UTF-8 多字节安全
  (`utf8.RuneStart` 回退)。tailBuffer 有界字节缓冲让 bash 持续读不 OOM
- **`tools/bash.go`**:`Bash{Cwd, Logger}`;普通管道合并 stdout+stderr;`pickShell`
  bash→sh 退化;进程组 SIGKILL;`finalizeBash` 拼 (no output)/截断提示/
  [exit code N]/128+sig;`exitCode` 用 `WaitStatus.Signaled()`;每次执行 slog
  记 cmd 摘要/duration/字节数/成败
- **`tools/fs.go`**:`fsPath`(相对→rootDir,不查逃逸,空→Getwd)。Read(二进制
  hasNUL 检测 + 首行过长守卫 + offset/limit 分页 + head 截断)、Write(MkdirAll +
  WriteFile,无确认)、Edit(多段精确替换,唯一+不重叠,从后往前,CRLF/BOM 保持)
- **`editSpan` 内嵌 new 字段**修一个真实 bug:初版 sortSpans 只排 span 不排
  news 切片,两段乱序替换时新文本错位("BETA ALPHA" 而非 "ALPHA BETA")
- **注入标记不在这**:core/compaction.go buildContext 出口加(见 core.md 同日
  条目),本包只保证 ToolResult.Output 是原文
- **单测**:`tools/bash_test.go` 5 例(echo/exit3→IsError+exit code/超时杀组<5s/
  seq5000 截断保尾/UTF8 不拆字)、`tools/fs_test.go` 6 例(read head 截断+offset/
  limit+二进制、write mkdir -p、edit 多段/唯一性报错/CRLF 保持)
- 涉及文件:tools/truncate.go(新)、tools/bash.go(新)、tools/fs.go(新)、
  tools/bash_test.go(新)、tools/fs_test.go(新)、core/compaction.go(注入标记)、
  core/injection_test.go(新)、example/gin-integration/main.go(注册)
### 2026-09-10 recall_event 落 core 而非 tools(文档校正 + 归属说明)
- 原文档把 recall_event 列在"待创建 tools/recall.go",实际它**已实现且落在
  `core/compaction.go`**(RecallEventTool,实现 core 的 SessionTool 可选接口)。
  原因:它按 seq 读 session store、是 L1 压缩占位符的配套解析器,放 core
  与 archivePlaceholder 同文件最内聚,且 core 本就依赖 session,零新依赖边;
  放 tools 会给 tools 引入一条 tools→session 边。详见 core.md recall_event
  条目 + ADR-019
- tools 包本身仍只有 registry.go;bash/fs/sql 依旧未落地
- 涉及文件:仅本文档(无 tools 代码改动);recall_event 代码在 core
### 2026-09-09 阶段 0 实现
- 只有 Registry;演示工具 echo 放在 example/hello 里(不进库,库不背演示代码)

### 2026-09-09 设计定稿(未实现)
- 阶段 3 实现(与 Casbin 工具权限、Web 确认交互同阶段验收)

## 当前状态 / TODO
Registry + **bash + fs(read/write/edit)已落地**(2026-09-11),纯标准库零新依赖,
1:1 贴 pi(无沙箱/无确认/无 gitignore),单测 11 例全绿,example 已注册。
recall_event 已实现(在 core,非本包)。**sql 明确不做**(pi 无此工具)。
TODO(v1 简化,均非阻塞):①bash spill 落盘 + 背压(现只有 tail 缓冲,超 50KB
  的中间输出不保留,LLM 只见截断版)②bash 流式 update / 工具级 timeout(现用
  core 的 ToolTimeout 上限)③edit NFKC fuzzy 回退 + diff 预览(现精确匹配,无
  TUI 消费者)④Windows 支持(bash 进程组杀需 build-tag 变体,现 POSIX only)。
