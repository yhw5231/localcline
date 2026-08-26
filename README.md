# cline2api-go

[Cline](https://cline.bot) API 反向代理的 **Go 移植版**：行为与原版 [Cloudflare Worker](https://github.com/6Kmfi6HP/cline2api)（`src/index.js`）保持一致。

核心目标：把 Cline 返回的 `delta.reasoning` 重写为 `delta.reasoning_content`，使下游（sub2api 等，只识别 `reasoning_content`）能把思考内容转成 Anthropic thinking block。

纯标准库实现，**零第三方依赖**，产出单个二进制，适合部署在自有 VPS / 容器。

## 工作原理

- **聊天**：原样转发请求到 OpenAI 兼容的 chat/completions 接口（默认 `https://api.cline.bot/api/v1/chat/completions`），`model` / `messages` 均不改动。
  - **流式**（SSE）：逐帧解析，把每个 chunk 的 `choices[].delta.reasoning` 复制到 `choices[].delta.reasoning_content`。
  - **非流式**：把 `choices[].message.reasoning` 复制到 `choices[].message.reasoning_content`。
- **模型列表**：`GET /v1/models`（`/models` 为别名）代理 Cline 的 `recommended-models` 端点，**只保留 `free` 数组**里的免费模型，转成 OpenAI 兼容格式（`{object, data:[{id, object, created, owned_by, name, description}]}`）。免费模型 ID 形如 `deepseek/deepseek-v4-flash`，可直接作为 chat/completions 的 `model` 使用。成功结果按 5 分钟内存 TTL 缓存，失败负缓存 30s，同上游地址并发去重。
- 鉴权：优先使用环境变量 `CLINE_API_KEY`，未设置则透传请求自带的 `Authorization`（模型列表端点无需鉴权）。

## 运行

```bash
cd go
UPSTREAM_URL=https://api.cline.bot/api/v1/chat/completions \
MODELS_UPSTREAM=https://api.cline.bot/api/v1/ai/cline/recommended-models \
CLINE_API_KEY=sk-xxx PORT=8080 \
  go run .
```

## 配置项

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `UPSTREAM_URL` | `https://api.cline.bot/api/v1/chat/completions` | 上游 OpenAI 兼容地址 |
| `MODELS_UPSTREAM` | `https://api.cline.bot/api/v1/ai/cline/recommended-models` | 免费模型列表上游地址 |
| `CLINE_API_KEY` | 空 | Cline API Key；未设置时透传请求自带的 `Authorization` |
| `PORT` | `8080` | 监听端口 |

## 测试

```bash
go test ./...           # 单元 + 集成（httptest 模拟上游）
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