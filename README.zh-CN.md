# pi-go

[English](README.md)

pi-go 是一个把大模型 Agent 嵌进 Go 后端的库。你提供工具和系统提示词，pi-go 负责
对话循环（模型 → 调工具 → 再问模型……）、通过 SSE 把过程推给前端，并保存会话记录。

它是库，不是独立服务：编译进你的二进制，以 `http.Handler` 的形式挂到 Gin 或标准库
`net/http` 上。前端访问的始终是你后端的地址。

## 安装

```
go get github.com/lee00jx/pi-go@latest
```

需要 Go 1.24+。

## 快速上手

三步：写一个工具，建一个 `Runner`，挂载 gateway。

```go
// 1. 工具就是你的业务接口，写清楚描述，模型才知道什么时候调用。
type queryOrder struct{}

func (queryOrder) Name() string        { return "query_order" }
func (queryOrder) Description() string { return "按订单号查询订单，返回支付状态。" }
func (queryOrder) Schema() json.RawMessage {
    return json.RawMessage(`{"type":"object","properties":{"order_id":{"type":"string"}},"required":["order_id"]}`)
}
func (queryOrder) ExecutionMode() core.Mode { return core.ModeParallel }
func (queryOrder) Execute(_ context.Context, call core.ToolCall, _ func(core.ToolUpdate)) (core.ToolResult, error) {
    var args struct{ OrderID string `json:"order_id"` }
    if json.Unmarshal(call.Arguments, &args) != nil || args.OrderID == "" {
        return core.ToolResult{Output: "缺少 order_id", IsError: true}, nil
    }
    return core.ToolResult{Output: "订单 " + args.OrderID + "：已支付"}, nil
}

// 2. Runner 组合模型、工具和提示词。
store := session.NewMemoryStore()
oa, _ := provider.NewOpenAI(os.Getenv("PI_BASE_URL"), os.Getenv("PI_API_KEY"))
runner := &core.Runner{
    Store:        store,
    StreamFn:     provider.WithRetry(oa.Stream, nil),
    Model:        core.Model{Provider: "openai", ID: "gpt-4o-mini", ContextWindow: 128000},
    Tools:        tools.NewRegistry(queryOrder{}).List(),
    SystemPrompt: "你是订单助手。",
    MaxTurns:     8,
}

// 3. 把 gateway 挂在鉴权之后。请求里没有用户身份会直接返回 401。
gw := gateway.New(store, runner)
protected := r.Group("/", func(c *gin.Context) {
    uid := "..." // 从你的 JWT / 会话中取
    c.Request = c.Request.WithContext(gateway.ContextWithUserID(c.Request.Context(), uid))
})
protected.Any("/api/agent/*catch", gin.WrapH(gw))
```

可运行的示例：[`example/hello`](example/hello/main.go)（最小）和
[`example/gin-integration`](example/gin-integration/main.go)（完整：真实模型、SQLite、
上下文压缩、优雅退出）。不设置 `PI_API_KEY` 时 Gin 示例使用假模型，可离线运行。

## HTTP 接口

所有路径都在前缀 `/api/agent` 下（可配置，见下文），调用方需按第 3 步完成鉴权。

| 方法 | 路径 | 用途 |
|---|---|---|
| POST | `/sessions` | 创建会话（可选 `{"agent":"..."}`） |
| GET | `/sessions` | 当前用户的会话列表（`?agent=`、`?limit=`） |
| POST | `/sessions/{id}/prompts` | 发送消息：`{"text":"..."}` |
| GET | `/sessions/{id}/events` | SSE 事件流，断线后带 `Last-Event-ID` 续传 |
| GET | `/sessions/{id}/messages` | 消息历史 |
| GET | `/sessions/{id}/turns` | 按每条用户消息分组的历史 |
| POST | `/sessions/{id}/stop` | 停止当前运行 |
| POST | `/sessions/{id}/confirmations` | 同意或拒绝等待确认的工具调用 |
| POST | `/sessions/{id}/model` | 从下一轮起切换模型 |
| POST | `/sessions/{id}/resume` | 继续因重启中断的运行 |
| GET | `/sessions/{id}/usage` | Token 用量和费用 |
| GET | `/sessions/{id}/context-stats` | 上下文窗口占用情况 |

前端的典型流程：创建会话 → 打开事件流 → 发送消息。

## 常用配置

**路由前缀。** `gateway.WithBasePath("/api/v1/agent")`。gateway 内部会按完整路径再匹配
一次，所以这个值必须和你的路由转发过来的路径一致，否则请求能进 gateway 但会返回 404。

**一个 gateway 挂多个 Agent。** 按名字注册多个 Runner，前端创建会话时传
`{"agent":"calc"}`。

```go
gw := gateway.New(store, calcRunner,
    gateway.WithAgentRunner("calc", calcRunner),
    gateway.WithAgentRunner("compare", compareRunner),
)
```

**危险工具先确认。** 工具本身保持简单，在钩子里要求确认；运行会暂停，直到前端调用
`/confirmations`。

```go
runner.Hooks = core.Hooks{
    BeforeToolCall: func(_ context.Context, call core.ToolCall) (core.Decision, error) {
        if call.Name == "refund" {
            return core.Decision{Confirm: true, Reason: "退款不可撤销"}, nil
        }
        return core.Decision{}, nil
    },
}
```

**存到数据库。** 用 GORM 适配器，数据库驱动由你选，pi-go 不绑定任何驱动。用 SQLite 时
要限制为单连接并开启 WAL，否则并发写入会报 `database is locked`。

```go
import gormstore "github.com/lee00jx/pi-go/adapter/gorm"

s := gormstore.New(db)
if err := s.Migrate(ctx); err != nil { ... }
gw := gateway.New(s, runner)
```

**加工模型输出。** 如果模型输出的是自定义格式（标签、表格、图表），实现
`output.Processor` 把它切成块，再用 `output.Stage` 校验或修复，通过
`gateway.WithEventMiddleware` 接进 gateway。pi-go 本身不内置任何格式。

```go
gateway.WithEventMiddleware(func(ctx context.Context, in <-chan core.AgentEvent) <-chan core.AgentEvent {
    return output.Transform(ctx, in,
        output.WithProcessor(NewMyProcessor),
        output.WithStages(MyValidateStage()),
    )
})
```

**其他选项。** `WithRateLimit`（每个用户的并发运行数和每分钟消息数）、`WithAuthorizer`
（按会话追加权限检查）、`gw.InjectTurn`（不调模型直接写入欢迎语或总结）、
`provider.WithExtraBody`（厂商特有的请求字段，比如关闭 Qwen 的思考）、
`core.Runner.Sampling`（温度等采样参数，不填则用模型服务的默认值）。详见代码注释。

## 不做什么

以下内容有意留给你的后端：登录鉴权与权限（JWT、Casbin）、SQL 工具、文件系统沙箱
（用 `BeforeToolCall` 拦截）、配置加载，以及任何命令行或界面。

## 目录结构

```
core/       对话循环、事件、工具接口、钩子、上下文压缩
provider/   OpenAI 兼容与 Anthropic 客户端、重试、测试用假模型
session/    存储接口 + 内存实现
tools/      工具注册表 + 内置 bash / 文件工具
output/     可选的输出加工管线
gateway/    HTTP 处理器（REST + SSE）
adapter/    GORM 存储
example/    可运行示例
```

核心包不依赖 gin、gorm、zap、viper。

## 开发

```
go test -race ./...
```

已知限制：运行锁和限流都只在单个进程内生效，暂不支持多实例部署；GORM 适配器目前只在
SQLite 上验证过。
