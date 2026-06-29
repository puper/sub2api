package repository

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// codex_tls_proxy_client.go
//
// Codex TLS 指纹代理客户端：将上游请求转发给 Rust 代理服务（reqwest + rustls 栈），
// 由 Rust 代理复刻 Codex CLI 的 TLS 指纹发出请求。
// 代理故障时不 fallback，直接返回 error，由上层触发 failover。

// codexTLSProxyForwardRequest /forward 接口请求体
type codexTLSProxyForwardRequest struct {
	Method   string              `json:"method"`
	URL      string              `json:"url"`
	ProxyURL string              `json:"proxy_url"`
	Headers  map[string][]string `json:"headers"`
	Body     string              `json:"body"` // base64 编码的原始请求 body
}

// CodexTLSProxyClient Codex TLS 指纹代理客户端
type CodexTLSProxyClient struct {
	url    string
	client *http.Client
}

// Forward 将请求转发给 Rust 代理
//
// 参数:
//   - ctx: 请求 context，控制超时和取消
//   - req: 已构建完整的上游请求（含 headers/body/method/url）
//   - proxyURL: 账号配置的代理 URL，空字符串表示直连
//
// 返回:
//   - *http.Response: 上游响应（body 为流式，调用方需关闭）
//   - error: 代理或上游错误
func (c *CodexTLSProxyClient) Forward(ctx context.Context, req *http.Request, proxyURL string) (*http.Response, error) {
	// 读取请求 body
	var bodyBytes []byte
	if req.Body != nil {
		var err error
		bodyBytes, err = io.ReadAll(req.Body)
		if err != nil {
			return nil, fmt.Errorf("codex_tls_proxy: read request body: %w", err)
		}
		_ = req.Body.Close()
	}

	// 收集 headers 为 map[string][]string
	headers := make(map[string][]string, len(req.Header))
	for key, values := range req.Header {
		headers[key] = values
	}

	// 构造 /forward 请求体
	forwardBody := codexTLSProxyForwardRequest{
		Method:   req.Method,
		URL:      req.URL.String(),
		ProxyURL: proxyURL,
		Headers:  headers,
		Body:     base64.StdEncoding.EncodeToString(bodyBytes),
	}

	jsonBody, err := json.Marshal(forwardBody)
	if err != nil {
		return nil, fmt.Errorf("codex_tls_proxy: marshal forward request: %w", err)
	}

	// 发送给 Rust 代理
	forwardReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url+"/forward", bytes.NewReader(jsonBody))
	if err != nil {
		return nil, fmt.Errorf("codex_tls_proxy: build forward request: %w", err)
	}
	forwardReq.Header.Set("Content-Type", "application/json")

	resp, err := c.client.Do(forwardReq)
	if err != nil {
		return nil, fmt.Errorf("codex_tls_proxy: forward request failed: %w", err)
	}

	// Rust 代理返回 502 表示上游请求失败，读取错误信息后返回 error
	if resp.StatusCode == http.StatusBadGateway {
		errBody, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return nil, fmt.Errorf("codex_tls_proxy: upstream request failed (502): %s", string(errBody))
	}

	// 透传响应：status / headers / body stream
	// 移除 hop-by-hop headers
	resp.Header.Del("Connection")
	resp.Header.Del("Keep-Alive")
	resp.Header.Del("Transfer-Encoding")
	resp.Header.Del("Upgrade")

	return resp, nil
}

// HealthCheck 检查 Rust 代理是否可用
func (c *CodexTLSProxyClient) HealthCheck(ctx context.Context) error {
	checkCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(checkCtx, http.MethodGet, c.url+"/health", nil)
	if err != nil {
		return fmt.Errorf("codex_tls_proxy: build health check request: %w", err)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("codex_tls_proxy: health check failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("codex_tls_proxy: health check returned status %d", resp.StatusCode)
	}
	return nil
}
