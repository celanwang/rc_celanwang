# API 通知系统运行说明

## 运行环境

服务使用 Go 1.26.0 和 MySQL 8.4.7。仓库中的 Compose 配置只启动项目独立的 MySQL 实例，容器名带 `rc-celanwang` 前缀，数据保存在独立卷中，不会操作其他数据库实例。

复制配置示例后再按环境修改密钥、目标和限制。示例允许访问本机 `18081` 端口，仅用于本地模拟；生产目标默认不应设置 `allow_private`。`NOTIFIER_API_KEYS` 的每一项是 `调用方:密钥:user|admin`，同一调用方可以分别配置提交密钥和管理密钥。目标中的 `callers` 决定哪些调用方可以使用该目标。

```sh
cp .env.example .env
docker compose up -d mysql
set -a
source .env
set +a
go run ./cmd/notifier -migrate-only
```

迁移可重复执行。服务启动时也会先执行尚未应用的 migration，因此单独运行迁移主要用于部署前检查。MySQL DSN 必须启用 `parseTime=true` 并使用 UTC；示例已经包含这些设置。

仓库根目录的 Makefile 为这些命令提供薄封装。它不替代本说明中的原始命令；需要快速建立本地环境时，可以运行：

```sh
cp .env.example .env
make mysql-up
make migrate
```

## 本地启动

先在一个终端启动模拟供应商。它提供成功、异步接受、暂时失败、超时和永久失败路径，并只记录 Method、Path 和业务幂等标识，不记录 Authorization 或正文。

```sh
go run ./cmd/mockvendor
```

在另一个已经载入 `.env` 的终端启动通知服务。

```sh
set -a
source .env
set +a
go run ./cmd/notifier
```

存活检查不访问数据库，就绪检查会验证数据库连接。指标接口需要同调用方的管理员密钥。

```sh
curl -sS http://127.0.0.1:8080/health/live
curl -sS http://127.0.0.1:8080/health/ready
curl -sS http://127.0.0.1:8080/metrics \
  -H 'Authorization: Bearer admin-secret'
```

## 提交与查询

调用方每次业务通知生成一个稳定的接收幂等键。相同调用方、相同幂等键和相同内容返回原任务；内容不同返回 `409`。供应商需要的幂等标识应明确放在业务 Header 中，服务不会把外层幂等键自动转发。

```sh
curl -sS -X POST http://127.0.0.1:8080/v1/notifications \
  -H 'Authorization: Bearer dev-secret' \
  -H 'Idempotency-Key: registration-42' \
  -H 'Content-Type: application/json' \
  --data '{
    "target_id":"local-demo",
    "url":"http://127.0.0.1:18081/success",
    "method":"POST",
    "headers":{
      "Content-Type":["application/json"],
      "X-Supplier-Idempotency":["registration-42"]
    },
    "body":"{\"event\":\"registered\"}"
  }'
```

从响应取出任务 ID 后，可以读取状态、尝试历史和列表。

```sh
curl -sS http://127.0.0.1:8080/v1/notifications/NOTIFICATION_ID \
  -H 'Authorization: Bearer dev-secret'
curl -sS 'http://127.0.0.1:8080/v1/notifications/NOTIFICATION_ID/attempts?limit=20' \
  -H 'Authorization: Bearer dev-secret'
curl -sS 'http://127.0.0.1:8080/v1/notifications?status=failed&target_id=local-demo' \
  -H 'Authorization: Bearer dev-secret'
```

模拟供应商的 `/success` 返回 `200` 及业务失败正文，`/accepted` 返回 `202`；两者都会成为 `succeeded`。`/retry` 返回带 `Retry-After: 2` 的 `503`，`/slow` 用于检查总超时，其他路径返回永久 `400`。

## 失败重放

只有 `failed` 任务可以重放。管理密钥所属调用方必须与任务所属调用方一致，请求同时提交当前 `run_no`、原因和新的绝对有效期。条件更新使重复或陈旧操作返回 `409`，重放不会修改原请求或供应商幂等标识。

```sh
curl -sS -X POST http://127.0.0.1:8080/v1/notifications/NOTIFICATION_ID/replays \
  -H 'Authorization: Bearer admin-secret' \
  -H 'Content-Type: application/json' \
  --data '{
    "expected_run":1,
    "reason":"供应商配置已经修复",
    "expires_at":"2026-09-15T12:00:00Z"
  }'
```

## 目标与凭证

`NOTIFIER_TARGETS_JSON` 是目标列表。每个目标固定允许的 Host、端口、调用方和单目标并发。DNS 在实际连接前重新解析；除非目标显式设置 `allow_private`，回环、私网、链路本地、未指定和组播地址都会被拒绝。HTTP 客户端不使用环境代理、不跟随重定向，HTTPS 最低 TLS 1.2 并正常验证证书和域名。

供应商静态凭证可以放在目标的 `credential_headers` 中，发送时注入。提交接口拒绝调用方提供 Authorization、Host、Content-Length、Connection、Transfer-Encoding 等受控 Header。私有 CA 使用目标的 `ca_file` 指向 PEM 文件，不支持关闭证书校验。

普通日志不输出目标 URL、查询参数、请求或响应正文、接入密钥和供应商凭证。响应诊断只保存读取字节数、截断及读取错误标记。

## 验证与停止

不设置测试 DSN 时，真实数据库用例会明确跳过。完整的本地检查使用项目容器：

```sh
test -z "$(gofmt -l .)"
go build ./...
go vet ./...
go test ./...
docker compose exec -T mysql env MYSQL_PWD=notifier-root mysql -uroot \
  -e "CREATE DATABASE IF NOT EXISTS notifier_test CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci; GRANT ALL PRIVILEGES ON notifier_test.* TO 'notifier'@'%';"
NOTIFIER_TEST_MYSQL_DSN='notifier:notifier@tcp(127.0.0.1:3307)/notifier_test?parseTime=true&charset=utf8mb4&loc=UTC' \
  go test ./internal/store/mysqlstore ./internal/integration -v
```

也可以运行 `make check` 执行格式、构建、静态检查和普通测试，或运行 `make check-all` 再使用独立的 `notifier_test` 数据库执行真实 MySQL 用例。后者会清理该测试数据库中的通知记录，不会操作示例配置使用的 `notifier` 开发数据库。

服务收到 `SIGINT` 或 `SIGTERM` 后停止领取，关闭接入并在配置的期限内等待在途请求。普通停止保留 MySQL 数据卷：

```sh
docker compose down
```
