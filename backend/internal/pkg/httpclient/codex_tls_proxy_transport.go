// Package httpclient 提供 Codex TLS 指纹代理的 transport 层拦截。
//
// 本文件实现自定义 http.RoundTripper，将 OpenAI 相关域名（chatgpt.com、auth.openai.com）
// 的请求路由到 Rust TLS 指纹代理服务，由 Rust 代理用 reqwest + rustls 栈发出请求，
// 复刻 Codex CLI 的 TLS 指纹。非 OpenAI 域名的请求直接走原始 transport，不受影响。
//
// 配置通过环境变量读取（与 repository/codex_tls_proxy_upstream.go 共享）：
//   CODEX_TLS_PROXY_ENABLED=true          启用（默认 false）
//   CODEX_TLS_PROXY_URL=http://127.0.0.1:18900  Rust 代理地址
//
// 用法：
//   client, err := httpclient.GetCodexTLSProxyClient(opts)
//   替代原来的 httpclient.GetClient(opts)，启用时自动包装 transport。
package httpclient

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// codex_tls_proxy_transport.go
//
// 自定义 RoundTripper：OpenAI 域名请求走 Rust 代理，其他请求走原始 transport。

var (
	codexTLSProxyTransportOnce sync.Once
	codexTLSProxyTransportCfg  codexTLSProxyTransportConfig
)

type codexTLSProxyTransportConfig struct {
	enabled bool
	url     string
}

// loadCodexTLSProxyConfig 从环境变量读取配置（只读一次）
func loadCodexTLSProxyConfig() {
	codexTLSProxyTransportOnce.Do(func() {
		codexTLSProxyTransportCfg.enabled = strings.EqualFold(os.Getenv("CODEX_TLS_PROXY_ENABLED"), "true")
		codexTLSProxyTransportCfg.url = strings.TrimSpace(os.Getenv("CODEX_TLS_PROXY_URL"))
		if codexTLSProxyTransportCfg.url == "" {
			codexTLSProxyTransportCfg.url = "http://127.0.0.1:18900"
		}
	})
}

// IsCodexTLSProxyEnabled 返回是否启用了 Codex TLS 指纹代理
func IsCodexTLSProxyEnabled() bool {
	loadCodexTLSProxyConfig()
	return codexTLSProxyTransportCfg.enabled
}

// codexTLSProxyForwardRequest /forward 接口请求体
type codexTLSProxyForwardRequest struct {
	Method   string              `json:"method"`
	URL      string              `json:"url"`
	ProxyURL string              `json:"proxy_url"`
	Headers  map[string][]string `json:"headers"`
	Body     string              `json:"body"`
}

// codexTLSProxyTransport 自定义 RoundTripper
//
// OpenAI 域名（chatgpt.com、auth.openai.com）的请求路由到 Rust 代理，
// 其他请求委托给 inner transport。
type codexTLSProxyTransport struct {
	inner        http.RoundTripper // 非 OpenAI 请求的 fallback transport
	upstreamProxy string            // 上游代理 URL（传给 Rust 代理用于发出请求）
	rustProxyURL string            // Rust 代理服务地址
	client       *http.Client      // 用于调用 Rust 代理的 HTTP 客户端
}

// RoundTrip 实现 http.RoundTripper
func (t *codexTLSProxyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if !isOpenAIDomain(req) {
		return t.inner.RoundTrip(req)
	}
	return t.forwardToRustProxy(req)
}

// forwardToRustProxy 将请求转发给 Rust 代理
func (t *codexTLSProxyTransport) forwardToRustProxy(req *http.Request) (*http.Response, error) {
	// 读取请求 body
	var bodyBytes []byte
	if req.Body != nil {
		var err error
		bodyBytes, err = io.ReadAll(io.LimitReader(req.Body, 512*1024*1024)) // 512MB 上限
		if err != nil {
			return nil, fmt.Errorf("codex_tls_proxy: read request body: %w", err)
		}
		_ = req.Body.Close()
	}

	// 收集 headers
	headers := make(map[string][]string, len(req.Header))
	for key, values := range req.Header {
		headers[key] = values
	}

	// 构造 /forward 请求体
	forwardBody := codexTLSProxyForwardRequest{
		Method:   req.Method,
		URL:      req.URL.String(),
		ProxyURL: t.upstreamProxy,
		Headers:  headers,
		Body:     base64.StdEncoding.EncodeToString(bodyBytes),
	}

	jsonBody, err := json.Marshal(forwardBody)
	if err != nil {
		return nil, fmt.Errorf("codex_tls_proxy: marshal forward request: %w", err)
	}

	// 发送给 Rust 代理
	forwardReq, err := http.NewRequestWithContext(req.Context(), http.MethodPost, t.rustProxyURL+"/forward", bytes.NewReader(jsonBody))
	if err != nil {
		return nil, fmt.Errorf("codex_tls_proxy: build forward request: %w", err)
	}
	forwardReq.Header.Set("Content-Type", "application/json")

	resp, err := t.client.Do(forwardReq)
	if err != nil {
		return nil, fmt.Errorf("codex_tls_proxy: forward request failed: %w", err)
	}

	// Rust 代理返回 502 表示上游请求失败
	if resp.StatusCode == http.StatusBadGateway {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
		return nil, fmt.Errorf("codex_tls_proxy: upstream request failed (502): %s", string(errBody))
	}

	// Rust 代理返回 504 表示响应头超时
	if resp.StatusCode == http.StatusGatewayTimeout {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
		return nil, fmt.Errorf("codex_tls_proxy: upstream response header timeout: %s", string(errBody))
	}

	// 移除 hop-by-hop headers
	resp.Header.Del("Connection")
	resp.Header.Del("Keep-Alive")
	resp.Header.Del("Transfer-Encoding")
	resp.Header.Del("Upgrade")

	return resp, nil
}

// isOpenAIDomain 判断请求目标是否为 OpenAI 相关域名
//
// 需要走 TLS 代理的域名：
// - chatgpt.com：Codex CLI 的所有 backend-api 请求
// - auth.openai.com：Codex PAT whoami 验证
//
// 不走代理的域名：
// - api.openai.com：标准 API Key 访问，不需要 TLS 指纹模拟
func isOpenAIDomain(req *http.Request) bool {
	if req == nil {
		return false
	}
	host := req.Host
	if host == "" && req.URL != nil {
		host = req.URL.Host
	}
	host = strings.ToLower(strings.TrimSpace(host))
	// 去掉端口号
	if idx := strings.Index(host, ":"); idx >= 0 {
		host = host[:idx]
	}
	return host == "chatgpt.com" || host == "auth.openai.com"
}

// wrapClientWithCodexTLSProxy 包装 http.Client 的 transport
//
// 如果未启用 Codex TLS 代理，直接返回原 client。
// 启用时，用 codexTLSProxyTransport 包装原 transport。
func wrapClientWithCodexTLSProxy(client *http.Client, upstreamProxyURL string) *http.Client {
	loadCodexTLSProxyConfig()
	if !codexTLSProxyTransportCfg.enabled {
		return client
	}

	inner := client.Transport
	if inner == nil {
		inner = http.DefaultTransport
	}

	return &http.Client{
		Transport: &codexTLSProxyTransport{
			inner:        inner,
			upstreamProxy: upstreamProxyURL,
			rustProxyURL: strings.TrimRight(codexTLSProxyTransportCfg.url, "/"),
			client:       &http.Client{Timeout: 0}, // 超时由请求 context 控制
		},
		Timeout: client.Timeout,
	}
}

// GetCodexTLSProxyClient 获取可能被 Codex TLS 代理包装的 HTTP 客户端
//
// 替代 httpclient.GetClient，启用时自动包装 transport。
// 未启用时行为与 GetClient 完全一致。
func GetCodexTLSProxyClient(opts Options) (*http.Client, error) {
	client, err := GetClient(opts)
	if err != nil {
		return nil, err
	}
	return wrapClientWithCodexTLSProxy(client, opts.ProxyURL), nil
}

// HealthCheckCodexTLSProxy 检查 Rust 代理是否可用
func HealthCheckCodexTLSProxy(ctx context.Context) error {
	loadCodexTLSProxyConfig()
	if !codexTLSProxyTransportCfg.enabled {
		return nil // 未启用，无需检查
	}

	checkCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(checkCtx, http.MethodGet, codexTLSProxyTransportCfg.url+"/health", nil)
	if err != nil {
		return fmt.Errorf("codex_tls_proxy: build health check request: %w", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("codex_tls_proxy: health check failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("codex_tls_proxy: health check returned status %d", resp.StatusCode)
	}
	return nil
}
