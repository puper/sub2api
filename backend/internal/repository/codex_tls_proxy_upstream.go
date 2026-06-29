package repository

import (
	"net/http"
	"os"
	"strings"
	"sync"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

// codex_tls_proxy_upstream.go
//
// HTTPUpstream 装饰器：在 OpenAI 相关请求时，将请求转发给
// Rust TLS 指纹代理服务（reqwest + rustls 栈），复刻 Codex CLI 的 TLS 指纹。
// 其他请求（Anthropic/Gemini/OpenAI APIKey 等）直接走原始 HTTPUpstream，不受影响。
//
// 覆盖的 HTTPUpstream 方法：
// - Do：主链路（forwardOpenAI / forwardOpenAIPassthrough）
// - DoWithTLS：account_test_service 的 chatgpt.com 请求
//
// 配置通过环境变量读取，不修改 config.go：
//   CODEX_TLS_PROXY_ENABLED=true          启用（默认 false）
//   CODEX_TLS_PROXY_URL=http://127.0.0.1:18900  Rust 代理地址

var codexTLSProxyEnvOnce sync.Once
var codexTLSProxyEnabled bool
var codexTLSProxyURL string

// loadCodexTLSProxyEnv 从环境变量读取配置（只读一次）
func loadCodexTLSProxyEnv() {
	codexTLSProxyEnvOnce.Do(func() {
		codexTLSProxyEnabled = strings.EqualFold(os.Getenv("CODEX_TLS_PROXY_ENABLED"), "true")
		codexTLSProxyURL = strings.TrimSpace(os.Getenv("CODEX_TLS_PROXY_URL"))
		if codexTLSProxyURL == "" {
			codexTLSProxyURL = "http://127.0.0.1:18900"
		}
	})
}

// codexTLSProxyUpstream 包装原始 HTTPUpstream，在 OpenAI 相关请求时走 Rust 代理
type codexTLSProxyUpstream struct {
	inner       service.HTTPUpstream
	proxyClient *CodexTLSProxyClient
	enabled     bool
}

// NewCodexTLSProxyHTTPUpstream wire provider：创建可能被 Rust 代理包装的 HTTPUpstream
//
// 替代原 NewHTTPUpstream，当 CODEX_TLS_PROXY_ENABLED=true 时返回装饰器，
// 否则返回原始 HTTPUpstream（行为完全不变）。
func NewCodexTLSProxyHTTPUpstream(cfg *config.Config) service.HTTPUpstream {
	inner := NewHTTPUpstream(cfg)

	loadCodexTLSProxyEnv()
	if !codexTLSProxyEnabled {
		return inner
	}

	return &codexTLSProxyUpstream{
		inner:       inner,
		proxyClient: &CodexTLSProxyClient{url: strings.TrimRight(codexTLSProxyURL, "/"), client: &http.Client{Timeout: 0}},
		enabled:     true,
	}
}

// Do 执行 HTTP 请求
//
// 当请求目标是 OpenAI 相关域名（chatgpt.com、auth.openai.com）时，走 Rust 代理；
// 否则走原始 HTTPUpstream.Do。
func (u *codexTLSProxyUpstream) Do(req *http.Request, proxyURL string, accountID int64, accountConcurrency int) (*http.Response, error) {
	if u.shouldForwardToProxy(req) {
		return u.proxyClient.Forward(req.Context(), req, proxyURL)
	}
	return u.inner.Do(req, proxyURL, accountID, accountConcurrency)
}

// DoWithTLS 执行带 TLS 指纹伪装的 HTTP 请求
//
// 当请求目标是 OpenAI 相关域名时，走 Rust 代理（Rust 代理用 rustls 处理 TLS，
// 不需要 utls profile）；否则走原始 HTTPUpstream.DoWithTLS（Anthropic utls 路径不受影响）。
func (u *codexTLSProxyUpstream) DoWithTLS(req *http.Request, proxyURL string, accountID int64, accountConcurrency int, profile *tlsfingerprint.Profile) (*http.Response, error) {
	if u.shouldForwardToProxy(req) {
		return u.proxyClient.Forward(req.Context(), req, proxyURL)
	}
	return u.inner.DoWithTLS(req, proxyURL, accountID, accountConcurrency, profile)
}

// shouldForwardToProxy 判断请求是否应该走 Rust 代理
//
// 需要走 TLS 代理的域名：
// - chatgpt.com：Codex CLI 的所有 backend-api 请求（主链路 + 账号测试）
// - auth.openai.com：Codex PAT whoami 验证
//
// 不走代理的域名：
// - api.openai.com：标准 API Key 访问，不需要 TLS 指纹模拟
func (u *codexTLSProxyUpstream) shouldForwardToProxy(req *http.Request) bool {
	if req == nil {
		return false
	}
	// req.Host 优先（buildUpstreamRequest 会设置 req.Host = "chatgpt.com"）
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
