# Go 构建环境坑点(本机 sub2api/backend)

## Trigger

在 `backend/` 下运行任何 `go`(`go build` / `go test` / `go run` / 甚至 `go version`)时报:

```
go: golang.org/toolchain@v0.0.1-go1.26.4.darwin-arm64: verifying module: checksum database disabled by GOSUMDB=off
```

原因:
- `backend/go.mod` 要求 `go 1.26.4`。
- 本机默认 `go` 为 `go1.26.1`(低于要求)。
- `GOTOOLCHAIN=auto`(默认)想下载 `go1.26.4`,但 `GOSUMDB=off` 导致无法校验下载的 toolchain 模块,直接报错退出。
- `GOTOOLCHAIN=local` 又因 `1.26.1 < 1.26.4` 被拒:`go.mod requires go >= 1.26.4 (running go 1.26.1; GOTOOLCHAIN=local)`。

## Action

目标 toolchain 其实已缓存在本机,直接调用缓存里的 1.26.4 二进制并强制 `GOTOOLCHAIN=local`(避免再触发切换/下载):

```bash
export TC=/Users/puper/go/pkg/mod/golang.org/toolchain@v0.0.1-go1.26.4.darwin-arm64/bin/go
export GOTOOLCHAIN=local
cd backend
"$TC" build ./...
"$TC" test ./internal/repository/... -run "CodexTLSProxy"
# wire 重生成(在 cmd/server 下):
cd cmd/server && "$TC" run -mod=mod github.com/google/wire/cmd/wire
```

若缓存路径不存在,先确认 `go env GOMODCACHE`(本机 `/Users/puper/go/pkg/mod`)下
`golang.org/toolchain@v0.0.1-go1.26.4.darwin-arm64/bin/go` 是否还在;
不在则需联网恢复 `GOSUMDB`(如 `GOSUMDB=sum.golang.org`)让 `GOTOOLCHAIN=auto` 重新下载。

## Context

- 已验证:用上述缓存二进制成功 `go build ./...` 与多个 `go test`、并成功重生成 `wire_gen.go`。
- 不要为了绕过而擅自下调 `go.mod` 的 `go 1.26.4` 版本指令。
- `go env`(本机):`GOPROXY=https://proxy.golang.org,direct`,`GOSUMDB=off`,`GOMODCACHE=/Users/puper/go/pkg/mod`。

## 更新时间

2026-06-29
