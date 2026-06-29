# =============================================================================
# Codex TLS Proxy Multi-Stage Dockerfile
# =============================================================================
# 独立 Rust 服务，用 reqwest + native-tls 栈复刻 Codex CLI 默认路径的 TLS 指纹。
# sub2api 将 OpenAI 相关请求转发给本代理，由本代理通过账号配置的 proxy 发出。
#
# codex CLI 默认 OpenAI 请求走 native-tls（macOS=Secure Transport, Linux=OpenSSL,
# Windows=SChannel），本代理对齐该默认路径。在 Alpine Linux 容器中 native-tls
# 使用 OpenSSL。
#
# 构建（在 codex-tls-proxy/ 目录内执行）：
#   cd codex-tls-proxy
#   docker build -t codex-tls-proxy:latest .
#
# 或在项目根目录执行：
#   docker build -t codex-tls-proxy:latest -f codex-tls-proxy/Dockerfile codex-tls-proxy/
#
# 运行：
#   docker run -d --name codex-tls-proxy -p 127.0.0.1:18900:18900 codex-tls-proxy:latest
#
# 环境变量：
#   CODEX_TLS_PROXY_ADDR=0.0.0.0:18900  监听地址（容器内需用 0.0.0.0）
# =============================================================================

ARG RUST_IMAGE=rust:1.88-alpine
ARG ALPINE_IMAGE=alpine:3.21

# -----------------------------------------------------------------------------
# Stage 1: Build
# -----------------------------------------------------------------------------
FROM ${RUST_IMAGE} AS builder

# 国内构建可通过 --build-arg 覆盖
ARG RUSTUP_DIST_SERVER=https://rsproxy.cn
ARG RUSTUP_UPDATE_ROOT=https://rsproxy.cn/rustup

ENV RUSTUP_DIST_SERVER=${RUSTUP_DIST_SERVER}
ENV RUSTUP_UPDATE_ROOT=${RUSTUP_UPDATE_ROOT}

# native-tls 在 Alpine/musl 上需要 OpenSSL 开发库
RUN apk add --no-cache openssl-dev openssl-libs-static

WORKDIR /build

# 先复制 Cargo.toml 利用 Docker 层缓存
COPY Cargo.toml ./Cargo.toml

# 创建空的 src 目录，预编译依赖（利用层缓存）
# 预编译后删除 src 和 target 中编译产物，但保留下载的依赖缓存
RUN mkdir -p src && \
    echo 'fn main() {}' > src/main.rs && \
    echo '' > src/lib.rs && \
    cargo build --release 2>/dev/null || true && \
    rm -rf src target/release/codex-tls-proxy target/release/deps/codex_tls_proxy* target/release/.fingerprint/codex*

# 复制源码并编译
COPY src ./src

# tests 目录可选：存在则复制，不存在则跳过
COPY tests ./tests

RUN cargo build --release --bin codex-tls-proxy && \
    strip target/release/codex-tls-proxy

# -----------------------------------------------------------------------------
# Stage 2: Runtime
# -----------------------------------------------------------------------------
FROM ${ALPINE_IMAGE}

LABEL maintainer="sub2api"
LABEL description="Codex TLS fingerprint proxy (reqwest + native-tls)"

# 安装运行时依赖：ca-certificates 用于 TLS 验证，openssl 用于 native-tls 后端
RUN apk add --no-cache ca-certificates openssl-libs tzdata && \
    rm -rf /var/cache/apk/*

# 创建非 root 用户
RUN addgroup -g 1000 codex && \
    adduser -u 1000 -G codex -s /bin/sh -D codex

WORKDIR /app

# 复制二进制
COPY --from=builder --chown=codex:codex /build/target/release/codex-tls-proxy /app/codex-tls-proxy

# 监听端口
EXPOSE 18900

# 健康检查
# 用 127.0.0.1 而非 localhost，避免容器内 localhost 解析到 IPv6 ::1 导致连接拒绝
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
    CMD wget -q -O /dev/null http://127.0.0.1:18900/health || exit 1

USER codex

ENTRYPOINT ["/app/codex-tls-proxy"]
