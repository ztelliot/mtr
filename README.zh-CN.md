# MTR Agent/Server

用于从托管边缘 Agent 发起 `ping`、`traceroute`、`mtr`、HTTP、DNS 和 TCP
端口探测的分布式网络诊断服务。

## 组件

- `cmd/server`：REST API、gRPC Agent 控制平面、存储、策略、调度与限流。
- `cmd/agent`：边缘侧 Worker。`grpc` 模式通过长连接控制面连接 Server；
  `http` 模式暴露调用端点，便于网关转发或 FaaS 平台部署。

## 快速开始

本地 SQLite 测试方式：

```sh
go run ./cmd/server -config configs/server.sqlite.yaml
```

Server 默认优先读取 `/etc/mtr/server.yaml`，不存在时读取
`configs/server.yaml`。可使用 `-config` 指定其他配置文件。

首次启动时，Server 会自动创建 SQLite 数据库。使用 PostgreSQL 时，如果配置账号有权限，
Server 也会尝试创建目标数据库和所需表结构。

Agent 同样通过 `-config` 读取配置：

```sh
./mtr-agent -config /etc/mtr/agent.yaml
```

两个二进制都支持 `-version`。可通过 Go ldflags 注入构建元数据，例如：

```sh
CGO_ENABLED=0 go build -trimpath -ldflags "-X github.com/ztelliot/mtr/internal/version.Version=v1.2.3 -X github.com/ztelliot/mtr/internal/version.Commit=$(git rev-parse --short HEAD) -X github.com/ztelliot/mtr/internal/version.BuiltAt=$(date -u +%Y-%m-%dT%H:%M:%SZ)" ./cmd/server
```

Docker 镜像也支持通过构建参数注入同样的元数据：

```sh
docker build -f Dockerfile.server -t mtr-server:v1.2.3 \
  --build-arg VERSION=v1.2.3 \
  --build-arg COMMIT=$(git rev-parse --short HEAD) \
  --build-arg BUILT_AT=$(date -u +%Y-%m-%dT%H:%M:%SZ) .

docker build -f Dockerfile.agent -t mtr-agent:v1.2.3 \
  --build-arg VERSION=v1.2.3 \
  --build-arg COMMIT=$(git rev-parse --short HEAD) \
  --build-arg BUILT_AT=$(date -u +%Y-%m-%dT%H:%M:%SZ) .
```

Agent 运行时配置位于 Agent YAML 中，包括 `mode`、`http_addr`、身份信息、
注册 token、HTTP token、能力列表、协议、TLS 和测速限制。
`protocols` 是一个位掩码：`1` 表示 IPv4，`2` 表示 IPv6，`3` 表示同时支持两者。
任务可将 `ip_version` 设置为 `4` 或 `6`；任务只会被分发给协议掩码支持目标协议的 Agent。

Server 进程配置和 Agent 配置字段也都可以通过环境变量提供。环境变量使用 YAML 路径，
加上 `MTR_` 前缀并转换为大写下划线格式，例如 `tls.ca_files` 对应
`MTR_TLS_CA_FILES`，`speedtest.max_bytes` 对应 `MTR_SPEEDTEST_MAX_BYTES`。
当配置文件和环境变量同时设置同一字段时，配置文件优先。字符串列表可以使用逗号分隔，
也可以使用 YAML/JSON 数组。
当 Server 位于反向代理后面时，将 `trusted_proxies` 设置为允许提供标准代理 header 的
代理 IP 或 CIDR；否则 `X-Forwarded-For` 和 `X-Real-IP` 不会用于日志和限流。
`client_ip_headers` 是一个自定义单 IP header 的有序列表，会在代理 header 之前被无条件信任，
例如较长的私有 header 名或 `Eo-Connecting-Ip`。

Server 运行时策略保存在持久化托管配置（managed settings）中，而不是 `server.yaml` 中。
这包括 API token、Agent 注册 token、限流、全局和按标签生效的工具策略、
按标签生效的调度/运行参数、探测次数、超时以及托管 HTTP Agent。节点级调整通过标签完成：
每个节点都会自动带有保留标签 `agent` 和 `id:<agent-id>`，因此全局、分组和单节点规则可以使用同一套标签机制。
空数据库首次启动时，Server 会创建一个 admin API token 并打印到日志。使用这个 token 通过管理页，
或调用 `/v1/manage/tokens`、`/v1/manage/register-tokens`、`/v1/manage/rate-limit`、
`/v1/manage/labels` 和 `/v1/manage/agents` 来创建需要的 token 和策略。
在 Server 或 Agent 配置中设置 `log_level: debug` 可开启更详细的调度和执行日志。

Server 也可以主动调用通过 `/v1/manage/agents` 创建的托管 HTTP Agent；记录中需要
`transport`、`id`、`base_url` 和 `http_token`。当 Agent 的 mode 包含 `http` 时，
会暴露 `/invoke` 端点；HTTP 模式必须配置 `http_token`。可以设置 Agent 的
`http_path_prefix`，把这些 HTTP 端点挂到 `/api` 或 `/v1` 之类的前缀下；托管 Agent
的 `base_url` 也要包含同样的前缀。Server 启动时会探测一次 `/healthz`，获取版本、
地域、服务商、能力和协议；之后只会在连接失败恢复过程中再次探测。HTTP Agent 的监听 TLS
由 Agent 配置里的 `http_tls` 控制；调用侧 TLS 参数保存在托管 HTTP Agent 记录上。
客户端 TLS 校验只检查配置的 CA 链，因此集群内访问地址不需要和证书 SAN 一致。

HTTP Agent 配置示例：

```sh
mode: "http"
id: "edge-fc-1"
http_addr: ":9000"
http_token: "change-me-http-token"
http_path_prefix: ""
http_tls:
  enabled: true
  ca_files:
    - "/var/run/mtr/tls/http-client-ca.crt"
  cert_file: "/var/run/mtr/tls/http-agent.crt"
  key_file: "/var/run/mtr/tls/http-agent.key"
```

如需在同一个进程中同时运行长连接 gRPC Agent 和 HTTP Agent，请使用 `mode: "grpc,http"`。

```sh
curl -N -X POST http://localhost:9000/invoke \
  -H 'Authorization: Bearer change-me-http-token' \
  -H 'Accept: application/x-ndjson' \
  -H 'Content-Type: application/json' \
  -d '{"id":"job-1","tool":"ping","target":"1.1.1.1","ip_version":4}'
```

响应是紧凑的换行分隔 JSON（`application/x-ndjson`）：每一行都是结构化的进度、
跳点、指标事件或最终汇总事件。Server outbound 模式只恢复浏览器路由和回放需要的外层字段，
例如 `job_id` 和 `agent_id`；它不会在增量事件中重复添加 `tool`、`target` 或协议等任务级字段。
gRPC 模式在 Agent 到 Server 的路径上也使用同样的紧凑事件格式。对于阿里云 FC、腾讯云 SCF
或华为 FunctionGraph，可将 `Dockerfile.agent` 作为自定义容器或 HTTP 函数部署，并将触发器流量路由到
`9000` 端口的 `/invoke`；镜像中运行的仍然是统一的 `mtr-agent` 二进制。

Agent 测速端点：

```sh
curl -o /dev/null 'http://localhost:9000/speedtest/random?bytes=10485760&token=change-me-http-token'
```

测速端点只在 HTTP 模式启用时可用，监听在 `http_addr` 上。它由 Agent 的 `http_token` 保护，
token 通过查询参数 `token` 传入。可在 Agent 配置的 `speedtest` 下配置限制；设置
`speedtest.max_bytes: 0` 可禁用该端点。测速端点限流使用直连的远端地址，并忽略
`X-Forwarded-For`。

创建任务：

```sh
curl -X POST http://localhost:8080/v1/jobs \
  -H 'Authorization: Bearer developer-token' \
  -H 'Content-Type: application/json' \
  -d '{"tool":"ping","target":"1.1.1.1"}'
```

`ping`、`traceroute` 和 `mtr` 接受 `args.protocol`，可选值为 `icmp` 或 `tcp`；
`count` 和 `max_hops` 由 Server 运行时配置控制，用户不可直接指定。`dns` 替代旧的
`nslookup` 工具名。`port` 执行原生 TCP 连接探测，并要求提供 `args.port`。默认情况下，
Server 会先解析目标主机名再将任务入队；设置 `resolve_on_agent: true` 可将 DNS 解析和解析后 IP
策略检查延后到 Agent 执行。

创建计划检测任务：

```sh
curl -X POST http://localhost:8080/v1/schedules \
  -H 'Authorization: Bearer developer-token' \
  -H 'Content-Type: application/json' \
  -d '{"name":"cf-ping","tool":"ping","target":"1.1.1.1","schedule_targets":[{"label":"agent","interval_seconds":60}]}'
```

计划任务通过 `schedule_targets` 选择节点。每个 Agent 都会自动获得服务端管理的
`agent` 标签和 `id:<agent-id>` 标签；`agent` 表示全部节点，自定义标签表示节点组，
`id:<agent-id>` 表示单个节点。每个 target 都可以设置自己的 interval。自定义标签可来自
Agent 配置、托管 HTTP Agent 配置或部署生成器。带固定 Agent allowlist 的 API token
也可以使用组标签；服务端会把 allowlist 固化到 schedule target 上，运行时跳过
不在 token scope 内的匹配 Agent。计划任务和任务读取接口有意允许被具备对应读权限的
token 共享读取。

查询计划任务历史：

```sh
curl -H 'Authorization: Bearer developer-token' \
  http://localhost:8080/v1/schedules/<schedule-id>/history
```

为浏览器 UI 流式接收结构化任务事件：

```js
const events = new EventSource('/v1/jobs/<job-id>/stream?access_token=developer-token')
events.addEventListener('hop', (event) => console.log(JSON.parse(event.data)))
events.addEventListener('metric', (event) => console.log(JSON.parse(event.data)))
events.addEventListener('progress', (event) => console.log(JSON.parse(event.data)))
events.addEventListener('parsed', (event) => console.log(JSON.parse(event.data)))
```

## Web 工作台

项目在 `web/` 下包含一个独立的 React 单页应用，用于在浏览器中执行网络诊断。
它使用 Vite、React、TypeScript 和 pnpm。

安装依赖：

```sh
pnpm --dir web install
```

在 `web/public/config.json` 中配置运行时 API 连接：

```json
{
  "apiBaseUrl": "",
  "apiToken": "frontend-token",
  "brand": "QwQ MTR",
  "brandUrl": null
}
```

空的 `apiBaseUrl` 可配合 Vite 开发代理使用。请在两个终端中分别启动 Go Server 和前端：

```sh
go run ./cmd/server -config configs/server.sqlite.yaml
pnpm --dir web dev
```

生产构建：

```sh
pnpm --dir web typecheck
pnpm --dir web test
pnpm --dir web build
```

构建前端容器镜像：

```sh
docker build -f Dockerfile.web -t mtr-web:v1.2.3 --build-arg VERSION=v1.2.3 .
```

传入 `COMMIT` 后，前端页脚会显示和 Server 一致的短提交号：

```sh
docker build -f Dockerfile.web -t mtr-web:v1.2.3 \
  --build-arg VERSION=v1.2.3 \
  --build-arg COMMIT=$(git rev-parse HEAD) .
```

前端镜像使用 Caddy 在 `80` 端口提供 Vite 构建产物。运行时会读取
`/config.json`，因此部署时可以只替换这个文件，而不需要重新构建镜像。
只有当前端公开访问域名也把 `/v1` 反向代理到 Server 时，`apiBaseUrl`
才适合留空。否则请设置为浏览器可以访问到的 Server API 地址：

```json
{
  "apiBaseUrl": "https://mtr-api.example.com",
  "apiToken": "frontend-token",
  "brand": "QwQ MTR",
  "brandUrl": "https://mtr.example.com"
}
```

不配置 `brandUrl` 时，页首 brand 保持为应用内首页链接；设置为 URL 时，不同部署地址
都会跳到同一个 canonical brand 地址；设置为 `null` 或 `""` 时则关闭 brand 链接。

Kubernetes 示例包含 `deploy/k8s/web.yaml`，会部署
`ghcr.io/ztelliot/mtr-web:latest`，创建 `mtr-web` Service，并通过
`mtr-web-config` 覆盖容器内的 `/usr/share/caddy/config.json`。本地集群可用
下面的方式快速验证：先按 Kubernetes 部署小节创建 Secrets，再应用基础资源：

```sh
kubectl apply -k deploy/k8s
kubectl -n mtr port-forward svc/mtr-server 8080:8080
kubectl -n mtr port-forward svc/mtr-web 8081:80
```

如果通过 `http://localhost:8081` 访问工作台，请把 `mtr-web-config` 中的
`apiBaseUrl` 设置为 `http://localhost:8080`，并把 `apiToken` 设置为有效的
API token。公开部署时应使用权限受限的前端 token，而不是初始 admin token。
也可以在两个服务前放置 Ingress 或网关，将 `/v1` 路由到 `mtr-server`，将
`/` 路由到 `mtr-web`。

`deploy/k8s/agent.yaml` DaemonSet 可以从 Kubernetes Node annotations 生成每个节点的
Agent 元数据，但这个逻辑只在部署层完成，Agent 二进制本身并不知道 Kubernetes。
示例使用 initContainer 读取当前 Node 的 annotations，把普通的 `agent.yaml` 渲染到
`emptyDir`，主容器再读取这个生成后的配置文件。ServiceAccount 只需要给
initContainer 使用的只读 `get nodes` 权限。initContainer 通过 manifest 中显式配置的
`MTR_KUBERNETES_API_SERVER` 访问 apiserver；如果集群的 Kubernetes Service IP 不是
`https://10.96.0.1:443`，请修改这个环境变量。默认映射这些 annotations：

```sh
kubectl annotate node <node> \
  mtr.ztelliot.dev/country=JP \
  mtr.ztelliot.dev/region='Tokyo East' \
  mtr.ztelliot.dev/provider=kubernetes \
  mtr.ztelliot.dev/isp=example-net \
  mtr.ztelliot.dev/protocols=3 \
  mtr.ztelliot.dev/hide-first-hops=0 \
  mtr.ztelliot.dev/capabilities=ping,traceroute,mtr,http,dns,port
```

如果集群已经有自己的 annotation 命名，可修改 `mtr-agent-config` 里的
`render-agent-config.sh`。计划任务标签由服务端管理，Agent 不再上报标签。
`protocols` 使用和 Agent 配置相同的位掩码：`1` 表示 IPv4，`2` 表示 IPv6，
`3` 表示同时支持两者。`capabilities` 是逗号分隔的工具列表。缺少 annotation 时，
initContainer 会把 fallback 值写入生成后的配置文件。

工作台可以创建 `ping`、`traceroute`、`mtr`、`http`、`dns` 和 `port` 任务，
列出 Agent，并从 `/v1/jobs/<job-id>/stream` 流式接收结构化任务事件。临时 `traceroute` 和
`mtr` 任务需要显式选择 Agent，因为服务端要求这些工具提供 `agent_id`；计划任务则使用
`schedule_targets` 标签选择节点，而不是单个 `agent_id`。

## Agent/Server mTLS 证书

`register_token` 用于 Agent 注册鉴权；`tls.ca_files`、`tls.cert_file`、
`tls.key_file` 用于保护 Agent 和 Server 的 gRPC 控制面。要启用双向 TLS，请：

- Agent 可以只设置 `tls.enabled: true` 且不设置 `ca_files`，此时会使用系统信任根证书。这适合连接 Cloudflare 反代后的公网 TLS 域名。
- Server 同时设置 `tls.ca_files`、`tls.cert_file`、`tls.key_file`。只设置服务端证书而不设置 `ca_files` 时，链路是 TLS，但不会校验 Agent 客户端证书。
- Agent 同时设置 `tls.ca_files`、`tls.cert_file`、`tls.key_file`。只设置 `ca_files` 时，Agent 只校验 Server 身份，不会向 Server 出示客户端证书。
- `register_token` 和 mTLS 建议同时启用：前者用于逻辑注册鉴权，后者用于传输层双向认证。

当前实现基于 CA 校验客户端证书链，不把单张客户端证书绑定到某个固定 Agent ID，因此多个 Agent 可以共享同一张客户端证书；Kubernetes 示例清单也默认这样做。共享证书时，仍应给每个 Agent 分配唯一 `id`，并接受“单个 Agent 泄露后需要整体轮换共享证书”的运维代价。

使用 OpenSSL 创建一套 CA、Server 证书和可被所有 Agent 共享的客户端证书：

```sh
mkdir -p certs

openssl genrsa -out certs/ca.key 4096
openssl req -x509 -new -nodes -key certs/ca.key -sha256 -days 3650 \
  -out certs/ca.crt -subj "/CN=mtr-ca"

cat > certs/server.ext <<'EOF'
subjectAltName=DNS:mtr-server,DNS:mtr-server.mtr.svc,DNS:mtr-server.mtr.svc.cluster.local
extendedKeyUsage=serverAuth
EOF

openssl genrsa -out certs/server.key 4096
openssl req -new -key certs/server.key -out certs/server.csr \
  -subj "/CN=mtr-server.mtr.svc.cluster.local"
openssl x509 -req -in certs/server.csr -CA certs/ca.crt -CAkey certs/ca.key \
  -CAcreateserial -out certs/server.crt -days 825 -sha256 -extfile certs/server.ext

cat > certs/agent.ext <<'EOF'
extendedKeyUsage=clientAuth
EOF

openssl genrsa -out certs/agent-shared.key 4096
openssl req -new -key certs/agent-shared.key -out certs/agent-shared.csr \
  -subj "/CN=mtr-agent-shared"
openssl x509 -req -in certs/agent-shared.csr -CA certs/ca.crt -CAkey certs/ca.key \
  -CAcreateserial -out certs/agent-shared.crt -days 825 -sha256 -extfile certs/agent.ext
```

如果 Agent 不是通过 `mtr-server.mtr.svc.cluster.local:8443` 访问 Server，而是使用其他域名或 IP，请把对应 DNS 名称或 `IP:` 项加入 `subjectAltName`。

Server 与 Agent 的 TLS 配置示例：

```yaml
tls:
  enabled: true
  ca_files:
    - "/var/run/mtr/tls/ca.crt"
  cert_file: "/var/run/mtr/tls/tls.crt"
  key_file: "/var/run/mtr/tls/tls.key"
```

### Cloudflare gRPC 反向代理

gRPC 控制面可以放在 [Cloudflare 的 gRPC 代理](https://developers.cloudflare.com/network/grpc-connections/) 后面，但需要满足 Cloudflare 的要求：在 zone 里启用 gRPC、使用已代理的 hostname、SSL/TLS 模式至少为 Full，并让源站 gRPC 端点监听 443 端口且支持 TLS、HTTP/2 和 ALPN。本项目使用 grpc-go 的 HTTP/2 gRPC，并发送 `application/grpc+json`，符合 Cloudflare 接受的 gRPC content-type 形式。

一种常见配置如下：

```yaml
# 源站 server.yaml
grpc_addr: ":443"
tls:
  enabled: true
  cert_file: "/var/run/mtr/tls/tls.crt"
  key_file: "/var/run/mtr/tls/tls.key"

# agent.yaml
server_addr: "grpc.example.com:443"
tls:
  enabled: true
```

如果要在同一个源站同时支持直连 Agent 和 Cloudflare 反代 Agent，可以启用 Cloudflare Authenticated Origin Pulls；更严格的做法是使用 zone-level 或 per-hostname 自定义证书，避免只证明“来自 Cloudflare 网络”。Server 同时信任直连 Agent CA 和 Cloudflare origin-pull CA：

```yaml
# 源站 server.yaml
grpc_addr: ":443"
tls:
  enabled: true
  ca_files:
    - "/var/run/mtr/tls/agent-ca.crt"
    - "/var/run/mtr/tls/cloudflare-origin-pull-ca.crt"
  cert_file: "/var/run/mtr/tls/tls.crt"
  key_file: "/var/run/mtr/tls/tls.key"
```

直连 Agent 保留自己的客户端证书配置并连接源站地址。走 Cloudflare 的 Agent 连接代理域名，通常不设置 Agent `cert_file`/`key_file`；此时由 Cloudflare 向 Server 出示 origin-pull 客户端证书。因为 Cloudflare 会终止 Agent 侧 TLS，普通 Agent 客户端证书不会穿透反向代理，所以 `register_token` 仍然要足够强。Cloudflare Access 不保护这种反代模式下的 gRPC 流量，敏感源站需要额外鉴权。Cloudflare Tunnel public hostname 也不是这里说的 proxied gRPC；[Cloudflare 文档](https://developers.cloudflare.com/cloudflare-one/networks/connectors/cloudflare-tunnel/use-cases/grpc/) 里的 Tunnel gRPC 支持走的是 private subnet routing，不是 public hostname。

## 安全说明

生产部署应在 REST API 前放置 Anubis 或其他面向浏览器的网关，用于 PoW/挑战处理；同时在 Server
和 Agent 配置中设置 mTLS 证书路径，定期轮换 API 与 Agent token，并在暴露服务前调优各工具策略。

Agent 使用原生 Go 实现执行诊断，只上传结构化结果事件。

解析到 loopback、private、link-local、multicast、运营商级 NAT、文档地址、基准测试地址或其他特殊用途地址段的目标，
会同时被 Server 和 Agent 拒绝。

Agent 镜像只需要编译后的 Agent 二进制和 CA 证书；诊断工具均由 Go 实现。

## Docker 部署加固

容器运行时建议使用非 root 用户、只读根文件系统、禁用提权并显式收敛 Linux capabilities。
Server 不需要额外 capability；Agent 的 ICMP `ping`、`traceroute`、`mtr` 需要 `NET_RAW`。

示例中的 `65532:65532` 是容器内非特权 UID/GID。挂载到容器里的配置和证书文件需要对这个
UID/GID 可读；私钥建议使用 `0640` 并让 group 为 `65532`，或使用 Docker/Compose secret。
发布的容器镜像不内置 Server 或 Agent 运行时配置，请显式挂载这些文件。

```sh
docker network create mtr-net

docker run -d --name mtr-server \
  --network mtr-net \
  --user 65532:65532 \
  --cap-drop ALL \
  --security-opt no-new-privileges:true \
  --read-only \
  --tmpfs /tmp:rw,noexec,nosuid,nodev,size=64m \
  --pids-limit 256 \
  --cpus 1 \
  --memory 512m \
  -p 8080:8080 \
  -p 8443:8443 \
  -v /etc/mtr/server.yaml:/etc/mtr/server.yaml:ro \
  -v /etc/mtr/tls/server:/var/run/mtr/tls:ro \
  ghcr.io/ztelliot/mtr-server:latest

docker run -d --name mtr-agent \
  --network mtr-net \
  --user 65532:65532 \
  --cap-drop ALL \
  --cap-add NET_RAW \
  --security-opt no-new-privileges:true \
  --read-only \
  --tmpfs /tmp:rw,noexec,nosuid,nodev,size=64m \
  --pids-limit 256 \
  --cpus 1 \
  --memory 512m \
  -v /etc/mtr/agent.yaml:/etc/mtr/agent.yaml:ro \
  -v /etc/mtr/tls/agent:/var/run/mtr/tls:ro \
  ghcr.io/ztelliot/mtr-agent:latest
```

如果 Server 使用 SQLite，需要额外挂载可写状态目录，并把 `database_url` 指向该目录中的文件：

```sh
  -v /var/lib/mtr:/var/lib/mtr \
  --workdir /var/lib/mtr
```

Agent 默认以 gRPC 长连接主动连 Server，不需要暴露端口。请在 `/etc/mtr/agent.yaml`
中设置 Agent 唯一 `id`、`server_addr` 和 `register_token`；配置文件值会优先于
`MTR_` 环境变量。只有在 `mode: http` 或 `mode: grpc,http` 且确实需要外部调用
`/invoke` 时，才为 Agent 添加 `-p 9000:9000`，并应放在受控网关之后。除非明确需要
宿主机网络视角，否则不要使用 `--network host`。

## systemd 部署加固

仓库中的 `systemd/mtr-server.service` 和 `systemd/mtr-agent.service` 已包含基础沙盒配置。
两者都以 `User=mtr`/`Group=mtr` 运行，启用只读系统目录、私有 `/tmp`、禁止提权、限制地址族和
系统调用过滤。Server 的 capability 集为空；Agent 只通过 `CapabilityBoundingSet` 和
`AmbientCapabilities` 保留 `CAP_NET_RAW`。

安装时建议创建专用系统用户，并让配置和证书只对 root 与 `mtr` 组可读：

```sh
useradd --system --home /var/lib/mtr --shell /usr/sbin/nologin mtr
install -d -o mtr -g mtr -m 0750 /var/lib/mtr
install -d -o root -g mtr -m 0750 /etc/mtr /etc/mtr/tls
install -o root -g mtr -m 0640 configs/server.yaml /etc/mtr/server.yaml
install -o root -g mtr -m 0640 configs/agent.yaml /etc/mtr/agent.yaml
install -o root -g mtr -m 0644 certs/ca.crt /etc/mtr/tls/ca.crt
install -o root -g mtr -m 0640 certs/server.crt /etc/mtr/tls/server.crt
install -o root -g mtr -m 0640 certs/server.key /etc/mtr/tls/server.key
install -o root -g mtr -m 0640 certs/agent-shared.crt /etc/mtr/tls/agent.crt
install -o root -g mtr -m 0640 certs/agent-shared.key /etc/mtr/tls/agent.key
install -o root -g root -m 0755 mtr-server /usr/local/bin/mtr-server
install -o root -g root -m 0755 mtr-agent /usr/local/bin/mtr-agent
install -o root -g root -m 0644 systemd/mtr-server.service /etc/systemd/system/mtr-server.service
install -o root -g root -m 0644 systemd/mtr-agent.service /etc/systemd/system/mtr-agent.service
systemctl daemon-reload
systemctl enable --now mtr-server
systemctl enable --now mtr-agent
```

若 Server 使用 SQLite，建议把数据库放在 `/var/lib/mtr/`。服务文件已设置
`WorkingDirectory=/var/lib/mtr` 和 `StateDirectory=mtr`，该目录会作为唯一持久可写状态目录使用。

可以用下面的命令查看 systemd 对服务沙盒的评分和剩余风险：

```sh
systemd-analyze security mtr-server.service
systemd-analyze security mtr-agent.service
```

## Kubernetes 部署

仓库提供了一套基础清单，位于 `deploy/k8s/`：

- `server.yaml`：单副本 Server `Deployment` 与 `Service`。
- `agent.yaml`：`DaemonSet` 形式的 Agent，每个 Pod 自动用自身 Pod 名作为 `MTR_ID`。
- `networkpolicy.yaml`：拒绝进入 Agent Pod 的入站流量。
- `secrets.example.yaml`：需要替换占位符后再应用的 Secret 模板。
- `kustomization.yaml`：用于 `kubectl apply -k deploy/k8s` 的基础资源；它有意不包含
  `secrets.example.yaml`。

这套清单默认假设：

- PostgreSQL 由外部服务提供，`database-url` 通过 Secret 注入。
- gRPC 控制面走 `mtr-server.mtr.svc.cluster.local:8443`，因此 Server 证书 SAN 已按这个域名示例生成。
- 所有 Agent 共享同一张客户端证书，但每个 Pod 通过 `MTR_ID=metadata.name` 保持唯一逻辑身份。
- Agent Pod 从 `mtr-agent-env` Secret 读取注册 token，且该值必须匹配 Server
  托管配置中保存的 Agent 注册 token。
- Server 保持 `replicas: 1`。当前调度 Hub 维护的是进程内连接状态，因此这份基础清单不直接做多副本控制面。
- API token、Agent 注册 token、限流、全局配置、按标签生效的调度/运行参数、
  标签策略和托管 HTTP Agent 都存储在 Server 托管配置中，不再从 Kubernetes
  Server ConfigMap 或 Secret 读取。

创建 Secret 时，可以先编辑 `deploy/k8s/secrets.example.yaml` 中的占位符并单独应用该文件，
也可以用命令行生成：

```sh
kubectl apply -f deploy/k8s/namespace.yaml

kubectl -n mtr create secret generic mtr-server-env \
  --from-literal=database-url='postgres://mtr:mtr@postgres:5432/mtr?sslmode=disable'

kubectl -n mtr create secret generic mtr-server-tls \
  --from-file=ca.crt=certs/ca.crt \
  --from-file=tls.crt=certs/server.crt \
  --from-file=tls.key=certs/server.key

kubectl -n mtr create secret generic mtr-agent-env \
  --from-literal=register-token='<managed-register-token>'

kubectl -n mtr create secret generic mtr-agent-tls \
  --from-file=ca.crt=certs/ca.crt \
  --from-file=tls.crt=certs/agent-shared.crt \
  --from-file=tls.key=certs/agent-shared.key
```

`<managed-register-token>` 是管理页或 `/v1/manage/register-tokens` 返回的 token
值。空数据库首次启动时，Server 会在日志中打印初始 admin API token；使用该 token
创建 Agent 注册 token 后，再把返回值写入 `mtr-agent-env`。

然后应用基础清单：

```sh
kubectl apply -k deploy/k8s
```

Agent 的权限在清单里被刻意压到很窄：

- 主 Agent 容器不挂载 ServiceAccount Token
- 不使用 host network/PID/IPC，`privileged: false`
- 根文件系统只读，只额外挂载只读配置、只读证书和一个 `emptyDir` 类型的 `/tmp`
- 默认丢弃全部 Linux capabilities，只额外保留 `NET_RAW`
- 附带 NetworkPolicy 拒绝进入 Agent Pod 的入站流量

之所以保留 `NET_RAW`，是因为当前 `ping`、`traceroute`、`mtr` 的 ICMP 实现需要原始 socket。Agent 主容器使用 UID 0 运行，以便 `NET_RAW` 能进入 permitted/effective capability sets；但它不是 privileged，且仍然丢弃除 `NET_RAW` 之外的全部 capabilities。若你的集群启用了严格的 Pod Security Admission，需要为这个 Agent securityContext 显式放行。Agent 启动时会打印 `CapEff`、`CapBnd`、`NoNewPrivs` 和 `Seccomp`，便于确认实际权限。

DaemonSet 示例额外包含一条很窄的 RBAC：initContainer 需要 `get nodes`，用于从 Node annotations 渲染每个节点的配置。生成后的配置写入 `emptyDir`，主 Agent 容器随后在不挂载 ServiceAccount Token 的情况下运行。

这套清单没有附带“默认拒绝 egress”的标准 `NetworkPolicy`。原因是 Agent 的核心职责就是对任意外部目标发起探测，而标准 Kubernetes `NetworkPolicy` 对 ICMP 的表达能力有限；如果你的 CNI 支持更细粒度的 ICMP/egress 规则，建议在该能力之上进一步收紧。

## FaaS 部署

FaaS 部署文件位于 `deploy/fc/`。仓库提交的 `targets.json` 只描述可公开的目标元数据
和占位运行参数；`generate.mjs` 会根据它生成各云厂商需要的部署模板。

目前支持：

- `aliyun`：阿里云 Function Compute，通过 Serverless Devs 的 `fc3` 组件部署。
- `ctyun`：天翼云函数服务，通过 Serverless Devs 的 `faas-cf` 组件部署。
- `qcloud`：腾讯云 SCF，通过 Serverless Cloud Framework 部署。
- `gcloud`：Google Cloud Run，通过 `gcloud alpha run deploy` 部署。

`targets.json` 是提交到仓库里的模板，只放占位符和可公开的目标信息。真实部署时，
复制一份本地配置，例如 `.targets.json`，填入真实 token、路径前缀、日志项目 ID
和镜像地址，然后运行：

```bash
cd deploy/fc
node generate.mjs --config .targets.json
```

也可以通过 `FC_TARGETS_FILE` 选择配置文件。当生成器使用默认的
`targets.json` 时，会拒绝看起来像真实 token、路径前缀或阿里云日志项目 ID
的值，避免误把私有配置写进公开模板。

通过 `binary.path` 指向本地已构建的 Agent 二进制，或配置
`binary.fromImage` 从 Docker 镜像中提取。生成内容都会写到
`deploy/fc/output/`：

```bash
output/binary/agent
output/s.aliyun.yaml
output/s.ctyun.yaml
output/qcloud/code/scf_bootstrap
output/qcloud/<target-key>/serverless.yml
output/qcloud/deploy.sh
output/gcloud/deploy.sh
```

FaaS Agent 不读取 `config.yaml`，所有运行参数都通过环境变量注入。每个目标都会得到
`MTR_COUNTRY`、`MTR_ID`、`MTR_ISP`、`TZ` 和 `MTR_REGION`；Agent 运行参数来自
`targets.json` 顶层的 `env`，也可以被 provider 或 target 里的 `env` 覆盖。

从已发布镜像提取 Agent 二进制的示例：

```json
{
  "binary": {
    "fromImage": {
      "image": "ghcr.io/ztelliot/mtr-agent:dev",
      "path": "/usr/local/bin/mtr-agent",
      "platform": "linux/amd64",
      "pull": true
    }
  }
}
```

设置 `binary.fromImage` 后，`generate.mjs` 会执行 `docker pull`，创建临时容器，
把二进制复制出来，并默认写入 `output/binary/agent`。如果已有本地二进制，可用
`binary.path` 指定来源：

```json
{
  "binary": {
    "path": ".cache/mtr-agent",
    "fromImage": {
      "image": "ghcr.io/ztelliot/mtr-agent:dev"
    }
  }
}
```

如果镜像已经在本地构建或加载，可以把 `binary.fromImage.pull` 设为 `false`。

不要把云凭据或真实账号私有值写进文档。access key、日志项目 ID、私有路径前缀、
私有镜像地址等值应放在本地配置或部署密钥中。

阿里云和天翼云模板通过 Serverless Devs 部署：

```bash
cd output
s deploy -t s.aliyun.yaml
s deploy -t s.ctyun.yaml
```

腾讯云 SCF 使用 Serverless Cloud Framework。由于该工具要求每个函数目录里都有独立的
`serverless.yml`，生成器会为 `providers.qcloud.targets` 中的每个目标写一个目录。
共享代码位于 `output/qcloud/code`；Agent 二进制会复制为 `scf_bootstrap`，这是
SCF 期望的入口文件名：

```text
output/qcloud/code/scf_bootstrap
output/qcloud/example/serverless.yml
```

腾讯云目标通过生成的脚本部署；脚本会进入每个目标目录并执行 `scf deploy`：

```bash
cd output
qcloud/deploy.sh
```

Google Cloud Run 也使用生成的脚本部署：

```bash
cd output
gcloud/deploy.sh
```

阿里云日志按 target 单独启用，默认关闭。需要日志时，在本地配置中设置 `log: true`，
并提供 `providers.aliyun.logProjectId`：

```json
{
  "env": {
    "MTR_MODE": "http",
    "MTR_LOG_LEVEL": "info",
    "MTR_HTTP_TOKEN": "<http-token>",
    "MTR_HTTP_ADDR": ":9000",
    "MTR_HTTP_PATH_PREFIX": "/<path-prefix>",
    "MTR_PROTOCOLS": 1,
    "MTR_HIDE_FIRST_HOPS": 0,
    "MTR_CAPABILITIES": "ping,traceroute,mtr,http,dns,port",
    "MTR_SPEEDTEST_MAX_BYTES": 0,
    "MTR_HTTP_TLS_ENABLED": false
  },
  "providers": {
    "aliyun": {
      "name": "mtr-agent-aliyun",
      "output": "s.aliyun.yaml",
      "src": "./binary",
      "access": "Aliyun",
      "logProjectId": "<log-project-id>",
      "targets": [
        {
          "key": "example",
          "region": "<aliyun-region>",
          "functionName": "mob-example",
          "country": "CN",
          "id": "ali.example.fc",
          "label": "Example",
          "log": true
        }
      ]
    }
  }
}
```

未设置 `log` 或值为 false 时，不生成 `logConfig`。启用后，SLS 项目名为
`serverless-{region}-{providers.aliyun.logProjectId}`。可以在 target 上设置
`logStore` 覆盖默认的 `default-logs` logstore。

阿里云 target 默认使用 Go 官方层：

```text
acs:fc:{region}:official:layers/Go1/versions/1
```

如果某个地域没有 Go 层，可把 `layer` 设为 `python-flask`：

```json
{
  "key": "example",
  "region": "<aliyun-region>",
  "functionName": "mob-example",
  "country": "XX",
  "id": "ali.example.fc",
  "label": "Example",
  "layer": "python-flask"
}
```

运行时仍是 `custom.debian10`，只会把生成的 layer ARN 改成：

```text
acs:fc:{region}:official:layers/Python3-Flask2x/versions/2
```

如果要完全省略 `layers` 字段，将 `layer` 设为 `null`、`false` 或空字符串：

```json
{
  "key": "example",
  "region": "<aliyun-region>",
  "functionName": "mob-example",
  "country": "XX",
  "id": "ali.example.fc",
  "label": "Example",
  "layer": null
}
```

天翼云和腾讯云的最小 target 示例：

```json
{
  "key": "example",
  "region": "<ctyun-resource-pool-id>",
  "functionName": "mob-example",
  "country": "CN",
  "id": "cty.example.fc",
  "label": "Example"
}
```

```json
{
  "key": "example",
  "region": "<qcloud-region>",
  "functionName": "mob-example",
  "country": "CN",
  "id": "txc.example.fc",
  "label": "Example"
}
```

Google Cloud Run 基于镜像部署。最小 provider 配置如下：

```json
{
  "providers": {
    "gcloud": {
      "name": "mtr-agent-gcloud",
      "output": "gcloud",
      "image": {
        "imageUrl": "asia-docker.pkg.dev/<project>/<repo>/mtr-agent:dev"
      },
      "env": {
        "MTR_PROTOCOLS": 3
      },
      "targets": [
        {
          "key": "hongkong",
          "region": "asia-east1",
          "functionName": "mob-hongkong",
          "country": "HK",
          "id": "gcp.ap-east-1.fc",
          "label": "Hong Kong"
        }
      ]
    }
  }
}
```

生成器会将该配置展开成 `gcloud alpha run deploy` 命令，包含镜像、地域、函数名、
运行参数和 Agent 环境变量：

```bash
gcloud alpha run deploy mob-hongkong \
  --image=asia-docker.pkg.dev/<project>/<repo>/mtr-agent:dev \
  --allow-unauthenticated \
  --public \
  --port=9000 \
  --concurrency=1 \
  --timeout=60 \
  --cpu=0.08 \
  --memory=128Mi \
  --min-instances=0 \
  --max-instances=4 \
  --set-env-vars=MTR_COUNTRY=HK \
  --set-env-vars=MTR_ID=gcp.ap-east-1.fc \
  --set-env-vars=MTR_ISP=GCP \
  --set-env-vars='MTR_REGION=Hong Kong' \
  --set-env-vars=MTR_MODE=http \
  --set-env-vars=MTR_LOG_LEVEL=info \
  --set-env-vars=MTR_HTTP_ADDR=:9000 \
  --set-env-vars=MTR_HTTP_TOKEN=<http-token> \
  --set-env-vars=MTR_HTTP_PATH_PREFIX=/<path-prefix> \
  --set-env-vars='^#^MTR_CAPABILITIES=ping,traceroute,mtr,http,dns,port' \
  --set-env-vars=MTR_PROTOCOLS=3 \
  --set-env-vars=MTR_HIDE_FIRST_HOPS=0 \
  --set-env-vars=MTR_HTTP_TLS_ENABLED=false \
  --set-env-vars=MTR_SPEEDTEST_MAX_BYTES=0 \
  --no-cpu-boost \
  --region=asia-east1
```

Cloud Run 默认端口为 `9000`，并发为 `1`，超时为 `60`，CPU 为 `0.08`，
内存为 `128Mi`，最小实例数为 `0`，最大实例数为 `4`，同时启用
`--allow-unauthenticated`、`--public` 和 `--no-cpu-boost`。可通过
`providers.gcloud.run` 或 `target.run` 覆盖。

腾讯云 SCF 也可以使用容器镜像部署，而不是上传复制出的二进制包。给
`providers.qcloud.image` 设置值后，所有腾讯云目标都会启用镜像模式：

```json
{
  "providers": {
    "qcloud": {
      "name": "mtr-agent-qcloud",
      "output": "qcloud",
      "image": {
        "sourceImage": "ghcr.io/ztelliot/mtr-agent:latest",
        "imageType": "personal",
        "imageUrl": "{registry}/sls-scf/mtr-agent:latest",
        "containerImageAccelerate": true
      },
      "targets": [
        {
          "key": "guangzhou",
          "region": "ap-guangzhou",
          "functionName": "mob-guangzhou",
          "country": "CN",
          "id": "txc.cn-south-1.fc",
          "label": "Guangzhou"
        }
      ]
    }
  }
}
```

`imageUrl` 支持 `{registry}`、`{key}`、`{region}` 和 `{functionName}` 占位符；
`sourceImage` 支持 `{key}`、`{region}` 和 `{functionName}`。target 可以覆盖
provider 级别的 image 配置，也可以设置 `"image": false` 回退到代码包模式。

`{registry}` 会根据腾讯云地域选择。中国大陆地域默认使用
`ccr.ccs.tencentyun.com`；境外地域使用本地 registry，例如 `ap-hongkong` 使用
`hkccr.ccs.tencentyun.com`，`ap-singapore` 使用
`sgccr.ccs.tencentyun.com`。可通过 `image.registry` 覆盖单个 image 对象，或用
`image.registries` 覆盖指定地域：

```json
{
  "image": {
    "sourceImage": "ghcr.io/ztelliot/mtr-agent:latest",
    "imageUrl": "{registry}/sls-scf/mtr-agent:latest",
    "registries": {
      "ap-hongkong": "hkccr.ccs.tencentyun.com",
      "ap-singapore": "sgccr.ccs.tencentyun.com"
    }
  }
}
```

启用镜像模式后，生成的腾讯云 `serverless.yml` 会包含：

```yaml
image:
  imageType: personal
  imageUrl: ccr.ccs.tencentyun.com/sls-scf/mtr-agent:latest
  containerImageAccelerate: true
```

`output/qcloud/deploy.sh` 会在部署前把源镜像同步到选定的腾讯云 registry：

```bash
docker pull ghcr.io/ztelliot/mtr-agent:latest
docker tag ghcr.io/ztelliot/mtr-agent:latest ccr.ccs.tencentyun.com/sls-scf/mtr-agent:latest
docker push ccr.ccs.tencentyun.com/sls-scf/mtr-agent:latest
```

脚本会按生成后的目标镜像 tag 去重同步。以上配置中，中国大陆目标共享
`ccr.ccs.tencentyun.com/sls-scf/mtr-agent:latest`；像 `ap-singapore` 这样的境外
目标则会使用 `sgccr.ccs.tencentyun.com/sls-scf/mtr-agent:latest`。

## 致谢

感谢 OpenAI Codex 在项目开发、重构、测试与文档整理过程中提供协作支持。
