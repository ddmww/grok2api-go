# grok2api-go

`grok2api-go` 是 `grok2api` 第一阶段 Go 重构版：保留管理后台和 OpenAI 兼容核心接口，移除 Python worker 架构与玩法页依赖，默认单进程多 goroutine 运行。

## 当前范围

- 已实现：`/health`、`/v1/models`、`/v1/chat/completions`、`/v1/responses`
- 已实现：`/admin` 静态后台与 `/admin/api/config`、`/status`、`/sync`、`/tokens`、`/batch`、`/cache`
- 已实现：`local + mysql` 存储、MySQL 首启从本地 SQLite 迁移、Docker、多架构 GHCR 工作流
- 暂不实现：`/v1/images/*`、`/v1/videos*`、`/v1/messages`、`/webui/*`

## 本地运行

1. 复制 `.env.example` 为 `.env`
2. 如需代理出口，在 `data/config.toml` 或后台配置中设置：

```toml
[proxy.egress]
mode = "single_proxy"
proxy_url = "http://127.0.0.1:7897"
```

3. 启动：

```bash
go mod tidy
go run ./cmd/server
```

默认地址：

- 服务：`http://127.0.0.1:8000`
- 后台：`http://127.0.0.1:8000/admin/login`
- 默认后台密钥：`grok2api`

## Docker

本地存储：

```bash
docker compose up --build -d
```

MySQL 模式：

```bash
ACCOUNT_STORAGE=mysql \
ACCOUNT_MYSQL_URL='grok2api:grok2api@tcp(mysql:3306)/grok2api?charset=utf8mb4&parseTime=True&loc=Local' \
docker compose --profile mysql up --build -d
```

远端镜像：

```bash
docker pull ghcr.io/ddmww/grok2api-go:latest
```

## 迁移

- `ACCOUNT_STORAGE=mysql` 且目标库为空时，会自动尝试导入 `${ACCOUNT_LOCAL_PATH}` 指向的本地 SQLite
- 导入成功后，原始 SQLite 会改名为 `*.migrated`
- 配置文件会在 MySQL 配置表为空时，从本地 `config.toml` 自动导入

## CI / CD

- `ci.yml`：`go test`、`go build`、`docker build`
- `docker-publish.yml`：`main` 分支自动更新 `ghcr.io/ddmww/grok2api-go:latest`，版本 tag 同时发布对应版本号镜像与多架构清单
