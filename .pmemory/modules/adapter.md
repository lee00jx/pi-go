# adapter

## 职责
官方可选适配层:目前仅 `adapter/gorm` —— 用 GORM 实现 `session.Store`
(append + seq range 查询 + 会话元信息 + 执行锁),GORM `AutoMigrate` 建表。
**不做**:配置加载(库内 FromViper 在根包或 config 处)、日志、鉴权;
也不持有连接池配置(接收现成的 `*gorm.DB`)——DSN/WAL/连接数是集成方职责。

## 对外接口(已实现,以 session.Store 为准)
- `New(db *gorm.DB) *GORMStore` — 包装已有 *gorm.DB,不自己 Open
- `(*GORMStore).Migrate(ctx) error` — `AutoMigrate(SessionModel, EntryModel)`,
  幂等(重启进程对同一文件再 migrate 无副作用)
- 实现 `session.Store` 全量方法:
  - 会话:`CreateSession` / `GetSession` / `UpdateSession` / `DeleteSession` /
    `ListSessions(SessionQuery)`(UserID + 可选 Agent + Limit)
  - 条目:`AppendEntry` / `ListEntries`(append-only,seq 单调)
  - 锁:`AcquireLock(ctx, sessionID) (release func(), error)`
- 表模型(GORM 默认命名,**注意不是 DESIGN §6.2 的裸名**):
  - `SessionModel` → 表 `session_models`(ID 主键,UserID index,
    `(user_id, agent)` 复合索引,Agent 不透明类型创建后 Updates 不含该列,
    Provider/Model/Status/Title, TokensIn/TokensOut int64, Cost float64,
    CreatedAt/UpdatedAt)
  - `EntryModel` → 表 `entry_models`(ID autoIncrement 主键,
    `uniqueIndex:idx_session_seq` 复合唯一 = session_id+seq,
    Type, Payload/Usage []byte, CreatedAt)
- 集成方:Viper 配 DSN → 建 `*gorm.DB`(见下方"已知坑"的 SQLite 调参)→
  `New(db)` → `Migrate(ctx)` → 传入 `gateway.New`

## 依赖关系
- 依赖:session(Store 接口)、gorm(库内唯一允许 gorm 的包,可选引入)
- 被依赖:example、集成方项目;不用官方 Store 的集成方零感知
- 测试驱动:gorm.io/driver/sqlite(mattn/go-sqlite3)——adapter 单测全跑在
  临时 SQLite 文件上,验证"跨重启持久化"这一阶段 4 核心

## 关键文件路径
- `adapter/gorm/gorm.go` — GORMStore + 两个 Model + 全部 Store 方法
- `adapter/gorm/gorm_test.go` — 7 例单测(CRUD/List/Entries/Lock/
  Migrate 幂等/**跨重启持久化**)

## 已知约束与坑
- **SQLite 并发写必须 `SetMaxOpenConns(1)`(阶段 4 踩坑,最重要)**:
  一次 run 有**两个 goroutine** 并发写同一 session(core runner 追加
  message,gateway 落 ui_event),mattn/go-sqlite3 的**默认多连接池**在这种
  负载下报 `SQLITE_BUSY`("database is locked")。`PRAGMA busy_timeout`
  (mattn 默认就是 5000ms)+ WAL 都**压不住**——busy handler 在池内部态下
  不可靠。正解是 `db.DB().SetMaxOpenConns(1)`:SQLite 本就是单写者,单连接
  串行化彻底消除连接间锁竞争。WAL(`?_journal_mode=wal`)仍建议保留
  (崩溃恢复 + 单连接下无副作用)。这是**集成方职责**(本包不持有连接池),
  example 已示范
- **`Config.TranslateError: true` 必开**:否则 sqlite 驱动不把唯一键冲突
  (1555/2067)映射成 `gorm.ErrDuplicatedKey`,`CreateSession` 的兜底
  `errors.Is(res.Error, gorm.ErrDuplicatedKey)` 判定失效。双保险:
  CreateSession 先 `Count` 预检 + TranslateError 兜底
- **`res.RowsAffected` 是字段不是方法**(GORM v2),写成 `RowsAffected()` 编译不过
- **seq 并发保护**:`AppendEntry` 在**单事务**里 `Row().Scan(COALESCE(MAX(seq),0))`
  → seq=max+1 → Create;复合唯一索引 `idx_session_seq` 兜底(append 冲突即锁,
  见 ADR-013)
- **执行锁是进程内内存锁**(`sync.Mutex` + `map[string]struct{}`),**不落库**
  (ADR-014):崩溃后锁自然消失,不会挡住重启后的 resume;代价是多进程/
  多实例部署时锁不跨实例
- ADR-005 遗留:一套实现覆盖 MySQL/PG/SQLite,但 **SQLite 集成前必须验证**
  DECIMAL(cost)/TIMESTAMP 类型差异;当前用 float64 + time.Time,SQLite 档已验证
- GORM 只做 append 与 range 查询,不搞复杂查询优化

## 实现细节记录
### 2026-09-16 SessionModel.Agent + ListSessions(SessionQuery)
- 加 `Agent` 列与 `(user_id, agent)` 索引。`UpdateSession` 的 Updates map
  **不含 agent**(类型不可变)。`ListSessions` 改吃 `session.SessionQuery`。
  单测:按 agent 过滤 + 改 status 不抹掉 Agent
- 涉及文件:adapter/gorm/gorm.go、adapter/gorm/gorm_test.go

### 2026-09-10 阶段 4:GORM Store 实现 + 跨重启持久化验收
- `gorm.go` 新建,实现 `session.Store` 全量:
  - `CreateSession`:空 ID → `ErrInvalidMeta`;`Count` 预检 → `ErrSessionExists`;
    否则 `Create`,`errors.Is(err, gorm.ErrDuplicatedKey)` 兜底 → `ErrSessionExists`
  - `GetSession`:`First(&m,"id = ?",id)`,`ErrRecordNotFound` → `ErrNotFound`
  - `UpdateSession`:`Updates(map[string]interface{}{全字段})`(map 而非 struct,
    避免零值陷阱——title 可回写空串);`RowsAffected==0` → `ErrNotFound`
  - `DeleteSession`:先删 session(`RowsAffected==0` → `ErrNotFound`)再删其 entries
  - `ListSessions`:`Order("updated_at DESC")` + userID 过滤 + limit
  - `AppendEntry`:事务内 First 校存在(→`ErrNotFound`)+ `MAX(seq)` + Create
  - `ListEntries`:先校 session 存在(不存在返回**空列表**而非 `ErrNotFound`);
    `Where("session_id=? AND seq > ?").Order("seq ASC")` + limit
  - `AcquireLock`:Count 校存在(0→`ErrNotFound`)+ 内存 locks map(已持→
    `ErrSessionBusy`),返回 release 闭包
- `Migrate` = `AutoMigrate(SessionModel, EntryModel)`
- 7 例单测全绿:TestMigrateIdempotent / TestSessionCRUD / TestListSessions
  (2ms sleep 造严格有序)/ TestEntries / TestLock / **TestPersistenceAcrossRestart**
  (进程#1 写+标 interrupted+close DB → 进程#2 重新 Open 同文件,验证
  status=interrupted 存活 + entries 全在 + 新 append 的 seq 从 3 续上)
- 踩坑记录(详见"已知约束与坑"):TranslateError 必开 / RowsAffected 是字段 /
  sqlite 表名带 _models 后缀 / SQLite 并发写要 MaxOpenConns(1)
- 涉及文件:adapter/gorm/gorm.go、adapter/gorm/gorm_test.go、根 go.mod
  (gorm v1.31.2 + driver/sqlite v1.6.0,go 1.24.0)
### 2026-09-09 设计定稿(未实现)
- 阶段 4 实现,与事件落库/断线重放同阶段验收

## 当前状态 / TODO
阶段 4 完成:GORMStore 实现 `session.Store` 全量方法 + `Migrate`,7 例单测
(含跨重启持久化)全绿;根 go.mod 已加 gorm + sqlite driver。
集成方侧注意:SQLite 档要 `SetMaxOpenConns(1)`(example 已示范)。
后续:若上 MySQL/PG,按 ADR-005 验证 DECIMAL/TIMESTAMP 差异并在此记录;
多实例部署需把执行锁换成 DB 级锁(当前为进程内内存锁,见 ADR-014)。
