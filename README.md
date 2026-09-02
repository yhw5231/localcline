# UniGate · 通用 AI 网关（原 cline2api-go）

一个 **OpenAI 兼容的多渠道 API 网关**：集中管理多个上游渠道的账号（key），每个 key
可绑定独立代理（固定 HTTP/SOCKS5 代理，或对接 [ipv6-proxy-pool](../ipv6-proxy-pool)
动态 IPv6 租约），对下游签发通用 key，按「渠道顺序 → key 顺序」做**故障转移**转发，
内置请求日志、用量统计（SQLite）与 **WebUI**。

由 cline2api（Cline 反代）演进而来：保留了其登录鉴权、SSE `reasoning →
reasoning_content` 改写（现改为**渠道级开关**）、请求日志与用量库；移除了 Cline
专属的正向代理/端口池监听器，重构为通用网关。参考了 newapi / sub2api 的核心思路，
只保留基础必要功能。

纯 Go 标准库 + SQLite（modernc.org/sqlite，无 CGO），单二进制，WebUI 嵌入二进制。

## 功能总览

- **多渠道**：每个渠道一个 OpenAI 兼容上游（`BaseURL` + 多个账号 key）。
  渠道可配置自定义请求头（模拟特定客户端指纹）、静态模型列表、模型列表端点。
- **每 key 独立代理**：
  - `直连`
  - `固定代理`：`http(s)://user:pass@host:port` 或 `socks5://...`
  - `IPv6 代理池`：对接 ipv6-proxy-pool（详见下文），支持自动申请/绑定/释放/换 IP
- **下游通用 key**：`sk-gw-...`，客户端用它调用本网关；启用/停用即生效。
- **故障转移**：单请求内自动换下一个 key/渠道；按 (key, model) 冷却跳过故障 key。
- **reasoning 改写**（渠道可选）：把上游 `reasoning` 复制为 `reasoning_content`，
  流式/非流式都支持（Cline 渠道需要，供 sub2api 等下游识别 thinking）。
- **请求日志**：最近 N 条请求（环形缓冲），含渠道/key/模型/状态/耗时/错误。
- **用量统计**：SQLite 逐条记录输入/输出 token，支持 今日/24h/7d/30d/全部 窗口，
  按下游 key、渠道、模型、上游 key 聚合。
- **WebUI**：渠道与 key 管理、通用密钥签发、日志、用量、代理池连通性测试、
  手动换 IP / 释放租约、单 key 连通性测试。

## 快速开始

```bash
go run .
# 浏览器打开 http://localhost:8080 ，默认 admin/admin（务必修改）
```

Docker：

```bash
docker compose up -d --build
```

### WebUI 里配一个渠道的最小流程

1. 「渠道」→ 新建：填名称、`BaseURL`（如 `https://api.cline.bot/api/v1`），
   Cline 渠道勾选「reasoning→reasoning_content 改写」。
2. 渠道内「+ 添加 Key」填上游 key；需要代理的 key 选择代理类型：
   - 固定代理：填 URL；
   - IPv6 代理池：填池管理端地址（如 `http://1.2.3.4:8080`）与 token（可空），
     租约 ID 留空自动按 `gw-<keyID>` 申请。可用「测试」按钮验证连通性。
3. 「通用密钥」→ 生成密钥，复制给下游。
4. 下游以 OpenAI 兼容方式调用：

```bash
curl http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer sk-gw-xxxx" \
  -H 'Content-Type: application/json' \
  -d '{"model":"deepseek/deepseek-v4-flash","messages":[{"role":"user","content":"hi"}]}'
```

`GET /v1/models` 聚合所有启用渠道的模型列表（静态列表 ∪ models 端点拉取，去重）。

## IPv6 代理池集成（ipv6-proxy-pool）

每个绑定代理池的 key 对应池子里一个**租约**（lease = 一个出口 IPv6 + SOCKS5 端口）。

**跨渠道复用**：key 代理配置开启 `share` 后，网关在本地维护一个共享代理池并自动做
「一号一 IP」分配（分配表持久化于 `data/lease-assignments.json`，重启不变、IP 稳定）：

- **同一渠道**（生效 BaseURL 相同，即上游同一站点）的不同 key **保证使用不同 IP**——
  同站多账号共用 IP 容易触发风控；
- **不同渠道**的 key **可以共用同一个 IP**——对每个上游来说依然是一个账号一个 IP，
  例如 A1,A2（渠道 A）与 B1,B2（渠道 B）只需 2 个 IP：A1+B1 一个、A2+B2 一个；
- 分配采用最优装填：新 key 优先复用「已承载分组数最多且不含本分组」的租约，
  池容量不够时才新申请；WebUI「代理池」页可见每个租约占用的分组；
- 共享租约换 IP 时所有使用方一起切换；key 删除/解绑后其租约在最后一个使用方
  消失时才释放（`Reconcile` 随渠道配置变更自动执行）；
- 显式配置 `lease_id` 时跳过自动分配（手动绑定，约束由用户自担）。

| 动作 | 行为 |
|---|---|
| 申请/绑定 | 该 key 首次使用时调 `POST /v1/leases`（幂等，同 ID 复用现有租约）；网关重启无需恢复 |
| 使用 | 请求经池子的 SOCKS5 出口转发（自动探测 `per_ipv6` / `multiplex` 模式） |
| 换 IP | `POST /v1/leases/{id}/rotate`。自动触发：网络失败、上游返回指定状态码（如 403/429）、按时间间隔、按请求次数 |
| 释放 | `DELETE /v1/leases/{id}`。手动（WebUI/Admin API）或删除/改绑 key 时自动释放不再使用的租约 |

- `per_ipv6` 模式：每个租约独立 SOCKS5 端口，SOCKS 地址默认取池管理端同机（可用
  `socks_host` 覆盖）。
- `multiplex` 模式：共用池基础端口，网关自动以 `user:<租约ID>` 作为 SOCKS5 用户名。

## 故障转移与冷却策略

候选顺序 = 渠道在配置中的顺序（即优先级）→ 渠道内 key 顺序。单请求最多尝试
`MAX_ROUTE_TRIES` 个候选（默认全部）。命中冷却的候选直接跳过：

| 故障 | 冷却时长（环境变量） | 默认 |
|---|---|---|
| 上游 429 | `Retry-After` 优先，否则 `RATE_LIMIT_COOLDOWN` | 1h |
| 上游 401/403（鉴权失败） | `AUTH_FAIL_COOLDOWN` | 10m |
| 上游 5xx | `SERVER_ERR_COOLDOWN` | 30s |
| 网络/代理错误 | `NET_ERR_COOLDOWN` | 15s |
| 全部候选失败 | 有 429 记录则返回 429 + `Retry-After`，否则 502 | |

冷却按 `(key, model)` 独立记录——同一账号不同模型的额度互不影响。

## 配置项（环境变量）

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `PORT` | `8080` | 监听端口 |
| `DATA_DIR` | `data`（容器内 `/data`） | 数据目录，**容器部署必须挂载** |
| `GATEWAY_CONFIG_PATH` | `${DATA_DIR}/gateway.json` | 渠道/密钥配置文件（WebUI 管理） |
| `LEASE_ASSIGN_PATH` | `${DATA_DIR}/lease-assignments.json` | 代理池「key→租约」分配表（跨渠道复用，持久化保证 IP 稳定） |
| `ADMIN_USERNAME` / `ADMIN_PASSWORD` | `admin` / `admin` | WebUI 管理员（也持久化于 accounts.json） |
| `EXTRA_USERS` | 空 | 额外 WebUI 用户，`user:pass,user2:pass2` |
| `TOKEN_TTL` | `24h` | 登录 token 有效期 |
| `TOKEN_SECRET` | 空 | 登录 token 签名密钥（缺省自动持久化到 data/token-secret） |
| `GW_KEY_AUTH` | `true` | 下游是否必须携带通用 key |
| `MAX_ROUTE_TRIES` | 0（全部） | 单请求最多尝试的 key 数 |
| `RATE_LIMIT_COOLDOWN` | `1h` | 429 冷却（无 `Retry-After` 时；有则优先） |
| `AUTH_FAIL_COOLDOWN` | `10m` | 401/403 冷却 |
| `SERVER_ERR_COOLDOWN` | `30s` | 5xx 冷却 |
| `NET_ERR_COOLDOWN` | `15s` | 网络错误冷却 |
| `UPSTREAM_HEADER_TIMEOUT` | `10m` | 等待上游响应头超时（LLM 非流式可能较慢，勿设过小） |
| `REQ_LOG_SIZE` | `1000` | 请求日志环形缓冲容量 |
| `USAGE_DB_PATH` | `${DATA_DIR}/usage.db` | 用量 SQLite 路径（空 = 纯内存） |
| `USAGE_RETENTION_DAYS` | `30` | 用量保留天数 |
| `USAGE_MAX_RECORDS` | `100000` | 用量最大条数 |

## Admin API（需管理员登录 token）

`POST /login` 换 token 后调用（`Authorization: Bearer <token>`）：

| 端点 | 说明 |
| --- | --- |
| `GET /admin/api/state` | 渠道、下游 key、代理池租约缓存总览 |
| `PUT /admin/api/channels` | 新增/整体更新渠道（含内嵌 keys） |
| `DELETE /admin/api/channels/{id}` | 删除渠道（自动释放其池租约） |
| `PUT /admin/api/gwkeys` | 新增/更新下游 key（key 留空自动生成） |
| `DELETE /admin/api/gwkeys/{id}` | 删除下游 key |
| `POST /admin/api/pool/test` | 测试池子连通性 `{pool_url, pool_token}` |
| `POST /admin/api/pool/rotate` | 手动换 IP `{channel_id, key_id}` 或 `{pool_url, lease_id}`（直连本地代理池条目） |
| `POST /admin/api/pool/release` | 手动释放租约 `{channel_id, key_id}` 或 `{pool_url, lease_id}` |
| `GET /admin/api/pool/leases` | 网关持有的租约列表 |
| `POST /admin/api/testkey` | 用指定 key 发一条测试请求 `{channel_id, key_id, model?}` |
| `GET /admin/api/requests?limit=` | 最近请求日志 |
| `GET /admin/api/usage?window=today\|24h\|7d\|30d\|all` | 用量统计（支持 `?user=&channel=&model=&key=`） |

## 渠道自定义请求头示例（Cline 渠道）

渠道级 headers（JSON）可模拟任意客户端指纹；Cline 渠道建议：

```json
{
  "http-referer": "https://cline.bot",
  "x-title": "Cline",
  "User-Agent": "Cline/4.1.16",
  "x-core-version": "4.1.16",
  "x-platform-version": "1.106.0",
  "x-client-version": "4.1.16",
  "x-platform": "vscode",
  "x-client-type": "cline-vscode"
}
```

## 测试

```bash
go test ./...
```

覆盖：配置存储、池客户端（申请幂等/换IP/释放/两种 SOCKS 模式/跨渠道复用分配约束
[同组互斥、跨组共享、最优装填]、分配持久化与 Reconcile 回收）、路由故障转移
（429/401/网络错误/冷却/模型过滤）、ipv6pool 端到端（真实 SOCKS5 stub 隧道 +
状态码触发换 IP）、下游鉴权、模型聚合、Admin API、用量库。

## 与原 cline2api 的差异（破坏性变更）

- 移除：`PROXY_PORT`/`SOCKS_PORT` 正向代理监听器、`PROXY_POOL_*` 端口池监听器、
  `UPSTREAM_URL`/`UPSTREAM_KEYS`/`KEY_SELECT_MODE`/`CLIENT_*` 全局环境变量配置
  （对应能力全部移入 WebUI 的渠道/代理配置）。
- `POST /`（根路径聊天转发）不再提供，统一走 `/v1/chat/completions`。
- 下游鉴权从登录 token 改为通用 key（`GW_KEY_AUTH=false` 可关闭校验）。
- reasoning 改写从全局行为改为渠道级开关（`rewrite_reasoning`）。
- 用量库增加 `channel` 维度（旧库自动迁移，兼容升级）。
