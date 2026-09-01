# cline2api-go

[Cline](https://cline.bot) API 反向代理的 **Go 移植版**：行为与原版 [Cloudflare Worker](https://github.com/6Kmfi6HP/cline2api)（`src/index.js`）保持一致，并在此基础上扩展了**登录验证、多上游 key、按模型限流冷却、HTTP/SOCKS 代理与代理池**。

核心目标：把 Cline 返回的 `delta.reasoning` 重写为 `delta.reasoning_content`，使下游（sub2api 等，只识别 `reasoning_content`）能把思考内容转成 Anthropic thinking block。

纯标准库实现，**零第三方依赖**，产出单个二进制，适合部署在自有 VPS / 容器。

## 工作原理

- **聊天**：原样转发请求到 OpenAI 兼容的 chat/completions 接口（默认 `https://api.cline.bot/api/v1/chat/completions`），`model` / `messages` 均不改动。
  - **流式**（SSE）：逐帧解析，把每个 chunk 的 `choices[].delta.reasoning` 复制到 `choices[].delta.reasoning_content`。
  - **非流式**：把 `choices[].message.reasoning` 复制到 `choices[].message.reasoning_content`。
- **模型列表**：`GET /v1/models`（`/models` 为别名）代理 Cline 的 `recommended-models` 端点，**只保留 `free` 数组**里的免费模型，转成 OpenAI 兼容格式（`{object, data:[{id, object, created, owned_by, name, description}]}`）。免费模型 ID 形如 `deepseek/deepseek-v4-flash`，可直接作为 chat/completions 的 `model` 使用。成功结果按 5 分钟内存 TTL 缓存，失败负缓存 30s，同上游地址并发去重。
- **登录验证**：`POST /login` 用用户名/密码换取 token（HMAC 签名、带过期时间）。除 `/login` 外的所有端点默认要求 `Authorization: Bearer <token>`。默认管理员 `admin/admin`。
- **多上游 key**：`UPSTREAM_KEYS` 配置多个 key，按 `顺序优先`（sequential：始终取第一个未冷却的）或 `轮询优先`（roundrobin：在各 key 间轮转）选择；上游请求的 `Authorization` 由服务端注入，不再透传客户端登录 token。
- **按模型限流冷却**：冷却状态按 `(key, model)` 单独记录——同一个账号（key）下不同模型额度独立，因此冷却也独立。上游返回 429 时记录冷却；命中冷却的 key 会被跳过，全部冷却则返回 429 + `Retry-After`。
- **代理支持**：
  - **普通代理**：HTTP 正向代理（支持 CONNECT 隧道与 absolute-form 请求，`PROXY_PORT`）与 SOCKS5 代理（`SOCKS_PORT`）。目标 host 等于上游 host（或 `PROXY_MANAGED_ALL=true`）时走 managed 路径：注入 key + 冷却 + 改写；其他 host 做普通正向转发。
  - **代理池**：`PROXY_POOL_ENTRIES` 定义若干代理（每条 `user:pass[:key][@backend]`）。
    - **单端口模式**（`PROXY_POOL_PORT`）：所有池代理共用一个端口，凭 `Proxy-Authorization` Basic 的用户名/密码区分，映射到各自的 key / 后端。
    - **端口范围模式**（`PROXY_POOL_RANGE` 形如 `20000-20050`）：每个池代理自动在范围内检测空闲端口并绑定独立端口。

## 运行

```bash
UPSTREAM_URL=https://api.cline.bot/api/v1/chat/completions \
MODELS_UPSTREAM=https://api.cline.bot/api/v1/ai/cline/recommended-models \
UPSTREAM_KEYS=sk-a,sk-b,sk-c \
KEY_SELECT_MODE=sequential \
PORT=8080 \
PROXY_PORT=8888 \
SOCKS_PORT=1080 \
  go run .
```

## 配置项

### 基础

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `UPSTREAM_URL` | `https://api.cline.bot/api/v1/chat/completions` | 上游 OpenAI 兼容地址 |
| `MODELS_UPSTREAM` | `https://api.cline.bot/api/v1/ai/cline/recommended-models` | 免费模型列表上游地址 |
| `CLINE_API_KEY` | 空 | 单个 Cline API Key；`UPSTREAM_KEYS` 未设置时的单 key 回退 |
| `PORT` | `8080` | API 监听端口 |

### 登录验证

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `LOGIN_REQUIRED` | `true` | 是否要求登录（`false` 时保留旧透传行为） |
| `ADMIN_USERNAME` | `admin` | 管理员用户名 |
| `ADMIN_PASSWORD` | `admin` | 管理员密码 |
| `EXTRA_USERS` | 空 | 额外用户，逗号分隔 `user:pass` |
| `TOKEN_TTL` | `24h` | 登录 token 有效期（Go duration，如 `30m`） |
| `TOKEN_SECRET` | 随机（进程内） | token 签名密钥；建议生产固定 |

### 多上游 key 与冷却

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `UPSTREAM_KEYS` | 空 | 逗号分隔的多个上游 key；为空时回退 `CLINE_API_KEY` |
| `KEY_SELECT_MODE` | `sequential` | `sequential`（顺序优先）或 `roundrobin`（轮询优先） |
| `RATE_LIMIT_COOLDOWN` | `60s` | 429 冷却时长（Go duration）；上游 `Retry-After` 优先 |
| `MAX_KEY_TRIES` | 0（自动） | 单请求最多尝试的 key 数（429 后换 key 重试） |

### 普通代理

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `PROXY_PORT` | 空（禁用） | HTTP 正向代理端口 |
| `PROXY_USER` / `PROXY_PASS` | 空 | HTTP 代理的 `Proxy-Authorization` Basic 认证 |
| `SOCKS_PORT` | 空（禁用） | SOCKS5 代理端口 |
| `SOCKS_USER` / `SOCKS_PASS` | 空 | SOCKS5 用户名/密码认证（RFC 1929） |
| `PROXY_MANAGED_ALL` | `false` | 为 `true` 时所有 absolute-form 请求都按 managed 路径转发到上游 |

### 代理池

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `PROXY_POOL_PORT` | 0 | 单端口模式端口（>0 时启用，所有池代理共用） |
| `PROXY_POOL_RANGE` | 空 | 端口范围模式，形如 `20000-20050` |
| `PROXY_POOL_ENTRIES` | 空 | 池条目，逗号分隔，每条 `user:pass[:key][@backend]` |

- `key` 为空时，该池代理使用全局 key 选择（`UPSTREAM_KEYS` / `CLINE_API_KEY`）。
- `backend` 为链经的上游代理，如 `@http://1.2.3.4:3128` 或 `@socks5://u:p@1.2.3.4:1080`。
- 单端口模式下客户端通过 `Proxy-Authorization` Basic 凭证区分；端口范围模式下每个池代理独立端口。

### 可观察性

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `REQ_LOG_SIZE` | `1000` | 请求记录环形缓冲容量 |
| `USAGE_DB_PATH` | `data/usage.db` | 用量数据库文件路径（SQLite，`modernc.org/sqlite` 纯 Go 驱动，无 CGO）；留空则纯内存 |
| `USAGE_RETENTION_DAYS` | `30` | 用量事件保留天数（超出自动删除） |
| `USAGE_MAX_RECORDS` | `100000` | 数据库中保留的最大记录数（超出自动裁剪最旧） |

用量数据存放在 **`data/` 目录**下的 SQLite 文件（默认 `data/usage.db`），启动时自动创建该目录。容器部署时**必须挂载 `data` 目录**为持久卷，否则容器重建后统计丢失：

```yaml
# docker-compose 示例（仅示意 volume）
services:
  cline2api:
    image: cline2api
    environment:
      USAGE_DB_PATH: /data/usage.db   # 可改，但确保挂载目录一致
    volumes:
      - ./data:/data                  # 持久化挂载
```

也可以直接用命令挂载：

```bash
docker run -d --name cline2api \
  -p 8080:8080 \
  -v ./data:/data \
  -e USAGE_DB_PATH=/data/usage.db \
  cline2api
```

### Admin 端点（需管理员登录）

| 端点 | 说明 |
| --- | --- |
| `GET /admin/stats` | 实时用量统计（总量 + 按用户/模型/key） |
| `GET /admin/usage` | 窗口化用量统计（`?window=today\|24h\|7d\|30d\|all`，支持 `?user=&model=&key=` 过滤） |
| `GET /admin/requests` | 最近请求记录（`?limit=` 控制条数） |

用量库逐条记录每次请求的**输入/输出 token**（解析自上游响应 `usage.prompt_tokens` / `usage.completion_tokens`，流式从末尾 chunk 提取），按用户 / 模型 / 上游 key 记账，并支持条件过滤：

```bash
# 今日用量
curl -H "Authorization: Bearer $TOKEN" \
  "http://localhost:8080/admin/usage?window=today"
# 近 7 天某用户某模型
curl -H "Authorization: Bearer $TOKEN" \
  "http://localhost:8080/admin/usage?window=7d&user=alice&model=deepseek/deepseek-v4-flash"
```

返回示例：

```json
{
  "window": "today",
  "start": "2026-09-01T00:00:00+08:00",
  "end": "2026-09-01T15:00:00+08:00",
  "requests": 12,
  "errors": 1,
  "bytes_out": 40960,
  "prompt_tokens": 3200,
  "completion_tokens": 1800,
  "total_tokens": 5000,
  "by_user": [{"name": "admin", "requests": 12, "prompt_tokens": 3200, "completion_tokens": 1800}],
  "by_model": [{"name": "deepseek/deepseek-v4-flash", "requests": 12, "prompt_tokens": 3200}],
  "by_key": [{"name": "sk-s****1234", "requests": 12, "prompt_tokens": 3200}]
}
```

> 说明：`by_key` 中的 key 已脱敏展示（内部记账保留原始 key 用于过滤）；用量库为 SQLite（`data/usage.db`），容器部署请挂载 `data` 目录持久化（见上方示例）。

## 登录与调用示例

```bash
# 1. 登录获取 token
TOKEN=$(curl -s -X POST http://localhost:8080/login \
  -H 'Content-Type: application/json' \
  -d '{"username":"admin","password":"admin"}' | jq -r .token)

# 2. 携带 token 调用
curl http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"model":"deepseek/deepseek-v4-flash","messages":[{"role":"user","content":"hi"}],"stream":false}'

# 3. 通过普通 HTTP 代理（managed 路径）
curl -x http://localhost:8888 http://api.cline.bot/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"deepseek/deepseek-v4-flash","messages":[{"role":"user","content":"hi"}],"stream":false}'
```

## 测试

```bash
go test ./...           # 单元 + 集成（httptest 模拟上游，含 auth/keys/proxy/pool 用例）
```

与 JS 原版的语义对拍（`testdata/compat_cases.json` 驱动的 38 用例，JSON deep-equal、忽略字段序）。需要原版 `src/index.js`（本仓库不包含），用环境变量指向它：

```bash
CLINE2API_JS_INDEX=/path/to/cline2api/src/index.js \
  node testdata/compat_js_runner.cjs \
  && go test -run TestCompatWithNode ./...
```

缺少 `js_out.json` 或原版 `index.js` 时，`TestCompatWithNode` 会自动跳过，不影响常规 `go test`。

## 与 Worker 版的差异

保持行为等价的前提下，合理保留的偏差（非 bug）：

- 类型安全下的容错：Go 对 `choices` 数组逐元素解码、对 models `free` 容忍脏数据，非 JSON 透传时 Content-Type 显式 `text/plain`。
- map 键按字母序（非 JS 插入序）、非流式不改写返回上游原字节、>2^53 大数保留原字节。
- 登录开启时，客户端 `Authorization` 用于校验登录 token，不再作为上游 key 透传（上游 key 由 `UPSTREAM_KEYS` / `CLINE_API_KEY` 管理）。
