package repository

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/imroc/req/v3"
)

// codex_tls_proxy_privacy_client.go
//
// 包装 PrivacyClientFactory（返回 *req.Client），使 OpenAI 隐私/配额/账号信息等
// 请求也走 Rust TLS 指纹代理。
//
// 这些请求全部目标 chatgpt.com/backend-api/，使用 imroc/req 客户端（Chrome 指纹模拟）。
// 启用 Codex TLS 代理后，用 Rust 代理替代 Chrome 指纹模拟，TLS 栈更贴近 Codex CLI。
//
// 覆盖的请求路径：
// - disableOpenAITraining → chatgpt.com/backend-api/settings/account_user_setting
// - fetchChatGPTAccountInfo → chatgpt.com/backend-api/accounts/check/v4-2023-04-27
// - fetchChatGPTSubscriptionExpiresAt → chatgpt.com/backend-api/subscriptions
// - OpenAIQuotaService.GetUsage → chatgpt.com/backend-api/wham/usage
// - OpenAIQuotaService.ResetCredit → chatgpt.com/backend-api/wham/rate-limit-reset-credits/consume

// CodexTLSProxyPrivacyClientFactory 创建可能被 Rust 代理包装的 privacy req 客户端
//
// 替代原 CreatePrivacyReqClient，当 CODEX_TLS_PROXY_ENABLED=true 时返回
// transport 被 codexTLSProxyTransport 包装的 req.Client；否则返回原 req.Client。
//
// 注意：不使用 getSharedReqClient 缓存，因为修改底层 transport 会影响缓存实例。
// 每次创建新客户端，这些请求频率低（token 刷新 / 账号设置），连接池复用不是关键。
func CodexTLSProxyPrivacyClientFactory(proxyURL string) (*req.Client, error) {
	loadCodexTLSProxyEnv()
	if !codexTLSProxyEnabled {
		return CreatePrivacyReqClient(proxyURL)
	}

	// 启用时创建新 req.Client（不用缓存），用 codex TLS 代理 transport 包装
	client := req.C().SetTimeout(30 * time.Second)

	trimmed := strings.TrimSpace(proxyURL)
	if trimmed != "" {
		client.SetProxyURL(trimmed)
	}

	// 获取底层 http.Client，用 codex TLS 代理 transport 包装其 Transport
	httpClient := client.GetClient()
	originalTransport := httpClient.Transport
	if originalTransport == nil {
		originalTransport = http.DefaultTransport
	}

	httpClient.Transport = &privacyReqTransportAdapter{
		inner:         originalTransport,
		upstreamProxy: trimmed,
		rustProxyURL:  strings.TrimRight(codexTLSProxyURL, "/"),
		rustClient:    &http.Client{Timeout: 0},
	}

	return client, nil
}

// privacyReqTransportAdapter 适配 req.Client 底层 http.Client 的 transport
//
// 此工厂的所有请求都目标 chatgpt.com，全部走 Rust 代理。
type privacyReqTransportAdapter struct {
	inner         http.RoundTripper
	upstreamProxy string
	rustProxyURL  string
	rustClient    *http.Client
}

func (t *privacyReqTransportAdapter) RoundTrip(req *http.Request) (*http.Response, error) {
	return forwardViaRustProxy(req, t.rustClient, t.rustProxyURL, t.upstreamProxy)
}

// forwardViaRustProxy 将请求转发给 Rust 代理
//
// 与 CodexTLSProxyClient.Forward 逻辑一致，独立定义以避免包间依赖。
func forwardViaRustProxy(req *http.Request, rustClient *http.Client, rustProxyURL, upstreamProxy string) (*http.Response, error) {
	// 读取请求 body（512MB 上限）
	var bodyBytes []byte
	if req.Body != nil {
		var err error
		bodyBytes, err = io.ReadAll(io.LimitReader(req.Body, 512*1024*1024))
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
	forwardBody := struct {
		Method   string              `json:"method"`
		URL      string              `json:"url"`
		ProxyURL string              `json:"proxy_url"`
		Headers  map[string][]string `json:"headers"`
		Body     string              `json:"body"`
	}{
		Method:   req.Method,
		URL:      req.URL.String(),
		ProxyURL: upstreamProxy,
		Headers:  headers,
		Body:     base64.StdEncoding.EncodeToString(bodyBytes),
	}

	jsonBody, err := json.Marshal(forwardBody)
	if err != nil {
		return nil, fmt.Errorf("codex_tls_proxy: marshal forward request: %w", err)
	}

	// 发送给 Rust 代理
	forwardReq, err := http.NewRequestWithContext(req.Context(), http.MethodPost, rustProxyURL+"/forward", bytes.NewReader(jsonBody))
	if err != nil {
		return nil, fmt.Errorf("codex_tls_proxy: build forward request: %w", err)
	}
	forwardReq.Header.Set("Content-Type", "application/json")

	resp, err := rustClient.Do(forwardReq)
	if err != nil {
		return nil, fmt.Errorf("codex_tls_proxy: forward request failed: %w", err)
	}

	// 502 = 上游请求失败
	if resp.StatusCode == http.StatusBadGateway {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
		return nil, fmt.Errorf("codex_tls_proxy: upstream request failed (502): %s", string(errBody))
	}

	// 504 = 响应头超时
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
