# API 通知系统验收记录

## 环境

验收于 2026-09-14 在 macOS arm64 本地执行。Go 为 `go1.26.0 darwin/arm64`，Docker 为 24.0.6，Compose 为 2.23.0。项目独立容器实际报告 MySQL `8.4.7`、默认存储引擎 `InnoDB`、会话时区 `+00:00`。

所有外呼均指向 `httptest` 或仓库内 `cmd/mockvendor`，没有使用真实供应商凭证。测试数据中的密钥仅用于本机验证。

## 自动检查

以下检查实际通过：

```sh
test -z "$(gofmt -l .)"
go build ./...
go vet ./...
go test ./...
NOTIFIER_TEST_MYSQL_DSN='notifier:notifier@tcp(127.0.0.1:3307)/notifier?parseTime=true&charset=utf8mb4&loc=UTC' go test ./...
NOTIFIER_TEST_MYSQL_DSN='notifier:notifier@tcp(127.0.0.1:3307)/notifier?parseTime=true&charset=utf8mb4&loc=UTC' go test -race ./internal/api ./internal/delivery ./internal/retry ./internal/store/mysqlstore ./internal/integration
```

单元测试覆盖有效 `2xx` 判定、`200` 业务失败正文、`202`、`400`、`429`、`501`、`503`、超时、Retry-After、次数与有效期、TLS 信任与拒绝、重定向、外层大小限制和鉴权。真实 MySQL 用例覆盖迁移重复执行、并发同键去重、内容冲突、竞争领取、过期租约回收、新领取后旧令牌写回拒绝，以及 HTTP 投递、状态/历史查询和失败重放的端到端生命周期。

接入数据库失败通过 API 测试确认返回 `503`，不返回接收成功。非法目标返回 `403`；查询响应和尝试摘要不包含 URL、正文或凭证。模拟目标收到配置注入的凭证和原始 Method、Header、Body，服务日志仅记录任务、目标、尝试、状态和安全错误分类。

## 重启演示

本地运行使用 `.env.example` 的参数，首次把扫描间隔临时设为 60 秒。任务 `e3c875df-7df0-4265-a652-f282a60fa7f3` 得到 `202` 后处于 `pending`、尝试数为 0；随后正常停止服务。数据库查询仍返回该任务。

使用默认扫描间隔重启同一服务后，任务被领取并投递到本地模拟供应商。查询结果为 `succeeded`、HTTP `200`、尝试数 1；尝试记录为 `http_success` 和 `http_response`。供应商实际观察到 `POST /success` 和业务幂等 Header；自动用例另行比对了 Header 的稳定值。该响应正文表达业务失败，任务仍按 HTTP 状态成功，符合投递确认契约。

## 阶段提交

| 提交 | 内容 | 阶段检查 |
|---|---|---|
| `638c4a0 feat(core): 建立配置与 MySQL 持久化基础` | Go 依赖、配置校验、MySQL 8.4.7 Compose、核心表与 migration | 配置单测、构建、vet、真实 MySQL migration 连续执行通过 |
| `316379e feat(api): 实现幂等接收查询与安全 HTTP 投递` | 接收、列表、状态、尝试查询、目标校验与 HTTP(S) 客户端 | API、状态判定、重定向和 TLS 单测通过 |
| `8e834c7 feat(worker): 实现租约调度恢复与受控重放` | 有界 Worker、退避、租约、条件写回、恢复、重放、指标和清理 | 真实 MySQL 竞争/恢复及端到端生命周期通过 |

交付文档、模拟服务和最终检查在后续交付提交中保存。

## 未覆盖项

本轮没有执行长时间压测、多副本全局限流、数据库网络分区或提交结果不明的系统性故障注入、数据库灾难恢复、全部 DNS 与证书故障矩阵，以及外部告警渠道联调。这些是实施文档列出的按需检查，不改变已经验证的单进程 MVP 行为。

指标来自当前保留数据，终态清理后不构成永久审计或长期指标存储。目标并发限制是单进程限制；供应商已执行但响应丢失时仍可能重复投递。系统不承诺恰好一次、供应商业务结果成功或永久故障下最终必达。
