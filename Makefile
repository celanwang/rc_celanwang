SHELL := /bin/sh

GO ?= go
COMPOSE ?= docker compose
ENV_FILE ?= .env
TEST_MYSQL_DSN ?= notifier:notifier@tcp(127.0.0.1:3307)/notifier_test?parseTime=true&charset=utf8mb4&loc=UTC

.DEFAULT_GOAL := help

.PHONY: help mysql-up mysql-test-db mysql-down migrate run mock-vendor fmt-check build vet test test-integration check check-all

help:
	@printf '%s\n' \
		'mysql-up         启动并等待项目 MySQL 就绪' \
		'mysql-down       停止项目容器并保留数据卷' \
		'migrate          从 ENV_FILE 加载配置并执行迁移' \
		'run              从 ENV_FILE 加载配置并启动服务' \
		'mock-vendor      启动本地模拟供应商' \
		'fmt-check        检查 Go 文件格式' \
		'build            构建全部 Go 包' \
		'vet              执行 Go 静态检查' \
		'test             执行测试，未配置测试 DSN 时数据库用例跳过' \
		'test-integration 在独立 notifier_test 数据库执行真实 MySQL 用例' \
		'check            执行格式、构建、静态检查和普通测试' \
		'check-all        执行 check 和真实 MySQL 集成测试'

mysql-up:
	$(COMPOSE) up -d --wait mysql

mysql-test-db: mysql-up
	$(COMPOSE) exec -T mysql env MYSQL_PWD=notifier-root mysql -uroot -e "CREATE DATABASE IF NOT EXISTS notifier_test CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci; GRANT ALL PRIVILEGES ON notifier_test.* TO 'notifier'@'%';"

mysql-down:
	$(COMPOSE) down

migrate:
	@test -f "$(ENV_FILE)" || { printf '缺少 %s，请先复制并修改 .env.example\n' "$(ENV_FILE)" >&2; exit 1; }
	@set -a; . "./$(ENV_FILE)"; set +a; exec $(GO) run ./cmd/notifier -migrate-only

run:
	@test -f "$(ENV_FILE)" || { printf '缺少 %s，请先复制并修改 .env.example\n' "$(ENV_FILE)" >&2; exit 1; }
	@set -a; . "./$(ENV_FILE)"; set +a; exec $(GO) run ./cmd/notifier

mock-vendor:
	$(GO) run ./cmd/mockvendor

fmt-check:
	@files="$$(gofmt -l .)"; test -z "$$files" || { printf '以下文件需要 gofmt：\n%s\n' "$$files" >&2; exit 1; }

build:
	$(GO) build ./...

vet:
	$(GO) vet ./...

test:
	$(GO) test ./...

test-integration: mysql-test-db
	NOTIFIER_TEST_MYSQL_DSN='$(TEST_MYSQL_DSN)' $(GO) test ./internal/store/mysqlstore ./internal/integration -v

check: fmt-check build vet test

check-all: check test-integration
