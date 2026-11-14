# MTR Agent/Server

用于从托管边缘 Agent 发起 `ping`、`traceroute`、`mtr`、HTTP、DNS 和 TCP
端口探测的分布式网络诊断服务。

## 组件

- `cmd/server`：REST API、gRPC Agent 控制平面、存储、策略、调度与限流。
- `cmd/agent`：边缘侧 Worker。`grpc` 模式通过长连接控制面连接 Server；
  `http` 模式暴露流式调用端点；`event` 模式适配由云平台同步调用的事件函数。

## 快速开始

本地 SQLite 测试方式：

```sh
bootstrap_token_file="$(mktemp)"
chmod 0600 "$bootstrap_token_file"
openssl rand -hex 32 > "$bootstrap_token_file"
MTR_BOOTSTRAP_ADMIN_TOKEN_FILE="$bootstrap_token_file" \
  go run ./cmd/server -config configs/server.sqlite.yaml
```

该文件保存初始 admin API token。先用它登录并创建所需的低权限 API token 和 Agent 注册
token。确认持久化 SQLite 数据库已完成初始化后，停止 Server，把仍需保留的 token 安全存入
密钥系统，删除 bootstrap 文件，再在不设置 `MTR_BOOTSTRAP_ADMIN_TOKEN_FILE` 的情况下重启：

```sh
rm -f "$bootstrap_token_file"
unset bootstrap_token_file
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
注册 token、HTTP 入站鉴权、能力列表、协议、TLS 和测速限制。
`protocols` 是一个位掩码：`1` 表示 IPv4，`2` 表示 IPv6，`3` 表示同时支持两者。
任务可将 `ip_version` 设置为 `4` 或 `6`；任务只会被分发给协议掩码支持目标协议的 Agent。
`protocols` 和 `capabilities` 都必须显式配置且不能为空；`0` 或空列表会直接被拒绝，不会被
扩展成更宽的权限。

Agent 控制协议按整代版本管理，不进行逐 feature 协商。hello、健康响应和每个任务请求都必须
携带 `protocol_version: 1`；UDP 探测和所有已公布的 HTTP 版本都是该协议版本的固定能力。
Server、Agent 和 Web 必须一起升级。

Server 进程配置和 Agent 配置字段也都可以通过环境变量提供。环境变量使用 YAML 路径，
加上 `MTR_` 前缀并转换为大写下划线格式，例如 `tls.ca_files` 对应
`MTR_TLS_CA_FILES`，`speedtest.max_bytes` 对应 `MTR_SPEEDTEST_MAX_BYTES`。
当配置文件和环境变量同时设置同一字段时，配置文件优先。字符串列表可以使用逗号分隔，
也可以使用 YAML/JSON 数组。
未知 YAML 字段、结构化子字段和 `MTR_` 环境变量名都会被拒绝，避免安全配置拼写错误后
静默回落。
当 Server 位于反向代理后面时，将 `trusted_proxies` 设置为允许提供标准代理 header 的
代理 IP 或 CIDR；否则 `X-Forwarded-For`、`X-Real-IP` 和 `client_ip_headers` 都不会用于
日志和限流。`client_ip_headers` 是一个自定义单 IP header 的有序列表，会在标准代理 header
之前检查，但只有直连 peer 命中 `trusted_proxies` 时才生效。除非防火墙或 NetworkPolicy 已保证
只有代理能访问监听端口，否则不要扩大 `trusted_proxies`。

Server 运行时策略保存在持久化托管配置（managed settings）中，而不是 `server.yaml` 中。
这包括 API token、Agent 注册 token、限流、全局和按标签生效的工具策略、
按标签生效的调度/运行参数、探测次数、超时以及出站 Agent。节点级调整通过标签完成：
每个节点都会自动带有保留标签 `agent` 和 `id:<agent-id>`，因此全局、分组和单节点规则可以使用同一套标签机制。
空存储只会使用 `bootstrap_admin_token_file`（或
`MTR_BOOTSTRAP_ADMIN_TOKEN_FILE`）所指文件中的 token 完成初始化。该路径必须是普通文件，
大小不得超过 4 KiB，不得可执行或允许 group 写入，也不能允许 world 用户访问；`0600`
和 `0440` 均可使用。Server
绝不会把 token 写入日志。持久化存储初始化完成后，应删除该文件并取消此配置；内存存储
不保留状态，因此每次启动都需要该文件。通过可信的运维客户端使用这个 token 调用
`/v1/manage/tokens`、`/v1/manage/register-tokens`、`/v1/manage/rate-limit`、
`/v1/manage/labels` 和 `/v1/manage/agents` 来创建需要的 token 和策略。
每个 Agent 注册 token 都有 `remaining_uses`（剩余新节点次数，新 token 默认为 `1`）。
只有首次出现的 Agent ID 创建记录时才会在事务内扣减次数；已有 Agent 重连不扣减，
次数为 0 时仍可重连。Agent 的国家、地区、provider 和 ISP 只允许首次注册时写入；
可通过注册 token 的 `allowed_agent_ids` 限制它只能创建
指定的 Agent ID，空列表表示不限制。白名单校验失败不会扣减次数，已经绑定的 Agent
在白名单变更后仍可重连。
后续连接仍可更新版本、能力列表和 IPv4/IPv6 协议支持。如需变更这些已锁定的身份字段，
请使用新的 Agent ID。
在 Server 或 Agent 配置中设置 `log_level: debug` 可开启更详细的调度和执行日志。

Server 可以主动调用通过 `/v1/manage/agents` 创建的出站 Agent。必填字段 `transport`
选择两种调用契约之一：

- `http`：`url` 是基础 URL。Server 请求 `GET <url>/healthz`，并从
  `POST <url>/invoke` 流式读取 NDJSON。
- `event`：`url` 是云函数的完整同步调用 URL。Server 向同一 URL 发送
  `action: healthz` 或 `action: invoke`，并读取一个完整 JSON 响应。

鉴权与传输类型相互独立。`auth.type` 必须显式选择 `none`、`bearer`、`header` 或
`huaweicloud_aksk`。前三种分别表示无鉴权、静态 Bearer Token 和静态自定义请求头 Token。
`huaweicloud_aksk` 是华为云 FunctionGraph 无人值守调用的原生方式：Server 会根据最终确定的
method、URL、headers 和 JSON body，为每次健康检查与函数调用重新计算
`SDK-HMAC-SHA256` 签名。该类型需要填写 `access_key`、`secret` 和 `project_id`，其中
`secret` 表示 Secret Key，管理接口不会回传。编辑时，只有传输类型、URL、鉴权字段、附加请求头
和 TLS 配置均未改变，Secret Key 留空才会保留原值；该凭据作用域任一部分变化时都必须重新填写。

出站 URL 可以使用 `http://` 或 `https://`；选择明文 HTTP 属于部署方决策，它会让网络路径
看到鉴权请求头和请求内容，因此只能用于经过审慎保护的可信链路。非敏感的服务商参数可放入
独立的 `headers` map；鉴权信息及其他敏感值必须放在 `auth` 中，不应写入 `headers`。
函数结果直接作为 HTTP body 时使用 `event_response: direct`；服务商把函数结果包装在顶层
`result` 对象或 JSON 字符串中时，使用 `result_field`，华为云 FunctionGraph 的同步调用属于
后者。这里有意不接受 API Gateway 的 `statusCode`、`body`、`isBase64Encoded` 包装。

FunctionGraph 也接受静态请求头 `X-Auth-Token` 中的 IAM Token，但它只有 24 小时有效期，
因此这里只适合手工测试，或由外部组件负责刷新。内置 AK/SK 鉴权目前只支持长期 AK/SK，
不支持携带 security token 的临时凭据，也不支持通过 IAM 用户名/密码自动刷新 Token。
华为云会校验动态生成的 `X-Sdk-Date`，因此 Server 必须通过 NTP 保持系统时间准确。详细规则见
[FunctionGraph 鉴权说明](https://support.huaweicloud.com/api-functiongraph/functiongraph_06_0230.html)
和 [API 请求签名算法](https://support.huaweicloud.com/intl/zh-cn/devg-apisign/api-sign-algorithm-001.html)。

出站 Agent 地址属于受信的控制面配置，可以使用公网或私网地址，因此 VPC 或集群内的 Agent
仍可访问。Server 会自行解析并固定每次连接，拒绝跳转和环境代理，并限制所有响应大小。
Agent 端点仍禁止环回、链路本地/metadata、未指定和组播地址。这不会放宽任务目标：用户提交的
探测目标仍始终拒绝私网、环回、链路本地、metadata 及其他
特殊地址。Event 调用始终只尝试一次；HTTP 调用只会重试
发生在取得连接之前的 DNS、dial 或 TLS 错误，请求一旦可能已经开始就绝不重放。`429` 被
视为临时背压，任务会通过原子状态转换重新入队。

Server 启动时探测一次出站 Agent，之后按配置的最大健康间隔刷新元数据；只有故障恢复阶段
才使用指数退避的健康探测。队列轮询与健康探测相互独立，因此不会在每次调度 tick 都计费调用
Event 函数。Agent 监听侧 TLS 由 `http_tls` 控制；出站 TLS 参数保存在出站 Agent 记录中。
未配置 `tls.ca_files` 时，TLS 使用系统根，并按 URL hostname 校验证书 hostname/SAN。显式
配置 `tls.ca_files` 后，Server 改为仅针对该专用信任集合校验证书链、有效期和 ServerAuth
用途，并有意忽略证书 SAN。这里应使用 Agent 专用 CA 或严格受控的共享证书 pin，不能放入
通用信任根。`tls.server_name` 只是可选的 SNI 路由提示，绝不会改变证书身份校验规则。配置
自定义 CA、客户端证书或 SNI 提示时，必须同时设置 `tls.enabled: true` 并使用 `https://` URL。

华为云 FunctionGraph Event Agent 示例：

```json
{
  "id": "edge-event-1",
  "transport": "event",
  "enabled": true,
  "url": "https://functiongraph.cn-north-4.myhuaweicloud.com/v2/<project-id>/fgs/functions/<function-urn>/invocations",
  "auth": {
    "type": "huaweicloud_aksk",
    "access_key": "<access-key>",
    "secret": "<secret-key>",
    "project_id": "<project-id>"
  },
  "headers": {"X-CFF-Request-Version": "v1"},
  "event_response": "result_field",
  "labels": ["apac"]
}
```

当 Agent 的 mode 包含 `http` 时会暴露流式调用端点。HTTP 模式必须显式启用应用层鉴权：
将 `http_auth.type` 设置为 `bearer` 或 `header`，并提供非空的 `secret`。可以设置
`http_path_prefix`，把端点挂到 `/api` 或 `/v1` 之类的前缀下，并在出站 Agent 的 `url`
中包含相同前缀。

HTTP Agent 配置示例：

```yaml
mode: "http"
id: "edge-fc-1"
http_addr: ":9000"
http_auth:
  type: "bearer"
  header: ""
  secret: "change-me-http-auth-secret"
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
  -H 'Authorization: Bearer change-me-http-auth-secret' \
  -H 'Accept: application/x-ndjson' \
  -H 'Content-Type: application/json' \
  -d '{"protocol_version":1,"id":"job-1","tool":"ping","target":"1.1.1.1","ip_version":4}'
```

若服务商或网关使用静态自定义请求头，需要让 Agent 与 Server 出站配置使用完全相同的请求头名
和 secret。例如 Agent 可以配置为：

```yaml
http_auth:
  type: "header"
  header: "X-MTR-Agent-Token"
  secret: "change-me-shared-secret"
```

对应的 Server 出站 Agent 记录应包含
`"auth":{"type":"header","header":"X-MTR-Agent-Token","secret":"change-me-shared-secret"}`。
若选择 `bearer`，Server 会改为发送 `Authorization: Bearer <secret>`。Server 也允许在明文
HTTP 上使用这两种鉴权形式，但这会把凭据和请求内容暴露给网络路径；除非链路确实可信，默认
仍应使用 HTTPS。

响应是紧凑的换行分隔 JSON（`application/x-ndjson`）：每一行都是结构化的进度、
跳点、指标事件或最终汇总事件。Server outbound 模式只恢复浏览器路由和回放需要的外层字段，
例如 `job_id` 和 `agent_id`；它不会在增量事件中重复添加 `tool`、`target` 或协议等任务级字段。
gRPC 模式在 Agent 到 Server 的路径上也使用同样的紧凑事件格式。

### 事件函数模式

当云函数平台以单次请求调用 Agent，并同步等待完整结果时，可以使用 `event` 模式。该模式默认监听
`:8000`，`POST /invoke` 会一次性返回一个 JSON 文档而不是 NDJSON 流。它还提供轻量且无需鉴权的
`POST /init`，返回 `{"status":"ok"}`，供启用了容器初始化调用的平台使用；没有这一生命周期的
平台可以忽略。事件模式必须显式配置 `http_auth.type`：只有在函数平台的调用 API 或 IAM 边界已经
鉴权每个请求时才使用 `none`。`bearer` 要求 `Authorization: Bearer <secret>`；`header` 要求配置的
请求头和 secret。两种鉴权形式都会覆盖每个 `POST /invoke` 请求，包括 `healthz` action。事件模式
不支持路径前缀和监听 TLS。

`configs/agent.event.yaml` 提供了一份完整示例：

```yaml
mode: "event"
id: "edge-event-1"
country: "CN"
region: "edge"
provider: "faas"
http_addr: ":8000"
http_auth:
  type: "none" # 仅在调用入口已有鉴权时使用
  header: ""
  secret: ""
http_path_prefix: ""
protocols: 1
capabilities: [ping, traceroute, mtr, http, dns, port]
http_tls:
  enabled: false
speedtest:
  max_bytes: 0
```

下面的 curl 示例假设端点未对外暴露，并使用 `http_auth.type: none`。需要纵深防护时，可以设置
`MTR_HTTP_AUTH_TYPE=bearer` 与 `MTR_HTTP_AUTH_SECRET=<secret>`；若选择 `header`，还要设置
`MTR_HTTP_AUTH_HEADER`。健康检查和任务请求都必须添加相匹配的请求头。鉴权发生在 event action
分派之前，因此 `healthz` 无法绕过鉴权。

发送 `action: healthz` 可以查看 Agent 信息和运行环境的网络权限：

```sh
curl -X POST http://localhost:8000/invoke \
  -H 'Content-Type: application/json' \
  -d '{"protocol_version":1,"action":"healthz"}'
```

任务请求沿用现有 `JobSpec` 字段，并且必须设置 `action: invoke`：

```sh
curl -X POST http://localhost:8000/invoke \
  -H 'Content-Type: application/json' \
  -d '{"protocol_version":1,"action":"invoke","id":"job-1","tool":"ping","target":"1.1.1.1","ip_version":4,"args":{"protocol":"udp"},"timeout_seconds":20}'
```

任务结束后，函数会一次性返回一个 `application/json` 文档。过程事件放在 `events`，最终汇总放在
`result`：

```json
{
  "job_id": "job-1",
  "events": [
    {"type": "target_resolved", "metric": {"target_ip": "1.1.1.1", "ip_version": 4}},
    {"type": "metric", "metric": {"seq": 1, "latency_ms": 27.1}}
  ],
  "result": {
    "type": "summary",
    "exit_code": 0,
    "metric": {"protocol": "udp", "port": 33434, "packets_transmitted": 1, "packets_received": 1}
  }
}
```

`http` 和 `event` 的 `/invoke` 协议不同，因此不能在同一进程中同时启用。

#### 容器镜像

`Dockerfile.agent` 构建的同一个镜像支持 `grpc`、`http` 和 `event` 三种模式。镜像只包含 Agent
二进制和 CA 证书；运行参数可以来自挂载的 YAML、`MTR_*` 环境变量，或两者组合。默认配置文件
不是必需的。Agent 和 Server 的 scratch 镜像默认使用非特权 UID/GID `65532:65532`。

下面的命令会构建镜像，并以 event 模式在本地启动：

```sh
docker buildx build \
  --platform linux/amd64 \
  --load \
  -f Dockerfile.agent \
  -t mtr-agent:latest .

docker run --rm -p 127.0.0.1:8000:8000 \
  -e MTR_MODE=event \
  -e MTR_HTTP_AUTH_TYPE=none \
  -e MTR_HTTP_ADDR=:8000 \
  -e MTR_ID=edge-event-1 \
  -e MTR_COUNTRY=CN \
  -e MTR_REGION=edge \
  -e MTR_PROVIDER=faas \
  -e MTR_PROTOCOLS=1 \
  -e MTR_CAPABILITIES=ping,traceroute,mtr,http,dns,port \
  mtr-agent:latest
```

部署时，需要让平台把调用请求发送到 8000 端口的 `POST /invoke`。同一个监听器还提供轻量的
`POST /init`，启用独立容器初始化调用的平台无需再增加适配层；不使用初始化流程的平台可以忽略。
函数超时应大于任务的 `timeout_seconds`，并为冷启动留出余量。平台在容器外添加的响应封装，应由
调用方或 Server 适配器处理。

推送镜像时，把 `--load` 换成 `--push`，并替换成目标仓库的镜像标签。新版 Buildx 可能会把
provenance 或 SBOM attestation 作为 image index 中的额外 manifest 一并推送。若旧仓库报错无法
解析 manifest，可以在同一个单架构构建命令中重试添加 `--provenance=false --sbom=false`。这两个
兼容开关会移除相应的供应链元数据，不应作为默认选项，也不能解决架构不匹配或所有仓库端 manifest
错误。构建架构应与函数运行环境一致，通常是 `linux/amd64` 或 `linux/arm64`。

#### 网络权限

云函数运行时通常会指定容器 UID/GID，并限制 Linux capability。Dockerfile 无法补回已经从
bounding set 移除的权限；强行给二进制写入 `cap_net_raw` 还可能导致程序无法启动。实际权限应以
`healthz` 返回的 `network.cap_net_raw` 为准。

没有 `NET_RAW` 时，ping 可以使用 `udp`、`tcp` 或 `auto`。traceroute/MTR 应改用
`udp`；ICMP 探测和需要发现中间跳的 TCP 探测都需要 `NET_RAW`。

UDP ping 会发送真实 UDP 数据报；收到 UDP 回复或 ICMP Port Unreachable 都判定为目标可达。
默认目标端口为 `33434`，也可通过 `args.port` 指定。防火墙可能静默丢弃 UDP，因此显式 `udp`
绝不降级；只有显式传入 `protocol: auto` 才会依次尝试 ICMP、UDP、TCP，并在第一个探测成功后将
本次任务固定到该协议。结果 summary 始终包含实际使用的 `protocol`；auto 结果还会包含
`requested_protocol: auto`。

`healthz` 还会返回 `network.ping_group_range`，以及内核是否允许 IPv4/IPv6 无特权 ICMP Echo
datagram socket。

例如在普通 Docker 中测试 ICMP 模式时，可以让 Agent 以 root 启动，同时丢弃除 `NET_RAW` 外的
全部 capability：

```sh
docker run --rm -p 8000:8000 \
  --user 0:0 \
  --cap-drop ALL \
  --cap-add NET_RAW \
  --security-opt no-new-privileges:true \
  -e MTR_ID=edge-docker-1 \
  -e MTR_MODE=event \
  -e MTR_HTTP_AUTH_TYPE=none \
  -e MTR_HTTP_ADDR=:8000 \
  -e MTR_PROTOCOLS=1 \
  -e MTR_CAPABILITIES=ping,traceroute,mtr,http,dns,port \
  mtr-agent:latest
```

如果运行时强制使用非 root 用户，`--cap-add NET_RAW` 仍可能无法进入 permitted/effective 集合；
应以 `healthz` 的结果为准。

Agent 测速端点：

```sh
curl -H 'Authorization: Bearer change-me-http-auth-secret' -o /dev/null 'http://localhost:9000/speedtest/random?bytes=10485760'
```

测速端点只在 HTTP 模式启用时可用，监听在 `http_addr` 上。它由 Agent 的 `http_auth` 保护，
使用已配置的 Bearer 或自定义请求头。可在 Agent 配置的 `speedtest` 下配置限制；设置
`speedtest.max_bytes: 0` 可禁用该端点。测速端点限流使用直连的远端地址，并忽略
`X-Forwarded-For`。

创建任务：

```sh
curl -X POST http://localhost:8080/v1/jobs \
  -H 'Authorization: Bearer developer-token' \
  -H 'Content-Type: application/json' \
  -d '{"tool":"ping","target":"1.1.1.1"}'
```

`ping`、`traceroute` 和 `mtr` 的 `args.protocol` 均可选 `icmp`、`udp`、`tcp` 或 `auto`。
默认严格使用 `icmp`；显式选择 `auto` 时，依次尝试 `icmp`、`udp`、`tcp`，并锁定第一个有响应的
协议。UDP 默认端口为 `33434`，TCP 默认端口为 `80`，均可通过 `args.port` 覆盖。UDP 路由探测
使用 Linux socket error queue，无需 raw socket；目前 UDP 路由探测只支持 Linux。
TCP 路由探测会从 raw ICMP Time Exceeded 中关联被引用的 TCP 流，并把连接成功或目标返回
RST 都视为到达。该实现目前仅支持 Linux，收集中间跳时需要 `NET_RAW`。
`http` 的 `args.http_version` 可选 `1.1`、`2`、`3` 或 `auto`。默认严格使用 HTTP/1.1；选择
`auto` 时依次尝试 HTTP/3、HTTP/2、HTTP/1.1，且只会在尚未收到响应时回落。HTTP/2 和 HTTP/3
要求 HTTPS，HTTP/3 还要求允许出站 UDP。结果中的 `protocol` 或 `http_version` 表示实际协议；
请求 `auto` 时还会返回 `requested_protocol` 或 `requested_http_version`。
`count` 和 `max_hops` 由 Server 运行时配置控制，用户不可直接指定。`dns` 替代旧的
`nslookup` 工具名。`port` 执行原生 TCP 连接探测，并要求提供 `args.port`。默认情况下，
Server 会先解析目标主机名再将任务入队；设置 `resolve_on_agent: true` 可将 DNS 解析和解析后 IP
策略检查延后到 Agent 执行。Fan-out 模式的 `ping`、`dns`、`port` 和 `http` 任务可通过
`agent_tags` 只选择匹配任一服务端托管标签的 Agent；不传时仍使用全部符合条件的 Agent。
标签过滤会与 API token 的节点范围，以及在线状态、工具能力和逐 Agent 策略校验取交集。
公开提交的 `agent_tags` 不得为 `agent`，也不得包含 `:` 或 `-`；这些形式保留给服务端
管理标签，API 会直接拒绝。

创建计划检测任务：

```sh
curl -X POST http://localhost:8080/v1/schedules \
  -H 'Authorization: Bearer developer-token' \
  -H 'Content-Type: application/json' \
  -d '{"name":"cf-ping","tool":"ping","target":"1.1.1.1","cron_expression":"*/5 * * * *","schedule_targets":[{"label":"agent"}]}'
```

计划任务通过 `schedule_targets` 选择节点。每个 Agent 都会自动获得服务端管理的
`agent` 标签和 `id:<agent-id>` 标签；`agent` 表示全部节点，自定义标签表示节点组，
`id:<agent-id>` 表示单个节点。`cron_expression` 使用标准 5 段格式
`分 时 日 月 周`，统一按 UTC 解释；每个触发点会同时运行全部所选节点。自定义标签可来自
Agent 配置、出站 Agent 配置或部署生成器。服务端会把创建者的 Agent scope
以选择规则保存到 schedule target；每次触发时都会根据当前节点重新计算标签和无限制范围，
因此后续新增的匹配节点也会运行，同时固定 allowlist 和 deny 规则仍然生效。计划任务和任务
读取接口有意允许被具备对应读权限的 token 共享读取。

查询计划任务历史：

```sh
curl -H 'Authorization: Bearer developer-token' \
  http://localhost:8080/v1/schedules/<schedule-id>/history
```

为浏览器 UI 流式接收结构化任务事件：

```js
const response = await fetch('/v1/jobs/<job-id>/stream', {
  headers: {Authorization: 'Bearer developer-token', Accept: 'text/event-stream'}
})
for await (const chunk of response.body.pipeThrough(new TextDecoderStream())) {
  console.log(chunk) // 按标准 SSE frame 增量解析
}
```

API token 只允许通过 Bearer header 提交。原生 `EventSource` 无法附加该 header，因此 Web
工作台使用带鉴权 header 的 `fetch` 流式读取，token 不会再进入 stream URL。

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
  "apiToken": "REPLACE_WITH_PUBLIC_API_TOKEN",
  "brand": "QwQ MTR",
  "brandUrl": null
}
```

空的 `apiBaseUrl` 可配合 Vite 开发代理使用。打开工作台前，应把
`REPLACE_WITH_PUBLIC_API_TOKEN` 替换成 Server 签发的公共诊断 token。新 Server 需先按
“快速开始”中的 bootstrap token 流程初始化（已初始化的存储不再需要该文件），然后在两个终端中
分别启动 Server 和前端：

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

前端镜像使用 UID/GID `65532:65532` 运行 Caddy，并在非特权端口 `8080` 提供 Vite 构建产物。
运行时会读取 `/config.json`，因此部署时可以只替换这个文件，而不需要重新构建镜像。
镜像会把同源 `/v1` 请求反向代理到 `MTR_SERVER_URL`，其默认值是
`http://mtr-server:8080`。推荐使用这种布局并把 `apiBaseUrl` 留空；也可以把它设置为浏览器
能访问且已配置相应跨域策略的 Server API 地址：

```json
{
  "apiBaseUrl": "https://mtr-api.example.com",
  "apiToken": "REPLACE_WITH_PUBLIC_API_TOKEN",
  "brand": "QwQ MTR",
  "brandUrl": "https://mtr.example.com"
}
```

不配置 `brandUrl` 时，页首 brand 保持为应用内首页链接；设置为 URL 时，不同部署地址
都会跳到同一个 canonical brand 地址；设置为 `null` 或 `""` 时则关闭 brand 链接。

Web 客户端会从 `config.json` 读取 `apiToken`，并自动在 API 请求和流式请求中发送
`Authorization: Bearer` 请求头，因此访客不需要输入 token。这个值是公开客户端凭据：任何浏览器
用户都能从 `/config.json` 或开发者工具中读取它。它只能拥有公开诊断所需的最小工具和 Agent
范围，并设置 `all: false`、`schedule_access: none`、`manage_access: none`。绝不能在这里配置
bootstrap/admin token、管理 token 或 schedule-write token。部署时必须假设公共 token 会被复制和
滥用；真正的安全边界是 Server 的权限校验和限流。

容器部署时，应在仓库外准备这份 JSON，并覆盖镜像内的默认文件；该文件需要允许 UID `65532`
读取：

```sh
docker run --rm -p 8081:8080 \
  --mount type=bind,src=/absolute/path/config.json,dst=/usr/share/caddy/config.json,readonly \
  ghcr.io/ztelliot/mtr-web:0.1.0
```

Kubernetes 示例包含 `deploy/k8s/web.yaml`，会部署
`ghcr.io/ztelliot/mtr-web:0.1.0`，创建 `mtr-web` Service，并通过
`mtr-web-config` 覆盖容器内的 `/usr/share/caddy/config.json`。这里的 `0.1.0`
是发布目标，并不表示公开 registry 当前一定已有这些镜像；请先发布镜像，或把本地构建的镜像
加载到集群节点。公开 Web 前，必须把 ConfigMap 中的 `REPLACE_WITH_PUBLIC_API_TOKEN` 替换成
公共诊断 token。这里有意使用 ConfigMap 而不是 Secret，因为浏览器必须取得该值，无法替它保密。
本地集群可按下面的方式快速验证：随后按 Kubernetes 部署小节创建 Secrets，再应用基础资源：

```sh
kubectl apply -k deploy/k8s
kubectl -n mtr port-forward svc/mtr-web 8081:80
```

如果通过 `http://localhost:8081` 访问工作台，请把 `apiBaseUrl` 留空；Web Pod 会把 `/v1`
代理到集群内的 `mtr-server` Service，客户端会自动使用预置的公共 token。如果 Server 的
集群内地址不同，请覆盖 Web 容器的 `MTR_SERVER_URL`。

基线 Caddy 会用直连 peer 地址覆盖 `X-Mtr-Proxy-Client-Ip`；Server 之所以信任这个 header，
是因为 `mtr-server-ingress` 只允许 Web Pod 访问 HTTP 端口。该信任模型仅在集群 CNI 确实支持并
执行 NetworkPolicy 时才安全；如果 CNI 不执行策略，部署前必须把 `0.0.0.0/0` 和 `::/0`
替换为真实 Web Pod CIDR 或其他严格受限的代理范围。如果增加 Ingress、集群外 Agent 或其他
Server 客户端，必须同时调整 NetworkPolicy 和可信代理链；失去网络隔离后，不能继续使用基线中的
宽泛 trusted-proxy 设置。Ingress 也不是直接添加即可：基线策略会阻止 controller 直连 Server，
Caddy 默认只从它的直连 peer 生成客户端 header。要在 Ingress 后保留终端用户 IP，应先把 Caddy
配置成只信任该 controller 的转发 header，再扩展 NetworkPolicy。

`deploy/k8s/agent.yaml` DaemonSet 可以从 Kubernetes Node annotations 生成每个节点的
Agent 元数据，但这个逻辑只在部署层完成，Agent 二进制本身并不知道 Kubernetes。
示例使用 initContainer 读取当前 Node 的 annotations，把普通的 `agent.yaml` 渲染到
`emptyDir`，主容器再读取这个生成后的配置文件。ServiceAccount 只需要给
initContainer 使用的只读 `get nodes` 权限。默认情况下，initContainer 使用 Kubernetes
自动注入的 `KUBERNETES_SERVICE_HOST` 和 `KUBERNETES_SERVICE_PORT_HTTPS` 组成
apiserver 地址；只有集群确实需要其他地址时，才设置可选的
`MTR_KUBERNETES_API_SERVER` 覆盖值。默认映射这些 annotations：

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
`mtr` 任务需要显式选择 Agent，因为服务端要求这些工具提供 `agent_id`。Fan-out 工具可按
服务端托管的 Agent 标签选择节点集；不选择表示全部可用节点，选择多个标签时按 OR 语义合并。
计划任务则使用
`schedule_targets` 标签选择节点，而不是单个 `agent_id`。

## HTTP 访问日志

Server 会在每个 HTTP 请求结束后输出一条结构化 JSON 访问日志。覆盖公开接口、正常鉴权
API、鉴权拒绝、限流请求、未匹配路由、`/metrics`；长连接和流式请求会在连接结束时记录。
日志消息为 `http access`，主要字段包括：

- `request_id`、方法、路由模板、转义后的路径、状态码和耗时；
- 实际读取的请求字节数、写出的响应字节数、声明的 Content-Length，以及请求/响应
  Content-Type；
- 解析后的客户端 IP、远端地址、Host、协议、User-Agent、鉴权结果和哈希后的 API token
  主体；
- `panicked` 标记，便于发现由 HTTP Server 在路由外层恢复的异常。

Server 会接受安全的 `X-Request-ID` 并在响应头中返回；缺失或格式不安全时自动生成 UUID。
访问日志有意不记录查询字符串，因为任意业务参数都可能包含敏感值。将 `log_level` 设置为
`info` 或 `debug` 即可输出访问日志。

## Prometheus Watch 指标

Server 在 `/metrics` 暴露 Prometheus 指标，用于把 Watch 的历史展示和告警迁移到
Prometheus/Grafana。每次抓取会导出每个计划任务当前 revision、每个 Agent 最新一次
**已完成**的结果；正在执行的下一轮任务不会暂时覆盖上一次完成结果。指标还包括 Agent
在线/启用状态、计划任务的 Crontab 配置和节点标签。

同一个 `/metrics` 接口还会导出 HTTP 访问统计：

- `mtr_http_requests_total`：按规范化方法、路由模板、状态码和鉴权结果统计请求量；
- `mtr_http_request_duration_seconds`、`mtr_http_request_size_bytes`、
  `mtr_http_response_size_bytes`：按方法和路由统计耗时及请求/响应体积直方图；
- `mtr_http_requests_in_flight`：按方法统计当前并发请求数。

未知方法统一标记为 `OTHER`，未匹配路由统一标记为 `unmatched`，不会把原始动态路径变成
无界的 Prometheus 时序。同时会导出标准 `go_*` Go Runtime 指标和 `process_*` 进程指标。

当 Server 配置了 API token 时，Prometheus 请求需要携带具有
`schedule_access: read`（或 `write`）权限的 token。示例抓取配置：

```yaml
scrape_configs:
  - job_name: mtr-watch
    metrics_path: /metrics
    scheme: https
    authorization:
      type: Bearer
      credentials_file: /etc/prometheus/mtr-watch.token
    static_configs:
      - targets: [mtr-server.example:8080]
```

`scrape_interval` 应足以覆盖 Watch 的 Crontab 运行频率；Exporter 暴露的是最新完成状态，
如果两次抓取之间完成了多轮任务，Prometheus 不会补录中间轮次的瞬时值。

主要指标包括：

- 通用结果：`mtr_watch_result_success`、`mtr_watch_result_timestamp_seconds`、
  `mtr_watch_result_duration_seconds`。
- Ping：丢包比例、收发包数和 RTT；HTTP：分阶段耗时、状态码和下载量；
  DNS：记录数量；Port：端口状态和连接耗时。
- Traceroute/MTR：逐跳 RTT、丢包、探测数，以及完整路径信息。

Prometheus 样本值只能是数字，文本通过值恒为 `1` 的 info 指标保存：DNS 使用
`mtr_watch_dns_record_info`，完整路由使用 `mtr_watch_route_path_info`，逐跳地址使用
`mtr_watch_route_hop_info`。文本标签同时附带稳定哈希；单项超过 2048 个字符时会截断并将
`truncated` 设为 `true`。Exporter 不会使用每次变化的 `job_id` 作为标签。

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

需要建立网络连接的探测如果解析到 loopback、private、link-local、multicast、运营商级 NAT、
文档地址、基准测试地址或其他特殊用途地址段，会同时被 Server 和 Agent 拒绝。`dns` 工具有意采用
不同语义：它只查询指定记录，并不连接返回地址，因此可以看到 Agent 所用解析器可见的私有记录。
给 token 开放 DNS 工具就等于允许访问该解析器的命名空间；公开 token 应据此收敛权限。

Agent 镜像只需要编译后的 Agent 二进制和 CA 证书；诊断工具均由 Go 实现。

## Docker 部署加固

容器运行时建议优先使用非 root 用户、只读根文件系统、禁用提权并显式收敛 Linux capabilities。
Server 不需要额外 capability；Agent 的 ICMP ping，以及 ICMP/TCP traceroute/MTR 需要
`NET_RAW`；UDP 探测和 TCP ping 不需要。

Server 示例使用容器内非特权 UID/GID `65532:65532`。ICMP Agent 示例使用 UID 0，但丢弃除
`NET_RAW` 外的全部 capability；原因是 Docker 不会把单独通过 `--cap-add NET_RAW` 添加的权限
放入非 root 进程的 permitted/effective 集合。若 Agent 只使用 UDP/TCP ping，可以使用镜像默认的
`65532:65532` 并移除 `--cap-add`。挂载的配置和证书需要对实际 UID/GID 可读；私钥建议使用
`0640` 并匹配 group，或使用 Docker/Compose secret。发布镜像不内置运行配置，请显式挂载。
Server 首次启动前，还需把随机 token 写入 `/etc/mtr/bootstrap/admin-token`，确保 UID/GID
65532 可读且其他用户不可读；持久化存储初始化完成后即可删除该文件。示例
`configs/server.yaml` 的 PostgreSQL DSN 将
`/var/run/mtr/postgres/ca.crt` 指定为 `sslrootcert`，所以下面的命令也把数据库 CA 挂载到
这个准确路径；宿主机文件必须允许 UID/GID 65532 读取。只有改用 SQLite，或 PostgreSQL
DSN 已指向其他经过验证的 CA 路径时，才能省略这个挂载。

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
  -e MTR_BOOTSTRAP_ADMIN_TOKEN_FILE=/var/run/mtr/bootstrap/admin-token \
  -v /etc/mtr/server.yaml:/etc/mtr/server.yaml:ro \
  -v /etc/mtr/bootstrap:/var/run/mtr/bootstrap:ro \
  -v /etc/mtr/postgres/ca.crt:/var/run/mtr/postgres/ca.crt:ro \
  -v /etc/mtr/tls/server:/var/run/mtr/tls:ro \
  ghcr.io/ztelliot/mtr-server:0.1.0

docker run -d --name mtr-agent \
  --network mtr-net \
  --user 0:0 \
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
  ghcr.io/ztelliot/mtr-agent:0.1.0
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
install -d -o root -g mtr -m 0750 /etc/mtr /etc/mtr/tls /etc/mtr/bootstrap
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
umask 0077
openssl rand -hex 32 > /etc/mtr/bootstrap/admin-token
chown root:mtr /etc/mtr/bootstrap/admin-token
chmod 0640 /etc/mtr/bootstrap/admin-token
systemctl daemon-reload
systemctl enable --now mtr-server
systemctl enable --now mtr-agent
```

Server unit 已指向这个 bootstrap 文件。持久化存储初始化完成且所需托管 token 已安全保存后，
应删除 `/etc/mtr/bootstrap/admin-token`；已初始化的存储仍可在文件不存在时正常启动。

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
- `networkpolicy.yaml`：拒绝进入 Agent Pod 的入站流量，并把 Server 的 HTTP/gRPC 入站来源
  限制为基线 Web/Agent Pod。
- `secrets.example.yaml`：需要替换占位符后再应用的 Secret 模板。
- `kustomization.yaml`：用于 `kubectl apply -k deploy/k8s` 的基础资源；它有意不包含
  `secrets.example.yaml`。

这套清单默认假设：

- PostgreSQL 由外部服务提供；启用完整 TLS 校验的 `database-url` 通过
  `mtr-server-env` 注入，数据库 CA 则从 `mtr-server-postgres-ca` 挂载。
- gRPC 控制面走 `mtr-server.mtr.svc.cluster.local:8443`，因此 Server 证书 SAN 已按这个域名示例生成。
- 所有 Agent 共享同一张客户端证书，但每个 Pod 通过 `MTR_ID=metadata.name` 保持唯一逻辑身份。
- Agent Pod 从 `mtr-agent-env` Secret 读取注册 token，且该值必须匹配 Server
  托管配置中保存的 Agent 注册 token。
- Server 保持 `replicas: 1`。当前调度 Hub 维护的是进程内连接状态，因此这份基础清单不直接做多副本控制面。
- API token、Agent 注册 token、限流、全局配置、按标签生效的调度/运行参数、
  标签策略和出站 Agent 都存储在 Server 托管配置中，不再从 Kubernetes
  Server ConfigMap 或 Secret 读取。
- 空存储首次启动时，只从可选的 `mtr-server-bootstrap` Secret 读取一次初始
  admin API token；Server 日志不会输出该 token。
- CNI 必须支持并实际执行 NetworkPolicy；如果没有这项保证，应用清单前必须把 Server
  ConfigMap 中的宽泛可信代理范围改成真实 Web Pod CIDR 或其他严格受限的代理范围。

bootstrap 流程需要分阶段进行：先只创建 Server 的前置 Secret 并启动 Server，再从可信的本地
客户端使用初始 admin token 创建公共 Web 诊断 token 和 Agent 注册 token，之后才配置 Web 并
部署 Web 与 Agent。这样既不会把 admin token 公开给浏览器，也不会在 Server 尚未启动时就要求
托管 token。可以按需改写 `deploy/k8s/secrets.example.yaml` 的占位符，也可以先用命令行创建
第一阶段 Secret：

```sh
kubectl apply -f deploy/k8s/namespace.yaml

kubectl -n mtr create secret generic mtr-server-env \
  --from-literal=database-url='postgres://mtr:mtr@postgres:5432/mtr?sslmode=verify-full&sslrootcert=/var/run/mtr/postgres/ca.crt'

kubectl -n mtr create secret generic mtr-server-postgres-ca \
  --from-file=ca.crt=certs/postgres-ca.crt

umask 077
openssl rand -hex 32 > admin-token
kubectl -n mtr create secret generic mtr-server-bootstrap \
  --from-file=admin-token=admin-token

kubectl -n mtr create secret generic mtr-server-tls \
  --from-file=ca.crt=certs/ca.crt \
  --from-file=tls.crt=certs/server.crt \
  --from-file=tls.key=certs/server.key

kubectl -n mtr create secret generic mtr-agent-tls \
  --from-file=ca.crt=certs/ca.crt \
  --from-file=tls.crt=certs/agent-shared.crt \
  --from-file=tls.key=certs/agent-shared.key
```

第一阶段只应用 Server，然后在一个终端中保持本地端口转发：

```sh
kubectl apply -f deploy/k8s/server.yaml
kubectl -n mtr rollout status deployment/mtr-server
kubectl -n mtr port-forward service/mtr-server 8080:8080
```

本地 `admin-token` 文件中的值就是初始 admin API token。在另一个终端创建严格收窄的公共 token
（下面的示例只允许 `ping`）以及本次上线使用的 Agent 注册 token：

```sh
ADMIN_TOKEN="$(tr -d '\r\n' < admin-token)"

curl --fail-with-body -X POST http://127.0.0.1:8080/v1/manage/tokens \
  -H "Authorization: Bearer ${ADMIN_TOKEN}" \
  -H 'Content-Type: application/json' \
  -d '{"name":"public-web","all":false,"schedule_access":"none","manage_access":"none","agents":["*"],"tools":{"ping":{}}}' \
  > /tmp/mtr-public-web-token.json

curl --fail-with-body -X POST http://127.0.0.1:8080/v1/manage/register-tokens \
  -H "Authorization: Bearer ${ADMIN_TOKEN}" \
  -H 'Content-Type: application/json' \
  -d '{"name":"k8s-agents","remaining_uses":3}' \
  > /tmp/mtr-agent-register-token.json
unset ADMIN_TOKEN
```

两个响应只会分别在 `.token.secret` 和 `.token.token` 中展示一次新值。应按部署需要继续收窄
公共 token 的工具和 Agent 范围，并按实际规模设置 `remaining_uses` 与 `allowed_agent_ids`。
把公共值写入 `mtr-web-config` 的私有副本或 overlay，绝不能写入 admin token；把注册值保存到
Agent Secret，然后部署其余工作负载：

```sh
kubectl -n mtr create secret generic mtr-agent-env \
  --from-literal=register-token='<managed-register-token>'
kubectl apply -f '<private-web-manifest>' \
  -f deploy/k8s/agent.yaml \
  -f deploy/k8s/networkpolicy.yaml
kubectl -n mtr rollout status deployment/mtr-web
```

持久化数据库初始化完成且所需托管 token 已安全保存后，应删除本地文件以及
一次性响应文件和 `mtr-server-bootstrap` Secret；后续重启时可选卷可以保持不存在。后续整体更新
应使用基于 `deploy/k8s` 的私有 Kustomize overlay，不能让仓库中的 Web 占位值覆盖已部署的
公共 token。

Agent 的权限在清单里被刻意压到很窄：

- 主 Agent 容器不挂载 ServiceAccount Token
- 不使用 host network/PID/IPC，`privileged: false`
- 根文件系统只读，只额外挂载只读配置、只读证书和一个 `emptyDir` 类型的 `/tmp`
- 默认丢弃全部 Linux capabilities，只额外保留 `NET_RAW`
- 禁用提权，并启用运行时默认 seccomp profile
- 附带 NetworkPolicy 拒绝进入 Agent Pod 的入站流量

之所以保留 `NET_RAW`，是因为当前 `ping`、`traceroute`、`mtr` 的 ICMP 实现需要原始 socket。Agent 主容器使用 UID 0 运行，以便 `NET_RAW` 能进入 permitted/effective capability sets；但它不是 privileged，且仍然丢弃除 `NET_RAW` 之外的全部 capabilities。若你的集群启用了严格的 Pod Security Admission，需要为这个 Agent securityContext 显式放行。Agent 启动时会打印 `CapEff`、`CapBnd`、`NoNewPrivs` 和 `Seccomp`，便于确认实际权限。

DaemonSet 示例额外包含一条很窄的 RBAC：initContainer 需要 `get nodes`，用于从 Node annotations 渲染每个节点的配置。生成后的配置写入 `emptyDir`，主 Agent 容器随后在不挂载 ServiceAccount Token 的情况下运行。

这套清单没有附带“默认拒绝 egress”的标准 `NetworkPolicy`。原因是 Agent 的核心职责就是对任意外部目标发起探测，而标准 Kubernetes `NetworkPolicy` 对 ICMP 的表达能力有限；如果你的 CNI 支持更细粒度的 ICMP/egress 规则，建议在该能力之上进一步收紧。

## FaaS 部署

FaaS 部署文件位于 `deploy/fc/`。仓库提交的 `targets.json` 只描述可公开的目标元数据
和占位运行参数；`generate.mjs` 会根据它生成各云厂商需要的部署模板。

公共模板的顶层 HTTP 配置默认使用 `header` 鉴权，请求头名为 `X-MTR-Agent-Token`。只在本地
targets 文件中填写 secret，并让 Server 出站 Agent 的 `auth` 使用完全相同的请求头名和
secret。华为云 FunctionGraph 是例外：对应 provider 会覆盖为受 FunctionGraph IAM 调用边界
保护的 `event` 模式，不使用这组应用层 HTTP secret。

目前支持：

- `aliyun`：阿里云 Function Compute，通过 Serverless Devs 的 `fc3` 组件部署。
- `ctyun`：天翼云函数服务，通过 Serverless Devs 的 `faas-cf` 组件部署。
- `huaweicloud`：华为云 FunctionGraph，自动拉取源镜像、推送到 SWR 后通过官方 KooCLI 部署。
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
`targets.json` 时，会拒绝看起来像真实 token、路径前缀、阿里云日志项目 ID、华为云项目 ID
或委托名称，以及华为云 SWR、VPC、日志、KMS 等部署标识，避免误把私有配置写进公开模板。

通过 `binary.path` 指向本地已构建的 Agent 二进制，或配置
`binary.fromImage` 从 Docker 镜像中提取。生成内容都会写到
`deploy/fc/output/`：

```bash
output/binary/agent
output/s.aliyun.yaml
output/s.ctyun.yaml
output/huaweicloud/<target-key>.list.json
output/huaweicloud/<target-key>.create.json
output/huaweicloud/<target-key>.update.json
output/huaweicloud/deploy.sh
output/huaweicloud/hcloud-helper.mjs
output/qcloud/code/scf_bootstrap
output/qcloud/<target-key>/serverless.yml
output/qcloud/deploy.sh
output/gcloud/deploy.sh
```

FaaS Agent 不读取 `config.yaml`，所有运行参数都通过环境变量注入。每个目标都会得到
`MTR_COUNTRY`、`MTR_ID`、`MTR_ISP`、`TZ` 和 `MTR_REGION`；Agent 运行参数来自
`targets.json` 顶层的 `env`，也可以被 provider 或 target 里的 `env` 覆盖。

部署完成后，可在仓库根目录生成出站 Agent 的 PostgreSQL 插入 SQL：

```bash
node deploy/fc/generate.mjs sql --provider all
```

脚本默认读取 `deploy/fc/.targets.json`（不存在时读取公开 `targets.json`），输出到
`deploy/fc/output/outbound-agents.sql`。可用 `--config`、`--output`、`--provider` 和 `--regions`
选择文件、厂商及地域。`--config` 相对于 `deploy/fc/` 解析，`--output` 相对于当前工作目录解析。
SQL 子命令只查询已部署资源，不清理部署输出，也不提取二进制；输出文件已存在时会拒绝覆盖。
原有 `node deploy/fc/generate.mjs --config .targets.json` 仍只生成部署文件。
阿里云、天翼云通过 `s <key> info` 读取地址；腾讯云在生成物父目录执行一次 `scf info`；
Google Cloud 执行一次 `gcloud alpha run services list --format=json`；华为云查询 IAM 项目 ID。
HTTP Agent 自动追加配置中的路径前缀，并使用合并后的 header/bearer token；华为云默认从数据库
现有 `hwc.cn-north-1.fc` 行复用 AK/SK。其他厂商也可以通过
`--source-id aliyun=已有ID,ctyun=已有ID,huaweicloud=已有ID` 复用认证。
天翼云如果只返回内网域名，会提示 Server 需要能访问该网络；也可在 target 配置 `agentUrl`
显式指定调用地址。SQL 文件可能包含 HTTP token，权限为 `0600`，不要提交。
脚本只读查询部署信息，不执行 SQL；已有 Agent ID 会跳过。在可访问该文件的 psql 客户端执行
`\i deploy/fc/output/outbound-agents.sql` 即可导入。

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
    "path": ".cache/mtr-agent"
  }
}
```

`binary.path` 与 `binary.fromImage` 互斥。镜像提取的可执行文件固定暂存在
`output/binary` 下，所有 provider 输出也必须位于 `output/` 内。

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
    "MTR_HTTP_AUTH_TYPE": "header",
    "MTR_HTTP_AUTH_HEADER": "X-MTR-Agent-Token",
    "MTR_HTTP_AUTH_SECRET": "<http-auth-secret>",
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
  --set-env-vars=MTR_HTTP_AUTH_TYPE=header \
  --set-env-vars=MTR_HTTP_AUTH_HEADER=X-MTR-Agent-Token \
  --set-env-vars=MTR_HTTP_AUTH_SECRET=<http-auth-secret> \
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

腾讯云 SCF 也可以使用容器镜像部署，而不是上传复制出的二进制包。
部署生成器不会代为发布源镜像，因此应先构建并发布所引用的 tag。给
`providers.qcloud.image` 设置值后，所有腾讯云目标都会启用镜像模式：

```json
{
  "providers": {
    "qcloud": {
      "name": "mtr-agent-qcloud",
      "output": "qcloud",
      "image": {
        "sourceImage": "ghcr.io/ztelliot/mtr-agent:0.1.0",
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
    "sourceImage": "ghcr.io/ztelliot/mtr-agent:0.1.0",
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
docker pull ghcr.io/ztelliot/mtr-agent:0.1.0
docker tag ghcr.io/ztelliot/mtr-agent:0.1.0 ccr.ccs.tencentyun.com/sls-scf/mtr-agent:latest
docker push ccr.ccs.tencentyun.com/sls-scf/mtr-agent:latest
```

脚本会按生成后的目标镜像 tag 去重同步。以上配置中，中国大陆目标共享
`ccr.ccs.tencentyun.com/sls-scf/mtr-agent:latest`；像 `ap-singapore` 这样的境外
目标则会使用 `sgccr.ccs.tencentyun.com/sls-scf/mtr-agent:latest`。

### 华为云 FunctionGraph 自定义镜像

`providers.huaweicloud` 使用 FunctionGraph v2 自定义镜像事件函数。仓库中的
`targets.json` 已包含可公开的 provider 和 target 模板；请在本地 `.targets.json` 中填写
委托名称、源镜像和目标镜像地址；组织名直接写在 `image.imageUrl` 中，默认无需填写项目 ID。
生成器会为每个 target 写出供
KooCLI 查询、创建和更新函数的三份 JSON 请求文件，并生成统一部署脚本：

```text
output/huaweicloud/<target-key>.list.json
output/huaweicloud/<target-key>.create.json
output/huaweicloud/<target-key>.update.json
output/huaweicloud/deploy.sh
output/huaweicloud/hcloud-helper.mjs
```

包含一个 target 的最小 provider 配置如下；更多地域可以直接参考仓库中的公开模板：

```json
{
  "providers": {
    "huaweicloud": {
      "name": "mtr-agent-huaweicloud",
      "output": "huaweicloud",
      "agencyName": "<functiongraph-swr-agency>",
      "package": "default",
      "function": {
        "timeout": 600,
        "strategy_config": {
          "concurrent_num": 400,
          "concurrency": 1
        }
      },
      "image": {
        "sourceImage": "ghcr.io/ztelliot/mtr-agent:<immutable-tag>",
        "imageUrl": "{registry}/<organization>/mtr-agent:<immutable-tag>",
        "sync": false,
        "uid": "1003",
        "gid": "1003",
        "isPublic": false
      },
      "targets": [
        {
          "key": "beijing",
          "region": "cn-north-4",
          "image": {
            "sync": true
          },
          "functionName": "mob-beijing",
          "country": "CN",
          "id": "hwc.cn-north-1.fc",
          "label": "Beijing"
        }
      ]
    }
  }
}
```

`projectId` 是可选项。部署脚本会先通过 `hcloud IAM KeystoneListProjects/v3` 查询与地域名称
精确匹配的可用项目，无需在 targets 中填写；所用身份需要有查询项目的权限。
如需指定子项目，或身份没有 IAM 项目查询权限，可在 provider 或 target 上显式填写 `projectId`。
所有地域的项目查询都会在上传镜像、创建或更新函数之前完成；没有唯一可用项目时直接退出。
脚本通过 `ListFunctions` 按函数名和分组精确查询，存在时使用返回的真实 URN 更新，不存在时创建；
查询失败、返回异常或结果不唯一时直接退出，不会误创建函数。部署脚本需要 Node.js 解析查询结果。
脚本会把完整的 `path.project_id` 和 `path.function_urn` 写入私有临时 JSON 文件，避免 KooCLI
混用命令行和 `cli-jsonInput` 的路径参数导致缺少必填参数。CLI 即使以退出码 0 输出
`[USE_ERROR]` 等错误，也会被判为失败并显示错误类型。临时请求在退出时删除；请将生成的
`hcloud-helper.mjs` 与 `deploy.sh` 一起使用。项目查询接口参见
[IAM 项目列表](https://support.huaweicloud.com/api-iam/iam_06_0001.html)。
可选的 `profile` 也支持 provider 默认值和 target 覆盖；未配置时 KooCLI 使用自身的默认 profile。
镜像格式与腾讯云一致：`image.sourceImage` 指定源镜像，
`image.imageUrl` 指定 SWR 目标地址，其中的 `<organization>` 替换成自己的组织名。
`sourceImage` 支持 `{key}`、`{region}` 和 `{functionName}`；`imageUrl` 还支持 `{registry}`。
target 的 `image` 可以覆盖 provider 的同名字段。

`image.sync` 控制脚本是否上传该 target 的镜像，省略时为 `true`。
设为 `false` 时跳过该地域的 `docker pull/tag/push`，但仍创建或更新函数，使用其 `imageUrl`。
上例及公开模板在 provider 上设为 `false`，仅示范为北京四覆盖成 `true`；这只是同步策略示例，
请按自己的平台同步规则调整，需要手工上传的其他地域也可以覆盖为 `true`。
所有选中的镜像先上传，再部署全部函数。跳过上传的地域需要事先在 SWR 中有对应镜像；
平台跨地域同步可能异步完成，脚本不会等待或轮询其状态。若全部 target 都设为 `false`，
无需配置 `sourceImage`，生成的部署脚本也不依赖 Docker。

生成器已内置各地域的 registry：
默认使用 `swr.{region}.myhuaweicloud.com`，`eu-west-101` 自动使用
`swr.eu-west-101.myhuaweicloud.eu`，示例无需配置 `registry` 或 `registries`。
如需自定义，可用 `image.registry` 统一覆盖，或用 `image.registries` 按地域覆盖。
脚本默认拉取 `linux/amd64` 变体，可通过 `image.platform` 覆盖。发布时，镜像应使用唯一且不可变的
tag，不能在发布后把同一个 tag 指向另一份内容。华为云部署不能通过 `"image": false` 关闭；FunctionGraph 对
容器镜像函数统一要求提供 `agencyName`，包括标记为公开的镜像。FunctionGraph 不允许容器使用
root 或其他保留 ID，因此生成的 `custom_image` 默认使用平台推荐的 UID/GID `1003`；如需覆盖
`image.uid` 和 `image.gid`，生成器也会按华为云的合法范围进行校验。非 root FunctionGraph
运行时不能假定具有原始 socket 权限：请求 ICMP 时
应优先使用 Agent 的 `protocol=auto`，让它在 ICMP 不可用时回退到 UDP/TCP，并在正式上线前做
一次真实函数调用验证。

公开模板中的地域均有 FunctionGraph v2 和 SWR endpoint，但华为云没有发布完整的自定义镜像
逐地域支持矩阵，并要求以控制台展示为准。首次部署前，请确认目标地域的 FunctionGraph 创建方式
包含“容器镜像”；部署脚本遇到不支持的地域会直接返回 Create/Update 错误，不会静默回退。

华为云函数默认超时为 `600` 秒，单实例并发为 `400`（`strategy_config.concurrent_num`），
最大实例数为 `1`（`strategy_config.concurrency`）。这些值可在 provider 或 target 的 `function`
中覆盖。`strategy_config` 仅由 `UpdateFunctionConfig` 接口支持，创建请求不会包含它；部署脚本
会在新建函数成功后补一次配置更新，已有函数也会应用同一份配置。这里只设置云端并发，Agent
和 Server 的应用层任务并发上限仍为 `4`，没有随之修改。

生成器默认写入 `network_controller.disable_public_network=false`，使 Agent 可以访问公网探测
目标。可在 provider 或 target 的 `function` 中覆盖内存、超时、VPC、日志及其他同时受
CreateFunction 和 UpdateFunctionConfig 支持的参数；除单独处理的 `strategy_config` 外，仅受其中
一个接口支持的字段会在生成阶段被拒绝，常用字段的类型和范围也会预先校验。若配置 `function.func_vpc`，FunctionGraph 会关闭
默认网卡，此时需要在 VPC 中另行配置 NAT/EIP 才能继续访问公网。

部署前需要完成以下准备：

- 对 `image.sync: true` 的地域，安装 Docker，并使用 SWR 控制台的登录指令登录目标 registry。
  脚本会向 `image.imageUrl` 中指定的组织推送镜像；请确保组织可用、登录身份有推送权限。
  对 `image.sync: false` 的地域，提前通过平台完成同步，无需本地 Docker 登录。
- 安装华为云 KooCLI，执行 `hcloud configure init`，并保证所选 profile 有查询、创建和更新
  FunctionGraph 函数的权限。KooCLI 的 AK/SK 只保存在其本地配置中，不要写入 targets 文件。
- 创建 FunctionGraph 云服务委托，为其授予 `SWR Admin`，再把委托名称填入 provider 的
  `agencyName`（生成的 FunctionGraph 请求中对应 `xrole`）。这是 FunctionGraph 使用容器镜像
  所必需的权限，与运行部署脚本的 KooCLI 身份是两个不同概念。

在 `deploy/fc` 目录生成并部署：

```bash
node generate.mjs --config .targets.json
bash output/huaweicloud/deploy.sh
```

`deploy.sh` 只为 `image.sync: true` 的目标执行 `docker pull`、`docker tag` 和 `docker push`，再对全部目标通过官方
`hcloud FunctionGraph` 命令：函数不存在时创建，已存在时更新其配置和镜像。JSON 和脚本都是
生成物，不要手工修改，也不要提交到仓库。
连续使用相同源镜像和平台时只拉取一次，同一个目标 tag 只推送一次；不同源镜像或平台映射到
同一需要上传的目标 tag 时会在生成阶段报错，避免误部署。

环境变量按“全局 `env` → provider `env` → provider `envs[region]`（没有则选 `envs[key]`）→
target `env`”的顺序合并，同名字段后者覆盖，未指定的字段继续继承，`null` 删除字段。
下述 FunctionGraph 协议字段会在合并后由生成器强制适配。

华为云 target 会覆盖顶层 HTTP 配置，以 `event` 模式监听 FunctionGraph 自定义镜像规定的
`8000` 端口，并提供 `POST /init` 和 `POST /invoke`。容器内的
`MTR_HTTP_AUTH_TYPE` 为 `none`，因为外部调用已经由 FunctionGraph IAM API 边界鉴权；不要把
容器端口另行匿名暴露。Server 侧沿用上文的华为云 Event Agent 配置：`transport` 使用
`event`，`auth.type` 使用 `huaweicloud_aksk`，请求头包含
`X-CFF-Request-Version: v1`，并设置 `event_response: result_field`。出站 Agent 的 `id`
必须与 target 的 `id` 相同。

虽然 FunctionGraph 官方提供 Serverless Devs `fgs` 组件，但当前
[YAML 配置说明](https://support.huaweicloud.com/devg-functiongraph/functiongraph_02_0725.html)
中的 `codeType` 仅支持 `inline`、`zip`、`obs` 和 `jar`，尚不支持自定义镜像所需的
`Custom-Image-Swr`。因此华为云镜像部署不能使用 `s deploy`，本仓库改用 KooCLI；待官方组件
支持自定义镜像后再考虑与其他 Serverless Devs provider 统一。更多官方资料见
[使用容器镜像创建函数](https://support.huaweicloud.com/usermanual-functiongraph/functiongraph_01_1047.html)、
[FunctionGraph 功能地域说明](https://support.huaweicloud.com/productdesc-functiongraph/functiongraph_01_0141.html)、
[通过 KooCLI 管理函数](https://support.huaweicloud.com/usermanual-functiongraph/functiongraph_01_0396.html)、
[创建函数 API](https://support.huaweicloud.com/api-functiongraph/functiongraph_06_0108.html)、
[更新函数 metadata API](https://support.huaweicloud.com/api-functiongraph/functiongraph_06_0111.html) 和
[配置函数的委托权限](https://support.huaweicloud.com/usermanual-functiongraph/functiongraph_01_0920.html)。

## 致谢

感谢 OpenAI Codex 在项目开发、重构、测试与文档整理过程中提供协作支持。
