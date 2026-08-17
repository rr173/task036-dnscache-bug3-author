# dnscache

这是一个纯 Go 的内存类 DNS 记录缓存服务，支持 A、AAAA、CNAME 和 TXT 记录、TTL 严格过期与惰性淘汰、CNAME 链解析、环路和深度保护、NXDOMAIN 负缓存以及名字大小写归一化。HTTP API 提供写入、查询、负缓存、淘汰和统计能力。

## 标准构建、运行和测试

在本目录执行：

```bash
go build ./...                  # 编译全部包
go run . --smoke-test           # 执行无需外部服务的自检并退出
go run . server --addr :8080   # 启动 HTTP 服务
go test ./...                   # 运行单元测试
go vet ./...                    # 执行静态检查
```

服务入口是根目录的 `main.go`。主要接口为 `POST /put`、`POST /lookup`、`POST /mark-nxdomain`、`POST /evict` 和 `POST /stats`。

## Benzhi 镜像

`build_benzhi_docker.sh` 固定使用 `benzhi.Dockerfile`，参数分别是镜像名和平台，默认值为 `my-project` 与 `linux/amd64`。镜像使用 Go 1.26.3，构建阶段执行 `go build ./...`；容器启动后进入 shell。

```bash
bash ./build_benzhi_docker.sh task036-dnscache:amd64 linux/amd64
bash ./build_benzhi_docker.sh task036-dnscache:arm64 linux/arm64
docker run -it task036-dnscache:amd64
```
