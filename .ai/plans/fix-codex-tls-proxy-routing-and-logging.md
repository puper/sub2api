# 修复 Codex TLS 代理:重置次数未走代理 + 请求无日志

## 目标

1. 让 OpenAI 配额/重置次数(`OpenAIQuotaService.QueryUsage` / `ResetCredit`)等走 privacy client 工厂的请求,在启用 Codex TLS 代理时真正经过 Rust 代理。
2. 给 Rust 代理 `/forward` 增加请求级日志,便于确认请求是否真的到达(不泄露 token/proxy 凭据)。
3. 给出 Docker 分容器部署下"健康检测无法通过"的诊断与精确配置指引。

## 非目标

- 不改动主网关 chat/responses 链路(`codex_tls_proxy_upstream.go` 装饰器已正确接线,`wire_gen.go:118`)。
- 不改三套代理机制的整体设计,只修复 wire 接线遗漏与日志缺失。
- 不改 go.mod 的 go/toolchain 版本(用缓存工具链绕过,不动版本)。
- 不擅自给主 `deploy/docker-compose.yml` 加 codex-tls-proxy 服务/环境变量(先给指引,确认后再做)。

## 当前理解(已验证)

存在三套独立"走 Rust 代理"机制,各自读同名环境变量:
- ① `repository/codex_tls_proxy_upstream.go`(HTTPUpstream 装饰器):主网关链路。已接 `wire_gen.go:118`。
- ② `repository/codex_tls_proxy_privacy_client.go`(`CodexTLSProxyPrivacyClientFactory`):配额/重置次数、token 刷新、账号信息。**未接**。
- ③ `pkg/httpclient/codex_tls_proxy_transport.go`(`GetCodexTLSProxyClient`):account_usage、codex PAT。调用点已用。

根因(重置次数没走代理):
- `OpenAIQuotaService.QueryUsage`(行 130)/`ResetCredit`(行 172)用 `s.privacyClientFactory(proxyURL)`。
- 手写 `wire.go:62` 已改成 `CodexTLSProxyPrivacyClientFactory`,但 `//go:build wireinject`,不参与运行编译。
- 真正编译的 `wire_gen.go:298` 仍返回 `repository.CreatePrivacyReqClient`。
- 说明之前只手工改了 `wire_gen.go:118`(httpUpstream),没重新生成 wire,导致 privacy 工厂遗漏。
- 结果:重置次数请求直连 chatgpt.com,不经代理,代理也收不到、无日志。

请求无日志:
- Rust `forward`/`health` handler 无任何请求级日志,仅 `main.rs` 启动/关闭两行;日志级别由 `RUST_LOG` 控制。

健康检测无法通过(Docker 分容器):
- `HealthCheckCodexTLSProxy` 是死代码,主项目无内置健康检查。
- 主 `deploy/docker-compose.yml` 未配置 `CODEX_TLS_PROXY_*`,默认 URL `127.0.0.1:18900` 在 sub2api 容器内指向自己,无法到达 codex-tls-proxy 容器。
- 正确做法:同一 docker 网络 + `CODEX_TLS_PROXY_URL=http://codex-tls-proxy:18900`。

环境坑点(已验证):
- go.mod `go 1.26.4`,本地 `go1.26.1`,`GOSUMDB=off` 导致 `GOTOOLCHAIN=auto` 无法下载/校验 1.26.4。
- 但 1.26.4 已缓存:`/Users/puper/go/pkg/mod/golang.org/toolchain@v0.0.1-go1.26.4.darwin-arm64/bin/go`。
- 绕过:直接用该 go 二进制 + `GOTOOLCHAIN=local`,已验证 `go build ./cmd/server` 通过。

## 实施计划

1. 重新生成 wire:在 `backend/cmd/server` 跑 wire(用缓存工具链),核对 `wire_gen.go`:
   - `providePrivacyClientFactory` 返回 `CodexTLSProxyPrivacyClientFactory`(修复点)。
   - `httpUpstream` 仍为 `NewCodexTLSProxyHTTPUpstream`(不丢失)。
   - 复查 `git diff` 仅为预期变更。
2. Rust `forward` 增加请求级日志:进入时记 method/url/是否带 proxy;拿到响应后记 status;错误分支记原因。**不打印 headers、body、token、完整 proxy_url(含凭据)**。
3. 验证。
4. 输出 Docker 健康检测诊断 + 精确配置。

## 验证计划

- `GOTOOLCHAIN=local <cached-go> build ./...`(backend)。
- `GOTOOLCHAIN=local <cached-go> test ./internal/repository/... ./internal/service/... -run "CodexTLSProxy|OpenAIQuota"`。
- `cargo build` + `cargo test`(codex-tls-proxy)。

## 风险与影响

- 重新生成 wire 会按规范重排 `wire_gen.go` 的 import 与初始化顺序,属生成代码正常变化,需 diff 复核确保无逻辑外变更。
- privacy 工厂改为代理包装后,仅在 `CODEX_TLS_PROXY_ENABLED=true` 时生效;未启用时 `CodexTLSProxyPrivacyClientFactory` 内部回退到 `CreatePrivacyReqClient`,行为不变。
- Rust 日志需严格避免泄露 authorization / proxy 凭据。

## 执行记录

- wire 重生成:在 `backend/cmd/server` 用缓存工具链跑 wire,`wire_gen.go` diff 仅为
  `providePrivacyClientFactory` 改为 `CodexTLSProxyPrivacyClientFactory` + import 块恢复规范格式;
  `httpUpstream` 仍为 `NewCodexTLSProxyHTTPUpstream`,无逻辑外变更。
- Rust 日志:`handler.rs` 的 `forward` 增加入口/响应/各错误分支日志(仅 method/url/proxy 布尔/状态码,
  不含 headers/body/凭据);`main.rs` 的 `EnvFilter` 改为未设 `RUST_LOG` 时默认 `info`,否则以 `RUST_LOG` 为准
  (计划外但与"看到请求日志"目标强相关的小改动)。
- 验证:`go build ./...` 通过;`internal/repository`、`internal/pkg/httpclient`、`internal/service`
  相关单测通过;`cargo build` + `cargo test`(10 个)通过。
- 环境:go 工具链坑点已记录到 `.ai/memories/go-build-environment-quirks.md`。
- Docker 健康检测:确认主 `deploy/docker-compose.yml` 未配置 `CODEX_TLS_PROXY_*`,
  跨容器需 `CODEX_TLS_PROXY_URL=http://codex-tls-proxy:18900` + 同网络。
- (用户确认后)已给主 `deploy/docker-compose.yml` 的 sub2api 服务补上
  `CODEX_TLS_PROXY_ENABLED`(默认 false)/`CODEX_TLS_PROXY_URL`(默认服务名),
  并同步 `deploy/.env.example`;`docker compose config` 校验通过,变量按预期解析。
- 另:`codex-tls-proxy/Dockerfile`、`docker-compose.yml` 的 healthcheck `localhost`→`127.0.0.1`
  改动为用户开始前已有的未提交改动(修复容器内 localhost→IPv6 ::1 导致 healthcheck 失败),本次未触碰。
