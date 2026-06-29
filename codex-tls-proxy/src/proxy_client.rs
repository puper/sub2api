// reqwest 客户端构建与缓存
//
// 按 proxy_url 缓存 reqwest::Client，避免每次请求都重建 TLS 连接池。
// TLS 栈对齐 codex CLI：rustls-tls-native-roots。

use std::collections::HashMap;
use std::time::{Duration, Instant};

use reqwest::Client;
use tokio::sync::Mutex;

/// 客户端缓存条目
struct CachedClient {
    client: Client,
    last_used: Instant,
}

/// 按 proxy_url 缓存 reqwest 客户端
///
/// proxy_url 为空字符串时表示直连，用同一个 key "" 缓存。
/// 空闲超过 TTL 的客户端在下次插入时被淘汰。
pub struct ProxyClientPool {
    clients: Mutex<HashMap<String, CachedClient>>,
    idle_ttl: Duration,
    max_entries: usize,
}

impl ProxyClientPool {
    pub fn new() -> Self {
        Self {
            clients: Mutex::new(HashMap::new()),
            idle_ttl: Duration::from_secs(900), // 15 分钟，对齐 sub2api WS 代理客户端 TTL
            max_entries: 256,
        }
    }

    /// 获取或创建指定 proxy_url 对应的 reqwest 客户端
    pub async fn get(&self, proxy_url: &str) -> Result<Client, reqwest::Error> {
        let now = Instant::now();
        let key = proxy_url.to_string();

        // 快速路径：读锁检查缓存命中
        {
            let clients = self.clients.lock().await;
            if let Some(entry) = clients.get(&key) {
                return Ok(entry.client.clone());
            }
        }

        // 慢路径：构建新客户端
        let client = build_client(proxy_url)?;

        let mut clients = self.clients.lock().await;
        // 双检：可能在等锁期间被其他请求插入
        if let Some(entry) = clients.get(&key) {
            return Ok(entry.client.clone());
        }

        // 淘汰空闲客户端
        self.evict_idle_locked(&mut clients, now);

        // 容量限制：淘汰最久未用
        while clients.len() >= self.max_entries {
            self.evict_oldest_locked(&mut clients);
        }

        clients.insert(
            key,
            CachedClient {
                client: client.clone(),
                last_used: now,
            },
        );

        Ok(client)
    }

    /// 淘汰超过 idle_ttl 的客户端
    fn evict_idle_locked(&self, clients: &mut HashMap<String, CachedClient>, now: Instant) {
        clients.retain(|_, entry| now.duration_since(entry.last_used) < self.idle_ttl);
    }

    /// 淘汰最久未用的客户端
    fn evict_oldest_locked(&self, clients: &mut HashMap<String, CachedClient>) {
        if let Some(oldest_key) = clients
            .iter()
            .min_by_key(|(_, entry)| entry.last_used)
            .map(|(k, _)| k.clone())
        {
            clients.remove(&oldest_key);
        }
    }
}

impl Default for ProxyClientPool {
    fn default() -> Self {
        Self::new()
    }
}

/// 构建 reqwest 客户端
///
/// TLS 栈：rustls-tls-native-roots（与 codex CLI 一致）
/// proxy_url 为空时直连，非空时设置 reqwest 代理
///
/// 超时策略：
/// - connect_timeout: 30s（TCP + TLS 握手）
/// - 不设 .timeout()（总超时会截断 SSE 流式响应）
/// - 响应头超时由 handler 层用 tokio::time::timeout 包裹 send() 实现
fn build_client(proxy_url: &str) -> Result<Client, reqwest::Error> {
    let mut builder = Client::builder()
        .pool_idle_timeout(Duration::from_secs(90))
        .connect_timeout(Duration::from_secs(30))
        .user_agent("codex-tls-proxy/0.1.0");

    if !proxy_url.is_empty() {
        builder = builder.proxy(reqwest::Proxy::all(proxy_url)?);
    }

    builder.build()
}
