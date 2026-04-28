# MTR Agent/Server

使用 Go 编写的分布式网络诊断服务。

## 组件

- `cmd/server`：云端 REST API、gRPC Agent 控制平面、PostgreSQL 存储、策略与限流。
- `cmd/agent`：边缘侧 Worker，支持两种模式：`grpc` 通过长连接控制平面连接 Server，`http` 暴露 HTTP 调用端点，便于网关转发或函数计算部署。

## 快速开始

无需 PostgreSQL 的本地测试方式：

```sh
go run ./cmd/server -config configs/server.sqlite.yaml
```

Server 默认优先读取 `/etc/mtr/server.yaml`，不存在时读取
`configs/server.yaml`。可使用 `-config` 指定其他配置文件。

首次启动时，Server 会自动创建 SQLite 数据库文件；对于 PostgreSQL，
如果配置账号有权限，Server 会尝试创建目标数据库和所需数据表。

Agent 同样通过 `-config` 读取配置：

```sh
./mtr-agent -config /etc/mtr/agent.yaml
```

两个二进制都支持 `-version`。可通过 Go ldflags 注入构建元数据，例如：

```sh
go build -ldflags "-X github.com/ztelliot/mtr/internal/version.Version=v1.2.3 -X github.com/ztelliot/mtr/internal/version.Commit=$(git rev-parse --short HEAD)" ./cmd/server
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
注册 token、HTTP token、能力列表、协议、TLS 和测速限制等。
`protocols` 是一个位掩码：`1` 表示 IPv4，`2` 表示 IPv6，`3` 表示同时支持两者。
任务可将 `ip_version` 设置为 `4` 或 `6`；任务只会被分发给协议掩码支持目标协议的 Agent。

Server 和 Agent 的每个配置字段也都可以通过环境变量提供。环境变量使用 YAML 路径，
加上 `MTR_` 前缀并转换为大写下划线格式，例如 `tls.ca_file` 对应
`MTR_TLS_CA_FILE`，`runtime.http_timeout_sec` 对应 `MTR_RUNTIME_HTTP_TIMEOUT_SEC`，
`speedtest.max_bytes` 对应 `MTR_SPEEDTEST_MAX_BYTES`。当配置文件和环境变量同时设置同一字段时，
配置文件优先。字符串列表可以使用逗号分隔，也可以使用 YAML/JSON 数组；`outbound_agents`、
`tool_policies` 和 `rate_limit.tools` 等 map/list 字段可使用 YAML/JSON 片段提供。

Server 的调度控制位于服务端配置的 `scheduler` 下：`agent_offline_after_sec`
用于将长时间未上报的 gRPC Agent 标记为离线，`grpc_max_inflight_per_agent` 控制单个
gRPC Agent 可并发运行的任务数量，`outbound_max_inflight_per_agent` 控制 HTTP outbound
Agent 的并发任务数量。
运行时参数位于 `runtime` 下，包括探测 `count`、`max_hops`、
每跳 `probe_step_timeout_sec`、工具超时预设、DNS 解析超时、HTTP outbound 调用重试次数，
以及 outbound 恢复健康检查退避时间。在 Server 或 Agent 配置中设置 `log_level: debug`
可开启更详细的调度和执行日志。

Server 也可以主动调用配置在 `outbound_agents` 下的 HTTP Agent。每个 outbound Agent
需要配置 `id`、`base_url` 和 `http_token`。当 Agent 配置 `mode: http` 或
`mode: grpc,http` 时，会暴露 `/invoke` 端点；Server 在启动时会探测一次 `/healthz`，
用于获取版本、地域、服务商、能力、协议和脱敏设置，之后只会在连接失败后的恢复过程中再次使用 `/healthz`。

HTTP Agent 配置示例：

```sh
mode: "http"
id: "edge-fc-1"
http_addr: ":9000"
http_token: "change-me-http-token"
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
跳点、指标事件，或最终汇总事件。Server outbound 模式只恢复面向浏览器路由和回放所需的外层字段，
例如 `job_id` 和 `agent_id`；它不会把 `tool`、`target` 或协议等重复的任务级字段重新添加到增量事件或汇总事件中。
gRPC 模式在 Agent 到 Server 的路径上也采用同样的紧凑思路。对于阿里云 FC、腾讯云 SCF
或华为 FunctionGraph，可将 `Dockerfile.agent` 作为自定义容器或 HTTP 函数部署，并将触发器流量路由到
`9000` 端口的 `/invoke`；镜像中运行的仍然是统一的 `mtr-agent` 二进制。

Agent 测速端点：

```sh
curl -o /dev/null 'http://localhost:9000/speedtest/random?bytes=10485760&token=change-me-http-token'
```

测速端点只在 HTTP 模式启用时可用，监听在 `http_addr` 上。它由 Agent 的 `http_token` 保护，
token 通过查询参数 `token` 传入。可在 Agent 配置的 `speedtest` 下配置限制；设置
`speedtest.max_bytes: 0` 可禁用该端点。

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
  -d '{"name":"cf-ping","tool":"ping","target":"1.1.1.1","interval_seconds":60}'
```

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
  "apiToken": "frontend-token"
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

前端镜像使用 Caddy 在 `80` 端口提供 Vite 构建产物。

工作台可以创建 `ping`、`traceroute`、`mtr`、`http` 和 `dns` 任务，列出 Agent，
并从 `/v1/jobs/<job-id>/stream` 流式接收结构化任务事件。`traceroute` 和 `mtr`
需要显式选择 Agent，因为服务端要求这些工具提供 `agent_id`。

## Agent/Server mTLS 证书

`register_token` 用于 Agent 注册鉴权；`tls.ca_file`、`tls.cert_file`、
`tls.key_file` 用于保护 Agent 和 Server 的 gRPC 控制面。要启用双向 TLS，请：

- Server 同时设置 `tls.ca_file`、`tls.cert_file`、`tls.key_file`。只设置服务端证书而不设置 `ca_file` 时，链路是 TLS，但不会校验 Agent 客户端证书。
- Agent 同时设置 `tls.ca_file`、`tls.cert_file`、`tls.key_file`。只设置 `ca_file` 时，Agent 只校验 Server 身份，不会向 Server 出示客户端证书。
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
  ca_file: "/var/run/mtr/tls/ca.crt"
  cert_file: "/var/run/mtr/tls/tls.crt"
  key_file: "/var/run/mtr/tls/tls.key"
```

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
  -e MTR_ID=edge-docker-1 \
  -v /etc/mtr/agent.yaml:/etc/mtr/agent.yaml:ro \
  -v /etc/mtr/tls/agent:/var/run/mtr/tls:ro \
  ghcr.io/ztelliot/mtr-agent:latest
```

如果 Server 使用 SQLite，需要额外挂载可写状态目录，并把 `database_url` 指向该目录中的文件：

```sh
  -v /var/lib/mtr:/var/lib/mtr \
  --workdir /var/lib/mtr
```

Agent 默认以 gRPC 长连接主动连 Server，不需要暴露端口。只有在 `mode: http` 或
`mode: grpc,http` 且确实需要外部调用 `/invoke` 时，才为 Agent 添加 `-p 9000:9000`，
并应放在受控网关之后。除非明确需要宿主机网络视角，否则不要使用 `--network host`。

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

仓库提供了一套基础清单，位于 `deploy/`：

- `server.yaml`：单副本 Server `Deployment` 与 `Service`。
- `agent.yaml`：`DaemonSet` 形式的 Agent，每个 Pod 自动用自身 Pod 名作为 `MTR_ID`。
- `networkpolicy.yaml`：拒绝进入 Agent Pod 的入站流量。
- `secrets.example.yaml`：需要替换占位符后再应用的 Secret 模板。
- `kustomization.yaml`：便于 `kubectl apply -k deploy`。

这套清单默认假设：

- PostgreSQL 由外部服务提供，`database-url` 通过 Secret 注入。
- gRPC 控制面走 `mtr-server.mtr.svc.cluster.local:8443`，因此 Server 证书 SAN 已按这个域名示例生成。
- 所有 Agent 共享同一张客户端证书，但每个 Pod 通过 `MTR_ID=metadata.name` 保持唯一逻辑身份。
- Server 保持 `replicas: 1`。当前调度 Hub 维护的是进程内连接状态，因此这份基础清单不直接做多副本控制面。

创建 Secret 时，可直接编辑 `deploy/secrets.example.yaml` 中的占位符，也可以用命令行生成：

```sh
kubectl apply -f deploy/namespace.yaml

kubectl -n mtr create secret generic mtr-server-env \
  --from-literal=database-url='postgres://mtr:mtr@postgres:5432/mtr?sslmode=disable' \
  --from-literal=register-token='change-me-agent-token' \
  --from-file=api-tokens.yaml=<(cat <<'EOF'
- secret: frontend-token
  agents: ["*"]
  schedule_access: "read"
  tools:
    ping:
      allowed_args:
        protocol: "^(icmp)$"
    mtr:
      allowed_args:
        protocol: "^(icmp)$"
    http:
      allowed_args:
        method: "^(HEAD)$"
    dns: {}
- secret: developer-token
  all: true
EOF
)

kubectl -n mtr create secret generic mtr-server-tls \
  --from-file=ca.crt=certs/ca.crt \
  --from-file=tls.crt=certs/server.crt \
  --from-file=tls.key=certs/server.key

kubectl -n mtr create secret generic mtr-agent-env \
  --from-literal=register-token='change-me-agent-token'

kubectl -n mtr create secret generic mtr-agent-tls \
  --from-file=ca.crt=certs/ca.crt \
  --from-file=tls.crt=certs/agent-shared.crt \
  --from-file=tls.key=certs/agent-shared.key
```

然后应用基础清单：

```sh
kubectl apply -k deploy
```

Agent 的权限在清单里被刻意压到很窄：

- 不挂载 ServiceAccount Token：`automountServiceAccountToken: false`
- 非 root 运行，不使用 host network/PID/IPC，`privileged: false`，`allowPrivilegeEscalation: false`
- 根文件系统只读，只额外挂载只读配置、只读证书和一个 `emptyDir` 类型的 `/tmp`
- 默认丢弃全部 Linux capabilities，只额外保留 `NET_RAW`
- 附带 NetworkPolicy 拒绝进入 Agent Pod 的入站流量

之所以保留 `NET_RAW`，是因为当前 `ping`、`traceroute`、`mtr` 的 ICMP 实现需要原始 socket。除此之外不再授予额外能力，也没有附带任何 RBAC 规则。若你的集群启用了严格的 Pod Security Admission，需要为 Agent 所在命名空间显式处理 `NET_RAW` 这一项例外。

这套清单没有附带“默认拒绝 egress”的标准 `NetworkPolicy`。原因是 Agent 的核心职责就是对任意外部目标发起探测，而标准 Kubernetes `NetworkPolicy` 对 ICMP 的表达能力有限；如果你的 CNI 支持更细粒度的 ICMP/egress 规则，建议在该能力之上进一步收紧。

## 致谢

感谢 OpenAI Codex 在项目开发、重构、测试与文档整理过程中提供协作支持。
