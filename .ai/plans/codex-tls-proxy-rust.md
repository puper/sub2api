# Codex TLS 指纹代理（Rust 独立服务）

## 目标

为 OpenAI OAuth 账号的上游请求增加 Codex 客户端 TLS 指纹模拟能力。
方案：用 Rust 实现一个独立 HTTP 代理服务，复刻 Codex CLI（reqwest + rustls）的 TLS 栈，
sub2api 把所有 OpenAI OAuth 账号的上游请求转发给该代理，由代理通过账号配置的 proxy 发出请求。

## 非目标

- 不模拟 Electron/Chromium 的 BoringSSL 指纹（Codex Desktop 的 backend-api 请求由内嵌 Rust 子进程发出，TLS 栈与 CLI 一致）。
- 不在 Rust 代理里设置 originator / User-Agent / OpenAI-Beta 等业务 headers（由 sub2api 现有逻辑设置，Rust 代理只做透明转发）。
- 不改动 sub2api 现有的 TLS 指纹体系（utls/Profile/Anthropic 路径保持不变）。
- 不处理 OpenAI APIKey 类型账号（仅 OAuth 账号走代理，api.openai.com 不走代理）。
- 不处理 WebSocket 路径（openai_ws_forwarder），仅覆盖 HTTP 路径。

## 当前理解

### Codex 客户端 TLS 栈

- Codex CLI / Codex Desktop（内嵌 CLI 子进程）的 backend-api 请求均由 Rust 端发出。
- HTTP 客户端：`reqwest` + `rustls-tls-native-roots` + `rustls`（见 codex-rs/codex-client/Cargo.toml）。
- OpenAI 区分 Codex CLI vs App 靠 `originator` + `User-Agent` headers，不靠 TLS 指纹。
- 因此"复刻 Codex app 的 TLS 指纹" = 用同样的 reqwest + rustls 栈发请求，TLS ClientHello 天然一致。

### sub2api OpenAI 上游请求入口

- 主入口：`OpenAIGatewayService`（backend/internal/service/openai_gateway_service.go）。
- 两条调用路径，均通过 `s.httpUpstream.Do(upstreamReq, proxyURL, account.ID, account.Concurrency)`：
  - 非 passthrough：`forwardOpenAI` → 行 3139。
  - passthrough：`forwardOpenAIPassthrough` → 行 3429。
- `httpUpstream` 接口定义在 `http_upstream_port.go`，实现在 `repository/http_upstream.go`。
- proxyURL 来源：`account.Proxy.URL()`（account.ProxyID != nil && account.Proxy != nil 时）。
- upstreamReq 已由 `buildUpstreamRequest` / `buildUpstreamRequestOpenAIPassthrough` 构建完成，包含完整 headers（authorization、originator、User-Agent、OpenAI-Beta、ChatGPT-Account-ID 等）和 body。
- 响应处理（streaming SSE / 非 streaming）完全基于 `*http.Response`，与请求发送方式解耦。

### 现有 TLS 指纹体系（Anthropic 专用，不改）

- `tlsfingerprint.Profile` + utls dialer，仅对 Anthropic OAuth/SetupToken 账号生效。
- `Account.IsTLSFingerprintEnabled()` 硬性 gate 在 `IsAnthropicOAuthOrSetupToken()`。
- OpenAI 路径走 `httpUpstream.Do`（不带 TLS），不走 `DoWithTLS`。
- 本方案不触碰这套体系，另起 Rust 代理通路。

### codex-tls-proxy 目录现状

- `codex-tls-proxy/` 目录存在，`src/` 为空，`target/` 有残留 Rust 编译产物，无 Cargo.toml。
- 判定为之前未完成的占位目录，本方案在此目录下从零构建。

## 实施计划

### 第 1 步：Rust 代理服务（codex-tls-proxy/）

在 `codex-tls-proxy/` 下创建完整 Rust 项目。

**Cargo.toml 依赖（对齐 Codex CLI 栈）：**
- `tokio`（rt-multi-thread, macros）
- `axum`（HTTP 服务框架，支持 streaming body）
- `reqwest`（features: `rustls-tls-native-roots`, `stream`, `json`, `socks`）— `socks` 用于支持 SOCKS5 proxy
- `rustls` / `rustls-native-certs` / `rustls-pki-types`
- `serde` / `serde_json`
- `tower` / `hyper`（axm 依赖）
- `tracing` / `tracing-subscriber`
- `clap`（CLI 参数，监听地址/端口）

**API 契约：**

`POST /forward`
- Request body（JSON）：
  ```json
  {
    "method": "POST",
    "url": "https://chatgpt.com/backend-api/codex/responses",
    "proxy_url": "http://user:pass@host:port",
    "headers": {"authorization": ["Bearer xxx"], "originator": ["codex_cli_rs"], ...},
    "body": "<base64 编码的原始请求 body>"
  }
  ```
  - `proxy_url` 为空字符串表示直连。
  - `body` 用 base64 编码避免 JSON 转义问题，支持二进制（图片输入等）。
  - `headers` 是 `map<string, string[]>`，保留多值 header。
- Response：
  - 状态码 = 上游响应状态码。
  - Response headers = 上游响应 headers（透传，排除 hop-by-hop headers）。
  - Response body = 上游响应 body 的 stream（axum 流式返回，支持 SSE）。
  - 错误时返回 502 + JSON error body，sub2api 侧当作 transport error 处理。

`GET /health`
- 返回 200，用于 sub2api 探活。

**核心逻辑：**
1. 用 `reqwest::Client::builder()` 构建客户端，features 用 `rustls-tls-native-roots`（与 codex 一致）。
2. 每个请求创建独立 reqwest Client（或按 proxy_url 缓存），设置 `.proxy(reqwest::Proxy::all(proxy_url)?)` 当 proxy_url 非空。
3. 用传入的 method/url/headers/body 构建 reqwest 请求并发送。
4. 拿到上游响应后，把 status / headers / body stream 透传回 sub2api。
5. body stream 用 reqwest 的 `bytes_stream()` + axum 的 `Body::from_stream()` 透传，不缓冲完整响应。

**配置：**
- 监听地址端口通过环境变量 `CODEX_TLS_PROXY_ADDR`（默认 `127.0.0.1:18900`）。
- 连接超时、响应头超时等对齐 codex 默认值。

### 第 2 步：sub2api 侧新增 Rust 代理客户端

**新增文件：** `backend/internal/repository/codex_tls_proxy_client.go`

- 定义 `CodexTLSProxyClient` 结构，持有 Rust 代理地址和 `*http.Client`。
- 方法 `Forward(ctx, req *http.Request, proxyURL string) (*http.Response, error)`：
  1. 读取 req.Body。
  2. 收集 req.Header 为 `map[string][]string`。
  3. 构造 `/forward` 请求 JSON（body base64 编码）。
  4. POST 到 Rust 代理。
  5. 把返回的 `*http.Response` 的 status / header / body 透传返回（body 直接用 resp.Body，天然 streaming）。
- 方法 `HealthCheck(ctx) error`：GET `/health`。

### 第 3 步：sub2api 配置项

**文件：** `backend/internal/config/config.go`

新增配置：
- `codex_tls_proxy_enabled`（bool，默认 false）：是否启用 Rust 代理。
- `codex_tls_proxy_url`（string，默认 `http://127.0.0.1:18900`）：Rust 代理地址。

启用后，所有 OpenAI OAuth 账号的上游请求走 Rust 代理；关闭则走原 `httpUpstream.Do`。

### 第 4 步：OpenAI 网关接入 Rust 代理

**文件：** `backend/internal/service/openai_gateway_service.go`

修改两个调用点（行 3139、行 3429）：

```go
// 修改前
resp, err := s.httpUpstream.Do(upstreamReq, proxyURL, account.ID, account.Concurrency)

// 修改后
resp, err := s.forwardOpenAIUpstream(ctx, upstreamReq, proxyURL, account)
```

新增 `forwardOpenAIUpstream` 方法：
- 如果 `account.Type == AccountTypeOAuth` 且配置启用了 codex_tls_proxy → 调 `s.codexTLSProxy.Forward(ctx, req, proxyURL)`。
- 否则 → 调 `s.httpUpstream.Do(req, proxyURL, account.ID, account.Concurrency)`（原路径）。

这样改动集中在两个调用点 + 一个新方法，不入侵 httpUpstream 接口和 repository 层。

### 第 5 步：wire 注入

**文件：** `backend/internal/service/wire.go`、`backend/internal/repository/wire.go`、`backend/cmd/server/wire_gen.go`

- 在 repository wire 里加 `NewCodexTLSProxyClient`。
- 在 OpenAIGatewayService 构造函数加 `codexTLSProxy *CodexTLSProxyClient` 参数。
- 从 config 读取 enabled / url 传入。

### 第 6 步：测试

**Rust 侧（codex-tls-proxy/）：**
- 单元测试：`/forward` 正常转发、proxy 传递、header 透传、SSE 流式响应透传、错误响应透传。
- 集成测试：启动真实 axum 服务 + mock 上游，验证端到端。

**sub2api 侧：**
- `codex_tls_proxy_client_test.go`：Forward 方法的请求构造、响应透传、body streaming。
- `openai_gateway_service_test.go`：启用 codex_tls_proxy 时 OpenAI OAuth 请求走代理、关闭时走原路径。
- 复用现有 test stub 模式（参考 `httpUpstreamRecorder`）。

## 验证计划

1. **Rust 代理编译**：`cd codex-tls-proxy && cargo build --release`。
2. **Rust 代理单元测试**：`cargo test`。
3. **Rust 代理手动验证**：启动代理，用 curl 发 `/forward` 请求打真实 chatgpt.com（需有效 token），抓包对比 TLS ClientHello 与真实 codex CLI 一致。
4. **sub2api 编译**：`cd backend && go build ./...`。
5. **sub2api 单元测试**：`go test ./internal/repository/... ./internal/service/... -run "CodexTLSProxy\|OpenAIGateway"`。
6. **端到端验证**：启动 Rust 代理 + sub2api，配置 `codex_tls_proxy_enabled=true`，用 OpenAI OAuth 账号发请求，确认请求经 Rust 代理发出且响应正常。
7. **TLS 指纹对比**：用 Wireshark 或 tls-fingerprint 工具抓 Rust 代理发出的 ClientHello，与真实 codex CLI 对比 JA3/JA4。

## 风险与影响

### 影响范围
- **新增**：codex-tls-proxy/ Rust 项目（独立，不影响现有 Go 代码）。
- **新增**：backend/internal/repository/codex_tls_proxy_client.go（新文件）。
- **修改**：openai_gateway_service.go 两个调用点（3139、3429）+ 新增 forwardOpenAIUpstream 方法。
- **修改**：config.go 新增 2 个配置项。
- **修改**：wire 注入相关文件。

### 风险
1. **性能**：多一跳本地 HTTP 转发（sub2api → Rust 代理 → 上游），增加延迟。本地回环网络开销极低（<1ms），可接受。
2. **可靠性**：Rust 代理挂掉会导致 OpenAI OAuth 请求全部失败（不 fallback，直接返回 transport error，触发 sub2api 现有 failover 机制切其他账号）。需运维保障代理可用性。
3. **body 编码**：base64 编解码有 CPU 开销，但保证二进制安全。大 body（图片输入）时需关注。
4. **SSE 透传**：axum + reqwest 的 streaming 需要正确处理背压和连接断开，需测试长连接稳定性。
5. **proxy 传递**：sub2api 支持 http/https/socks5 三种 proxy 协议（见 ent/schema/proxy.go + crs_sync_service.go 校验）。reqwest 需开启 `socks` feature 以支持 SOCKS5。

### 兼容性
- `codex_tls_proxy_enabled=false`（默认）时，行为与当前完全一致，零影响。
- 不影响 Anthropic TLS 指纹路径。
- 不影响 OpenAI APIKey 账号。

## 执行记录

- 2026-06-29：创建计划文件。确认方案为独立 Rust HTTP 服务 + 自定义 /forward API + 所有 OpenAI OAuth 请求走代理 + SSE 透传流。
- 2026-06-29：用户要求最小入侵源文件，所有新文件用 `codex_tls_proxy_` 前缀。方案调整为：
  - 配置改用环境变量（`CODEX_TLS_PROXY_ENABLED` / `CODEX_TLS_PROXY_URL`），不改 config.go。
  - 用 HTTPUpstream 装饰器模式（`codexTLSProxyUpstream`）包装原始 HTTPUpstream，不改 openai_gateway_service.go。
  - 装饰器通过 `req.Host == "chatgpt.com"` 判断是否走 Rust 代理，不影响其他平台请求。
  - 源文件仅改 `repository/wire.go`（1 行：`NewHTTPUpstream` → `NewCodexTLSProxyHTTPUpstream`）和 `cmd/server/wire_gen.go`（1 行，自动生成文件的同步修改）。
- 2026-06-29：完成全部实现。
  - Rust 代理：`codex-tls-proxy/` 下 Cargo.toml + src/{lib,main,handler,proxy_client}.rs + tests/forward_test.rs，10 个测试全部通过，release 编译通过。
  - sub2api：新增 `codex_tls_proxy_client.go` + `codex_tls_proxy_upstream.go` + 对应测试文件，15 个测试全部通过，go build 通过。
  - WebSocket 路径本期不覆盖（用户确认）。
- 2026-06-29：用户要求所有 OpenAI 相关请求都走 TLS 代理 + 修复所有超时/body 限制问题。
  - 需要覆盖的路径（除已覆盖的 httpUpstream.Do 外）：
    1. httpUpstream.DoWithTLS → account_test_service 的 chatgpt.com 请求
    2. privacyClientFactory（imroc/req）→ 配额查询、额度重置、隐私设置、账号信息、订阅信息
    3. httpclient.GetClient → 账号探测（account_usage_service）、PAT whoami（openai_codex_pat_service）
  - 需要修复的问题：
    1. Rust 代理 .timeout(300s) 会截断 SSE 流 → 去掉总超时，改用 connect_timeout + 响应头超时
    2. Rust 代理无响应头超时 → 用 tokio::time::timeout 包裹 send()
    3. Rust 代理 axum 无请求体大小限制 → 加 DefaultBodyLimit
    4. Go 客户端 /forward 请求体无大小限制 → 加 body 限制
  - 方案：
    - 在 httpclient 包新增 `codex_tls_proxy_transport.go`：自定义 RoundTripper + GetCodexTLSProxyClient 辅助函数
    - 在 repository 包新增 `codex_tls_proxy_privacy_client.go`：包装 privacyClientFactory
    - 更新 codex_tls_proxy_upstream.go：DoWithTLS 也拦截 OpenAI 域名
    - 源文件修改：account_usage_service.go（1 行）、openai_codex_pat_service.go（1 行）、wire.go（1 行）、wire_gen.go（1 行）
  - 完成全部修改，编译 + 测试通过：
    - Rust 代理：10 个测试通过
    - Go httpclient 包：7 个测试通过
    - Go repository 包：14 个测试通过（含 privacy client 3 个 + upstream 6 个 + client 5 个）
    - go build ./... 通过
  - 新增 Docker 镜像构建支持：
    - `codex-tls-proxy/Dockerfile`：多阶段构建（rust:1.88-alpine → alpine:3.21），release 二进制 4.6MB
    - `codex-tls-proxy/docker-compose.yml`：独立部署配置，接入 sub2api-network
    - 已验证：docker build 成功，容器启动正常，/health 返回 200
