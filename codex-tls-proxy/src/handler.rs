// axum 路由与请求处理
//
// POST /forward: 接收 sub2api 转发的请求，用 reqwest + native-tls 栈通过指定 proxy 发出，
//                把上游响应（含 SSE 流）透传回 sub2api。
// GET  /health:  健康检查。

use std::sync::Arc;
use std::time::{Duration, Instant};

use indexmap::IndexMap;

use axum::body::Body;
use axum::extract::State;
use axum::http::{HeaderMap, HeaderName, HeaderValue, StatusCode};
use axum::response::{IntoResponse, Response};
use axum::routing::{get, post};
use axum::Json;
use base64::Engine;
use serde::{Deserialize, Serialize};
use tokio::time::timeout;

use crate::proxy_client::ProxyClientPool;

/// hop-by-hop headers，不透传
const HOP_BY_HOP: &[&str] = &[
    "connection",
    "keep-alive",
    "proxy-authenticate",
    "proxy-authorization",
    "te",
    "trailers",
    "transfer-encoding",
    "upgrade",
];

/// /forward 请求体
#[derive(Debug, Deserialize)]
pub struct ForwardRequest {
    pub method: String,
    pub url: String,
    /// 代理 URL，空字符串表示直连
    #[serde(default)]
    pub proxy_url: String,
    /// 请求 headers，保持 JSON 解析顺序（IndexMap），
    /// 避免 HashMap 遍历打乱 header 顺序导致 H2 HPACK 编码顺序与 codex CLI 不一致
    #[serde(default)]
    pub headers: IndexMap<String, Vec<String>>,
    /// 请求 body，base64 编码
    #[serde(default)]
    pub body: String,
}

/// 错误响应体
#[derive(Debug, Serialize)]
struct ForwardError {
    error: String,
}

/// 应用状态
#[derive(Clone)]
pub struct AppState {
    pool: Arc<ProxyClientPool>,
}

/// /forward 请求体最大大小：512MB（base64 编码后约 682MB）
///
/// 对齐 sub2api 的 gateway.max_body_size（256MB）+ server.max_request_body_size（256MB），
/// base64 编码膨胀约 4/3 倍，取 512MB 覆盖最大场景。
const FORWARD_BODY_MAX_BYTES: usize = 512 * 1024 * 1024;

/// 等待上游响应头的超时时间：300s
///
/// 对齐 sub2api 的 defaultResponseHeaderTimeout（5 分钟）。
/// OpenAI/Codex 请求可能在上游排队较久，需要足够长的等待时间。
/// 注意：这不影响流式数据传输，只控制等待响应头的时间。
const UPSTREAM_RESPONSE_HEADER_TIMEOUT: Duration = Duration::from_secs(300);

/// 构建 axum 应用
pub fn app() -> axum::Router {
    let state = AppState {
        pool: Arc::new(ProxyClientPool::new()),
    };
    axum::Router::new()
        .route("/forward", post(forward))
        .route("/health", get(health))
        .with_state(state)
        // 限制 /forward 请求体大小，防止 OOM
        .layer(axum::extract::DefaultBodyLimit::max(FORWARD_BODY_MAX_BYTES))
}

/// 健康检查
async fn health() -> StatusCode {
    StatusCode::OK
}

/// 转发请求到上游
async fn forward(
    State(state): State<AppState>,
    Json(req): Json<ForwardRequest>,
) -> Response {
    // 记录请求入口：打印 method/url/是否带 proxy/user-agent，
    // 不打印 body、完整 proxy_url（可能含 token 或代理凭据）。
    // user-agent 不含敏感凭据，打印用于排查客户端指纹伪装是否生效。
    let start = Instant::now();
    let has_proxy = !req.proxy_url.is_empty();
    let user_agent = extract_header(&req.headers, "user-agent");
    tracing::info!(method = %req.method, url = %req.url, proxy = has_proxy, user_agent = %user_agent, "forward: incoming request");

    // 解析 method
    let method = match reqwest::Method::from_bytes(req.method.as_bytes()) {
        Ok(m) => m,
        Err(e) => {
            tracing::warn!(url = %req.url, error = %e, "forward: invalid method");
            return error_response(StatusCode::BAD_REQUEST, format!("invalid method: {e}"));
        }
    };

    // 获取 reqwest 客户端（按 proxy_url 缓存）
    let client = match state.pool.get(&req.proxy_url).await {
        Ok(c) => c,
        Err(e) => {
            tracing::error!(url = %req.url, error = %e, elapsed_ms = start.elapsed().as_millis(), "forward: build http client failed");
            return error_response(
                StatusCode::BAD_GATEWAY,
                format!("failed to build http client for proxy {:?}: {e}", req.proxy_url),
            );
        }
    };

    // 构建上游请求
    let mut builder = client.request(method, &req.url);

    // 设置 headers
    let mut headers = HeaderMap::new();
    for (key, values) in &req.headers {
        let name = match HeaderName::from_bytes(key.as_bytes()) {
            Ok(n) => n,
            Err(_) => continue, // 跳过非法 header 名
        };
        for v in values {
            if let Ok(val) = HeaderValue::from_str(v) {
                headers.append(&name, val);
            }
        }
    }
    builder = builder.headers(headers);

    // 设置 body（base64 解码）
    if !req.body.is_empty() {
        let body_bytes = match base64::engine::general_purpose::STANDARD.decode(&req.body) {
            Ok(b) => b,
            Err(e) => {
                tracing::warn!(url = %req.url, error = %e, "forward: invalid base64 body");
                return error_response(StatusCode::BAD_REQUEST, format!("invalid base64 body: {e}"));
            }
        };
        builder = builder.body(body_bytes);
    }

    // 发送请求，带响应头超时
    //
    // UPSTREAM_RESPONSE_HEADER_TIMEOUT 只控制等待响应头的时间，
    // 一旦响应头返回，流式 body 读取不受超时限制（SSE 可持续很久）。
    let upstream_resp = match timeout(UPSTREAM_RESPONSE_HEADER_TIMEOUT, builder.send()).await {
        Ok(Ok(r)) => r,
        Ok(Err(e)) => {
            // 展开完整 source 链：reqwest 顶层 Display 会丢掉真实原因，
            // 偶发失败必须看到底层原因（对端关闭/连接被拒/超时/TLS 等）才能定位。
            let detail = describe_reqwest_error(&e);
            tracing::error!(url = %req.url, proxy = has_proxy, error = %detail, elapsed_ms = start.elapsed().as_millis(), "forward: upstream request failed");
            return error_response(
                StatusCode::BAD_GATEWAY,
                format!("upstream request failed: {detail}"),
            );
        }
        Err(_) => {
            tracing::error!(
                url = %req.url,
                timeout_secs = UPSTREAM_RESPONSE_HEADER_TIMEOUT.as_secs(),
                elapsed_ms = start.elapsed().as_millis(),
                "forward: upstream response header timeout"
            );
            return error_response(
                StatusCode::GATEWAY_TIMEOUT,
                format!(
                    "upstream response header timeout after {}s",
                    UPSTREAM_RESPONSE_HEADER_TIMEOUT.as_secs()
                ),
            );
        }
    };

    // 透传响应
    let status = upstream_resp.status();
    let resp_headers = upstream_resp.headers().clone();
    tracing::info!(url = %req.url, status = status.as_u16(), elapsed_ms = start.elapsed().as_millis(), "forward: upstream responded");

    // 构建 axum 响应
    let mut axum_headers = HeaderMap::new();
    for (name, value) in resp_headers.iter() {
        let name_lower = name.as_str().to_lowercase();
        if HOP_BY_HOP.contains(&name_lower.as_str()) {
            continue;
        }
        axum_headers.append(name.clone(), value.clone());
    }

    // 流式透传 body：reqwest bytes_stream → axum Body::from_stream
    let stream = upstream_resp.bytes_stream();
    let body = Body::from_stream(stream);

    let mut response = Response::new(body);
    *response.status_mut() = status;
    *response.headers_mut() = axum_headers;

    response
}

/// 构建错误响应
fn error_response(status: StatusCode, msg: String) -> Response {
    (
        status,
        Json(ForwardError { error: msg }),
    )
        .into_response()
}

/// 从 headers 中按大小写不敏感方式取首个值，缺失返回 "-"
fn extract_header(headers: &IndexMap<String, Vec<String>>, name: &str) -> String {
    headers
        .iter()
        .find(|(k, _)| k.eq_ignore_ascii_case(name))
        .and_then(|(_, v)| v.first())
        .map(|s| s.as_str())
        .unwrap_or("-")
        .to_string()
}

/// 展开 reqwest 错误的完整 source 链，并附带错误分类标记。
///
/// reqwest::Error 的 Display 只输出顶层信息（如 "error sending request for url (...)"），
/// 真正原因（对端关闭连接/连接被拒/超时/TLS 握手失败/SOCKS 失败等）藏在 source() 链里。
/// 偶发失败必须看到底层原因，才能区分两类问题：
///   - 连接池复用了已被代理/上游关闭的旧连接（多为 request/body 类，提示连接被关闭）
///   - 代理本身不稳定（connect/timeout 类）
/// 因此这里展开整条链，并把 reqwest 的错误分类（connect/timeout/request/body/decode）一并打出。
fn describe_reqwest_error(e: &reqwest::Error) -> String {
    let mut chain = e.to_string();
    let mut source = std::error::Error::source(e);
    while let Some(err) = source {
        chain.push_str(" -> ");
        chain.push_str(&err.to_string());
        source = err.source();
    }

    let mut kinds = Vec::new();
    if e.is_connect() {
        kinds.push("connect");
    }
    if e.is_timeout() {
        kinds.push("timeout");
    }
    if e.is_request() {
        kinds.push("request");
    }
    if e.is_body() {
        kinds.push("body");
    }
    if e.is_decode() {
        kinds.push("decode");
    }

    if kinds.is_empty() {
        chain
    } else {
        format!("{chain} [kind={}]", kinds.join(","))
    }
}
