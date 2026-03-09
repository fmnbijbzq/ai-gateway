# AI Gateway - AI API 网关

一个高性能的 AI API 反向代理网关，支持在多个 OpenAI 兼容的 API 端点之间进行智能负载均衡。

## 功能特性

- **加权轮询负载均衡** - 根据配置的权重在多个上游之间分配请求，支持最少连接数回退策略
- **模型感知路由** - 根据请求中的模型名称，自动选择支持该模型的上游
- **健康检查** - 定期探测上游健康状态，自动摘除/恢复不健康的上游
- **熔断器** - 每个上游独立的熔断器（关闭 → 打开 → 半开），防止故障扩散
- **SSE 流式传输** - 完整支持 Server-Sent Events 流式响应透传
- **失败重试** - 请求失败时自动重试下一个健康的上游
- **管理接口** - 查看上游状态、手动启用/禁用上游
- **结构化日志** - 使用 Go 标准库 `slog` 输出 JSON 格式日志
- **优雅关闭** - 支持 SIGINT/SIGTERM 信号优雅关闭

## 项目结构

```
ai-gateway/
├── cmd/gateway/main.go          # 入口文件
├── internal/
│   ├── config/config.go         # 配置加载与校验
│   ├── upstream/upstream.go     # 上游节点管理、熔断器
│   ├── balancer/balancer.go     # 负载均衡器
│   ├── proxy/proxy.go           # 请求代理转发
│   ├── health/checker.go        # 健康检查
│   └── admin/admin.go           # 管理接口
├── config.example.yaml          # 示例配置
├── go.mod
└── README.md
```

## 快速开始

### 编译

```bash
go build -o ai-gateway ./cmd/gateway/
```

### 配置

复制并编辑配置文件：

```bash
cp config.example.yaml config.yaml
```

配置文件说明：

```yaml
server:
  listen: ":8080"          # 监听地址
  read_timeout: 30s        # 读取超时
  write_timeout: 120s      # 写入超时（流式请求需要较长时间）
  max_retries: 3           # 最大重试次数

health_check:
  interval: 10s            # 健康检查间隔
  timeout: 5s              # 健康检查超时
  failure_threshold: 3     # 连续失败多少次标记为不健康
  recovery_cooldown: 30s   # 恢复冷却时间

circuit_breaker:
  failure_threshold: 5     # 熔断器触发阈值
  recovery_timeout: 30s    # 熔断器恢复超时

upstreams:
  - name: "openai-primary"        # 上游名称（唯一）
    base_url: "https://api.openai.com"  # 上游地址
    api_key: "sk-xxxx"            # API 密钥
    weight: 10                    # 权重（越大分配请求越多）
    models:                       # 该上游支持的模型列表
      - "gpt-4o"
      - "gpt-4o-mini"
```

### 运行

```bash
./ai-gateway -config config.yaml
```

## API 端点

### 代理端点

所有 `/v1/` 路径的请求会被代理到上游：

```bash
# 聊天补全
curl http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gpt-4o",
    "messages": [{"role": "user", "content": "你好"}]
  }'

# 流式请求
curl http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gpt-4o",
    "messages": [{"role": "user", "content": "你好"}],
    "stream": true
  }'

# 获取模型列表
curl http://localhost:8080/v1/models
```

网关会自动替换 `Authorization` 头为上游配置的 `api_key`，客户端可以使用任意或空的 API Key。

### 健康检查

```bash
curl http://localhost:8080/health
```

返回示例：

```json
{
  "status": "ok",
  "upstreams": [
    {
      "name": "openai-primary",
      "base_url": "https://api.openai.com",
      "healthy": true,
      "enabled": true,
      "active_connections": 2,
      "consecutive_failures": 0,
      "last_latency": "145ms",
      "total_requests": 1234,
      "total_failures": 3,
      "circuit_state": "closed",
      "models": ["gpt-4o", "gpt-4o-mini"]
    }
  ]
}
```

### 管理接口

```bash
# 查看所有上游状态
curl http://localhost:8080/admin/status

# 禁用某个上游
curl -X POST http://localhost:8080/admin/upstream/openai-primary/disable

# 启用某个上游
curl -X POST http://localhost:8080/admin/upstream/openai-primary/enable
```

## 负载均衡策略

1. **加权轮询**（主策略）：根据上游配置的 `weight` 值按比例分配请求
2. **最少连接数**（回退策略）：当加权轮询无法选择时，选择活跃连接数最少的上游
3. **模型过滤**：只有在 `models` 列表中包含请求模型的上游才会被选中
4. **健康过滤**：不健康或被禁用的上游自动跳过

## 熔断器

每个上游有独立的熔断器，状态转换：

```
关闭(closed) --连续失败达阈值--> 打开(open) --超时--> 半开(half-open) --成功--> 关闭(closed)
                                    ^                                      |
                                    +-------------- 失败 -----------------+
```

- **关闭**：正常转发请求
- **打开**：拒绝所有请求，等待恢复超时
- **半开**：允许一个探测请求通过，成功则关闭熔断器，失败则重新打开

## 部署

### 使用 systemd

创建 `/etc/systemd/system/ai-gateway.service`：

```ini
[Unit]
Description=AI API Gateway
After=network.target

[Service]
Type=simple
User=www-data
WorkingDirectory=/opt/ai-gateway
ExecStart=/opt/ai-gateway/ai-gateway -config /opt/ai-gateway/config.yaml
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl enable ai-gateway
sudo systemctl start ai-gateway
```

### 使用 Docker

```dockerfile
FROM golang:1.23-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o ai-gateway ./cmd/gateway/

FROM alpine:3.19
RUN apk add --no-cache ca-certificates
COPY --from=builder /app/ai-gateway /usr/local/bin/
COPY config.example.yaml /etc/ai-gateway/config.yaml
EXPOSE 8080
ENTRYPOINT ["ai-gateway", "-config", "/etc/ai-gateway/config.yaml"]
```

```bash
docker build -t ai-gateway .
docker run -d -p 8080:8080 -v $(pwd)/config.yaml:/etc/ai-gateway/config.yaml ai-gateway
```

### 配合 Nginx 使用

```nginx
upstream ai_gateway {
    server 127.0.0.1:8080;
}

server {
    listen 443 ssl;
    server_name api.example.com;

    location / {
        proxy_pass http://ai_gateway;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_buffering off;           # 流式传输必需
        proxy_cache off;
        proxy_read_timeout 300s;       # 长请求超时
    }
}
```

## 依赖

- Go 1.21+
- `gopkg.in/yaml.v3` — YAML 配置解析

无其他外部依赖，全部使用 Go 标准库实现。
