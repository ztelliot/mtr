# MTR Agent/Server

Distributed network diagnostics for running `ping`, `traceroute`, `mtr`, HTTP,
DNS, and TCP port probes from managed edge Agents.

Chinese version: [README.zh-CN.md](README.zh-CN.md)

## Components

- `cmd/server`: REST API, gRPC Agent control plane, storage, policy, scheduling,
  and rate limiting.
- `cmd/agent`: edge worker. In `grpc` mode it keeps a long-lived control-plane
  connection to Server; `http` exposes a streaming invoke endpoint, while
  `event` adapts synchronous cloud function events.

## Quick Start

For a local SQLite-backed test run:

```sh
bootstrap_token_file="$(mktemp)"
chmod 0600 "$bootstrap_token_file"
openssl rand -hex 32 > "$bootstrap_token_file"
MTR_BOOTSTRAP_ADMIN_TOKEN_FILE="$bootstrap_token_file" \
  go run ./cmd/server -config configs/server.sqlite.yaml
```

The file contains the initial admin API token. Use it to sign in and create the
narrower API and Agent register tokens you need. After the persistent SQLite
database has been seeded successfully, stop Server, save any token that must be
retained in an appropriate secret store, delete the bootstrap file, and restart
without `MTR_BOOTSTRAP_ADMIN_TOKEN_FILE`:

```sh
rm -f "$bootstrap_token_file"
unset bootstrap_token_file
go run ./cmd/server -config configs/server.sqlite.yaml
```

Server reads `/etc/mtr/server.yaml` by default when present, otherwise
`configs/server.yaml`. Use `-config` to point at another file.

On first startup, Server creates the SQLite database automatically. With
PostgreSQL, it also tries to create the target database and schema when the
configured account has permission.

Agents read config from `-config`, matching Server startup:

```sh
./mtr-agent -config /etc/mtr/agent.yaml
```

Both binaries support `-version`. Build metadata can be injected with Go
ldflags, for example:

```sh
CGO_ENABLED=0 go build -trimpath -ldflags "-X github.com/ztelliot/mtr/internal/version.Version=v1.2.3 -X github.com/ztelliot/mtr/internal/version.Commit=$(git rev-parse --short HEAD) -X github.com/ztelliot/mtr/internal/version.BuiltAt=$(date -u +%Y-%m-%dT%H:%M:%SZ)" ./cmd/server
```

Docker images accept the same metadata through build args:

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

Agent runtime settings live in the Agent YAML: `mode`, `http_addr`, identity,
register token, inbound HTTP authentication, capabilities, protocols, TLS, and
speed-test limits.
`protocols` is a bitmask: `1` means IPv4, `2` means IPv6, and `3` means both.
Jobs may set `ip_version` to `4` or `6`; tasks are only dispatched to Agents
whose protocol mask supports the requested protocol. `protocols` and
`capabilities` must both be explicit and non-empty; zero or an empty list is
rejected instead of being expanded into broader access.

Agent control is intentionally versioned as one complete protocol, without
per-feature negotiation. Every hello, health response, and job request must
carry `protocol_version: 1`; UDP probes and every advertised HTTP version are
part of that version. Upgrade Server, Agent, and Web together.

Server and Agent config fields can also be supplied through environment
variables. Use the YAML path with an `MTR_` prefix and uppercase underscores:
`tls.ca_files` becomes `MTR_TLS_CA_FILES`, and `speedtest.max_bytes` becomes
`MTR_SPEEDTEST_MAX_BYTES`. Config file values win when both sources set the same
field. String lists may be comma-separated or YAML/JSON arrays.
Unknown YAML keys, structured subfields, and `MTR_` environment variable names
are rejected so misspelled security settings cannot silently fall back.
When Server runs behind a reverse proxy, set `trusted_proxies` to the proxy IPs
or CIDRs allowed to supply proxy headers; otherwise `X-Forwarded-For`,
`X-Real-IP`, and `client_ip_headers` are ignored for logging and rate limiting.
`client_ip_headers` is an ordered list of custom single-IP headers evaluated
before the standard proxy headers, but only for a peer in `trusted_proxies`.
Never broaden `trusted_proxies` unless a firewall or NetworkPolicy makes the
proxy the only process that can reach the listener.

Server runtime policy is stored in persisted managed settings, not in
`server.yaml`. This includes API tokens, Agent register tokens, rate limits,
global and label-based tool policies, label-based scheduler/runtime knobs,
probe counts, timeouts, and outbound Agents. Only `agent`, `agent:*`, and `id:*`
labels may configure execution parameters and tool policies; other labels only
group nodes for selection and access scopes. Every node carries `agent`, its
transport-specific `agent:*` label, and `id:<agent-id>` for global, node-type,
and per-node configuration. User interfaces hide `agent` and `agent:*` policy
labels while retaining group and individual-node selection. An empty store is initialized
only from the token in `bootstrap_admin_token_file` (or
`MTR_BOOTSTRAP_ADMIN_TOKEN_FILE`). The file must be regular, no larger than 4
KiB, non-executable, not group-writable, and inaccessible to world users;
modes `0600` and `0440` are accepted.
The token is never written to logs. After a persistent store has been seeded,
remove the file and unset the option. An in-memory store needs the file on every
start because it is not persistent. Use that token from a trusted operator
client with `/v1/manage/tokens`, `/v1/manage/register-tokens`, `/v1/manage/rate-limit`,
`/v1/manage/labels`, and `/v1/manage/agents` endpoints to create the tokens and
policy you want. Each Agent register token has a `remaining_uses` allowance
(new tokens default to `1`). The allowance is consumed atomically only when a
previously unseen Agent ID is created; reconnecting an existing Agent does not
consume it and remains possible at zero.
Set `allowed_agent_ids` on a register token to restrict which exact Agent IDs
it may create. An empty list means the token is unrestricted;
allowlist failures do not consume a use, and already-bound Agents may still
reconnect after the allowlist changes.
An Agent's country, region, provider, and ISP are fixed by its first
registration; later connections may still refresh version, capabilities, and
IPv4/IPv6 protocol support. Use a new Agent ID when those fixed identity fields
must change. Set
`log_level: debug` in the Server or Agent config for verbose scheduling and
execution logs.

Server can actively invoke outbound Agents created through
`/v1/manage/agents`. The required `transport` selects one of two contracts:

- `http`: `url` is a base URL. Server reads `GET <url>/healthz` and streams
  NDJSON from `POST <url>/invoke`.
- `event`: `url` is the complete synchronous function-invocation URL. Server
  posts `action: healthz` and `action: invoke` events to that same URL and
  consumes one complete JSON response.

Authentication is independent of transport. `auth.type` is explicitly one of
`none`, `bearer`, `header`, or `huaweicloud_aksk`. The first three cover no
authentication, a static bearer token, and a static custom-header token.
`huaweicloud_aksk` is the native unattended option for Huawei Cloud
FunctionGraph: Server calculates a new `SDK-HMAC-SHA256` signature from the
final method, URL, headers, and JSON body for every health or invocation
request. Set its `access_key`, `secret`, and `project_id`; `secret` is the Secret
Key and is write-only in management responses. When editing, leaving it empty
retains the existing value only if the transport, URL, authentication fields,
additional headers, and TLS settings are all unchanged; changing any part of
that credential scope requires the Secret Key to be entered again.

Both `http://` and `https://` outbound URLs are accepted; choosing cleartext
HTTP is a deployment decision that exposes authentication headers and payloads
to the network, so use it only on a suitably trusted path. Non-secret provider
options can be supplied through the separate `headers` map; authentication and
other sensitive values belong in `auth`, not in `headers`. Event responses use
`event_response: direct` when the function result is the HTTP body, or
`result_field` when the provider wraps the function result in a top-level
`result` object or JSON string. The latter matches synchronous Huawei Cloud
FunctionGraph invocation. This deliberately does not accept API Gateway
`statusCode`/`body`/`isBase64Encoded` envelopes.

FunctionGraph also accepts an IAM token in a static `X-Auth-Token` header, but
that token is valid for only 24 hours and is therefore suitable here only for
manual testing or when an external component refreshes it. The built-in AK/SK
authorizer currently supports long-lived AK/SK credentials only; temporary
credentials with a security token and automatic IAM username/password token
refresh are not supported. Keep Server time synchronized with NTP because
Huawei Cloud validates the generated `X-Sdk-Date`. See the official
[FunctionGraph authentication](https://support.huaweicloud.com/intl/en-us/api-functiongraph/functiongraph_06_0230.html)
and [API request signing](https://support.huaweicloud.com/intl/en-us/devg-apisign/api-sign-algorithm-001.html)
documentation.

An outbound Agent endpoint is trusted control-plane configuration and may be
public or private, so Agents inside a VPC or cluster remain reachable. Server
resolves and pins each connection itself, rejects redirects and environment
proxies, and limits every response. Loopback, link-local/metadata, unspecified,
and multicast endpoint addresses remain forbidden. This does not relax job targets: user
probe targets still always reject private, loopback, link-local, metadata, and
other special-use addresses. Event invocation is always single-attempt. HTTP invocation may
retry only a DNS, dial, or TLS failure that occurred before a connection was
obtained; once the request might have started, it is never replayed. A `429`
response is treated as temporary backpressure and the job is atomically
requeued.

Server probes an outbound Agent once at startup, periodically refreshes its
metadata at the configured maximum health interval, and uses exponential
health probes only while recovering. Queue polling is separate, so Event
functions are not invoked on every scheduler tick. Listener TLS is controlled
by Agent `http_tls`; outbound TLS settings live on the managed Agent record.
Without `tls.ca_files`, TLS uses the system roots and verifies the certificate
hostname/SAN against the URL hostname. With explicit `tls.ca_files`, Server
instead verifies the chain, validity period, and ServerAuth usage against that
dedicated trust set and intentionally ignores certificate SANs. Use a CA
dedicated to these Agents, or a narrowly shared certificate pin, rather than a
general-purpose trust root. `tls.server_name` is only an optional SNI routing
hint and never changes certificate identity verification. Any custom CA, client
certificate, or SNI hint requires `tls.enabled: true` and an `https://` URL.

Example Huawei Cloud FunctionGraph Event Agent record:

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

An Agent exposes the streaming contract when its mode includes `http`. That
mode requires explicit application-layer authentication: set `http_auth.type`
to `bearer` or `header` and provide a non-empty `secret`. Use
`http_path_prefix` to serve those endpoints below a prefix such as `/api` or
`/v1`, and include that prefix in the outbound Agent `url`.

HTTP Agent contract:

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

To run both the long-lived gRPC Agent and HTTP Agent in one process, use
`mode: "grpc,http"`.

```sh
curl -N -X POST http://localhost:9000/invoke \
  -H 'Authorization: Bearer change-me-http-auth-secret' \
  -H 'Accept: application/x-ndjson' \
  -H 'Content-Type: application/json' \
  -d '{"protocol_version":1,"id":"job-1","tool":"ping","target":"1.1.1.1","ip_version":4}'
```

For a provider or gateway that uses a static custom header, configure both
sides with the same header name and secret. For example, the Agent can use:

```yaml
http_auth:
  type: "header"
  header: "X-MTR-Agent-Token"
  secret: "change-me-shared-secret"
```

The corresponding Server outbound Agent record must then contain
`"auth":{"type":"header","header":"X-MTR-Agent-Token","secret":"change-me-shared-secret"}`.
With `bearer`, Server instead sends `Authorization: Bearer <secret>`. Server
also accepts these authentication modes over cleartext HTTP, but doing so
exposes the credential and request contents on the network; HTTPS remains the
safe default outside a deliberately trusted path.

The response is compact newline-delimited JSON (`application/x-ndjson`): each
line is a structured progress, hop, metric, or final summary event. Server
outbound mode restores only the browser-facing envelope needed for routing and
replay, such as `job_id` and `agent_id`; it does not repeat task-level fields
like `tool`, `target`, or protocol in incremental events. gRPC mode uses the
same compact event shape on the Agent-to-Server path.

### Event mode

Use `event` mode when a function platform invokes the Agent one request at a
time and waits for a complete response. It listens on `:8000` by default and
returns one JSON document from `POST /invoke` instead of an NDJSON stream. A
lightweight, unauthenticated `POST /init` endpoint returns `{"status":"ok"}`
for platforms that perform a container initialization call; platforms without
that lifecycle hook can ignore it. Event mode requires an explicit
`http_auth.type`: use `none` only when the function platform's invocation API
or IAM boundary already authenticates every request. `bearer` requires
`Authorization: Bearer <secret>`; `header` requires the configured header and
secret. Both authenticated forms cover every `POST /invoke`, including the
`healthz` action. Event mode does not support path prefixes or listener TLS.

`configs/agent.event.yaml` contains a complete example:

```yaml
mode: "event"
id: "edge-event-1"
country: "CN"
region: "edge"
provider: "faas"
http_addr: ":8000"
http_auth:
  type: "none" # only behind an authenticated invocation boundary
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

The curl examples below assume an unexposed local test with
`http_auth.type: none`. For defense in depth, select `bearer` with
`MTR_HTTP_AUTH_TYPE=bearer` and `MTR_HTTP_AUTH_SECRET=<secret>`, or select
`header` and additionally set `MTR_HTTP_AUTH_HEADER`. Add the matching header
to both health and job requests. Authentication runs before event action
dispatch, so `healthz` cannot bypass it.

Send `action: healthz` to inspect the Agent and its runtime network privileges:

```sh
curl -X POST http://localhost:8000/invoke \
  -H 'Content-Type: application/json' \
  -d '{"protocol_version":1,"action":"healthz"}'
```

Job requests use the existing `JobSpec` fields and require `action: invoke`:

```sh
curl -X POST http://localhost:8000/invoke \
  -H 'Content-Type: application/json' \
  -d '{"protocol_version":1,"action":"invoke","id":"job-1","tool":"ping","target":"1.1.1.1","ip_version":4,"args":{"protocol":"udp"},"timeout_seconds":20}'
```

The function returns one `application/json` document after the job finishes.
Progress events are collected in `events`, while the final summary is placed in
`result`:

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

The `http` and `event` modes use different `/invoke` contracts and therefore
cannot run together in one process.

#### Container image

The same image built from `Dockerfile.agent` supports `grpc`, `http`, and
`event` modes. It contains only the Agent binary and CA certificates; runtime
configuration is supplied through a mounted YAML file, `MTR_*` environment
variables, or both. The default config file is optional. The Agent and Server
scratch images run as the unprivileged UID/GID `65532:65532` by default.

Build the image and start an event-mode container locally:

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

For deployment, configure the platform to send invocation requests to
`POST /invoke` on port 8000. The same listener provides a lightweight
`POST /init` response for platforms that enable a separate container
initialization call; no extra adapter is required, and platforms that do not
use initialization can ignore it. The function timeout should exceed the
job's `timeout_seconds` and leave room for cold start. Provider-specific
envelopes added outside the container must be handled by the caller or a Server
adapter.

To push the image, replace `--load` with `--push` and use the desired registry
tag. Current Buildx versions may attach provenance or SBOM attestations as
additional manifests in the image index. If an older registry reports that it
cannot parse the manifest, retry the same single-platform build with
`--provenance=false --sbom=false`. These compatibility flags remove that
supply-chain metadata, so they should not be the default and do not fix an
architecture mismatch or every registry-side manifest error. Build for the
architecture used by the runtime, usually `linux/amd64` or `linux/arm64`.

#### Network privileges

Function runtimes often choose the container UID/GID and restrict Linux
capabilities. A Dockerfile cannot restore a capability removed from the runtime
bounding set, and adding a `cap_net_raw` file capability may cause the binary to
fail at startup. Check `network.cap_net_raw` in the `healthz` response instead
of relying on image metadata.

When `NET_RAW` is unavailable, ping can use `udp`, `tcp`, or `auto` mode. For
traceroute and MTR, use `udp`: both ICMP probes and TCP probes that discover
intermediate hops need `NET_RAW`.

UDP ping sends a real UDP datagram. A UDP reply or an ICMP Port Unreachable
response counts as reachability; the default destination port is `33434` and
`args.port` may override it. Firewalls may silently discard UDP, so explicit
UDP never falls back. Only `protocol: auto` may try ICMP, then UDP, then TCP;
after the first successful probe it locks that protocol for the rest of the
job. The result summary always includes the actual `protocol`, and auto results
also include `requested_protocol: auto`.

`healthz` also reports `network.ping_group_range` and whether the kernel permits
unprivileged ICMP Echo datagram sockets for IPv4 and IPv6.

For example, to test ICMP mode on ordinary Docker, run the Agent as root while
dropping every capability except `NET_RAW`:

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

If the runtime forces a non-root user, `--cap-add NET_RAW` may still leave the
permitted and effective sets empty. The `healthz` result is authoritative.

Agent speed test endpoint:

```sh
curl -H 'Authorization: Bearer change-me-http-auth-secret' -o /dev/null 'http://localhost:9000/speedtest/random?bytes=10485760'
```

The speed test endpoint is only available when HTTP mode is enabled, on
`http_addr`. It is protected by the Agent `http_auth`, using the configured
Bearer or custom header. Configure limits under `speedtest` in the Agent config, or set
`speedtest.max_bytes: 0` to disable the endpoint. Speed-test rate limiting uses
the direct remote address and ignores `X-Forwarded-For`.

Create a job:

```sh
curl -X POST http://localhost:8080/v1/jobs \
  -H 'Authorization: Bearer developer-token' \
  -H 'Content-Type: application/json' \
  -d '{"tool":"ping","target":"1.1.1.1"}'
```

`ping`, `traceroute`, and `mtr` accept `args.protocol` as `icmp`, `udp`, `tcp`,
or `auto`. The default is strict `icmp`; an explicit `auto` request tries
`icmp`, then `udp`, then `tcp`, and locks the first protocol that responds. UDP
defaults to port `33434`, while TCP defaults to port `80`; `args.port`
overrides either. UDP route probes use the Linux socket error queue and do not
require a raw socket; UDP route probing is currently supported only on Linux.
TCP route probes correlate the quoted TCP flow in raw ICMP Time Exceeded
messages and treat either a completed connect or a destination RST as reached.
This implementation is Linux-only and needs `NET_RAW` to collect intermediate
hops.
`http` accepts `args.http_version` as `1.1`, `2`, `3`, or `auto`. The default is
strict HTTP/1.1; `auto` tries HTTP/3, HTTP/2, then HTTP/1.1 and only falls back
before a response has started. HTTP/2 and HTTP/3 require HTTPS, and HTTP/3
requires outbound UDP access. Results report the selected protocol in
`protocol` or `http_version`, plus `requested_protocol` or
`requested_http_version` when `auto` was requested.
`auto` is a request selection strategy, not a configurable permission. Protocol
and HTTP version allowlists contain only concrete values. Auto selects and falls
back within the intersection of the token, node, and Agent policies, including
probe port restrictions; it rejects a job when no permitted candidate remains.
`count` and `max_hops` are controlled by Server runtime policy, not by user
requests. `dns` replaces the old `nslookup` tool name. `port` performs a native
TCP connect probe and requires `args.port`. By default, Server resolves target
hostnames before queuing jobs; set `resolve_on_agent: true` to defer DNS
resolution and resolved-IP policy checks to the Agent. Fan-out `ping`, `dns`,
`port`, and `http` jobs may set `agent_tags` to run only on Agents matching any
selected managed label; omitting it keeps the existing all-eligible-Agent
behavior. Tag filtering is intersected with the API token scope and the normal
online, capability, and per-Agent policy checks. Public `agent_tags` values
must not be `agent` or contain `:` or `-`; those forms are reserved for
server-managed labels and are rejected by the API.

Create a scheduled detection task:

```sh
curl -X POST http://localhost:8080/v1/schedules \
  -H 'Authorization: Bearer developer-token' \
  -H 'Content-Type: application/json' \
  -d '{"name":"cf-ping","tool":"ping","target":"1.1.1.1","cron_expression":"*/5 * * * *","schedule_targets":[{"label":"agent"}]}'
```

Scheduled jobs select nodes with `schedule_targets`. Every Agent gets the
server-managed `agent` label plus `id:<agent-id>`. Custom labels are set in
server-side Agent or outbound Agent config. Use `agent` for all nodes, a
custom label for a group, or `id:<agent-id>` for one node.
`cron_expression` uses the standard five fields `minute hour day month weekday`
and is evaluated in UTC. All selected targets run together at each cron tick.
The server stores the creator's Agent scope as selector rules on each schedule
target. Labels and unrestricted scopes are re-evaluated against the current
Agent set on every cron tick, so newly added matching Agents are included while
fixed allowlists and deny rules remain enforced. Schedule and job read endpoints
are intentionally shareable to any token with the corresponding read permission.

Query scheduled task history:

```sh
curl -H 'Authorization: Bearer developer-token' \
  http://localhost:8080/v1/schedules/<schedule-id>/history
```

Stream structured job events for browser UI:

```js
const response = await fetch('/v1/jobs/<job-id>/stream', {
  headers: {Authorization: 'Bearer developer-token', Accept: 'text/event-stream'}
})
for await (const chunk of response.body.pipeThrough(new TextDecoderStream())) {
  console.log(chunk) // parse the standard SSE frames incrementally
}
```

API tokens are accepted only through the Bearer header. The Web workbench uses
authenticated `fetch` streaming because native `EventSource` cannot attach that
header; tokens are never placed in stream URLs.

## Web Workbench

The project includes an independent React SPA in `web/` for browser-based
diagnostics. It uses Vite, React, TypeScript, and pnpm.

Install dependencies:

```sh
pnpm --dir web install
```

Configure the runtime API connection in `web/public/config.json`:

```json
{
  "apiBaseUrl": "",
  "apiToken": "REPLACE_WITH_PUBLIC_API_TOKEN",
  "brand": "QwQ MTR",
  "brandUrl": null
}
```

An empty `apiBaseUrl` works with the Vite development proxy. Replace
`REPLACE_WITH_PUBLIC_API_TOKEN` with a Server-issued public diagnostics token
before opening the workbench. Initialize a fresh Server with the bootstrap-token
procedure in Quick Start (an initialized store does not need that file), then
start Server and the frontend in separate terminals:

```sh
go run ./cmd/server -config configs/server.sqlite.yaml
pnpm --dir web dev
```

Production build:

```sh
pnpm --dir web typecheck
pnpm --dir web test
pnpm --dir web build
```

Build the frontend container image:

```sh
docker build -f Dockerfile.web -t mtr-web:v1.2.3 --build-arg VERSION=v1.2.3 .
```

Pass `COMMIT` to include the same short revision shown by the Server:

```sh
docker build -f Dockerfile.web -t mtr-web:v1.2.3 \
  --build-arg VERSION=v1.2.3 \
  --build-arg COMMIT=$(git rev-parse HEAD) .
```

The frontend image serves the Vite build with Caddy on unprivileged port `8080`
and runs as UID/GID `65532:65532`. At runtime it loads `/config.json`, so
deployments can replace that single file without rebuilding the image. The
image proxies same-origin `/v1` requests to `MTR_SERVER_URL`, which defaults to
`http://mtr-server:8080`. Leave `apiBaseUrl` empty for that recommended layout,
or set it to a browser-reachable Server API origin with the required
cross-origin policy:

```json
{
  "apiBaseUrl": "https://mtr-api.example.com",
  "apiToken": "REPLACE_WITH_PUBLIC_API_TOKEN",
  "brand": "QwQ MTR",
  "brandUrl": "https://mtr.example.com"
}
```

Omit `brandUrl` to keep the header brand as an in-app home link, set it to a
URL to send every deployment to one canonical brand address, or set it to
`null`/`""` to render the brand without a link.

The Web client reads `apiToken` from `config.json` and automatically sends it as
an `Authorization: Bearer` header on API and streaming requests, so visitors do
not need to enter a token. This value is a public client credential: every
browser user can read it from `/config.json` or developer tools. Give it only
the minimum tools and Agent scope needed for public diagnostics, with
`all: false`, `schedule_access: none`, and `manage_access: none`. Never publish
the bootstrap/admin token, a management token, or a schedule-write token here.
Assume the public token can be copied and abused; Server authorization and rate
limits are the security boundary.

For a container deployment, prepare that JSON file outside the repository and
mount it over the image default; it must be readable by UID `65532`:

```sh
docker run --rm -p 8081:8080 \
  --mount type=bind,src=/absolute/path/config.json,dst=/usr/share/caddy/config.json,readonly \
  ghcr.io/ztelliot/mtr-web:0.1.0
```

The Kubernetes examples include `deploy/k8s/web.yaml`, which deploys
`ghcr.io/ztelliot/mtr-web:0.1.0`, exposes it as the `mtr-web` Service, and
mounts `mtr-web-config` over `/usr/share/caddy/config.json`. The `0.1.0` image
names are release targets, not an assertion that the public registry already
contains them. Publish those images or load locally built images into the
cluster first. Before exposing Web, replace
`REPLACE_WITH_PUBLIC_API_TOKEN` in the ConfigMap with a public diagnostics token.
It intentionally remains a ConfigMap rather than a Secret because the browser
must receive the value and cannot keep it confidential. For a local cluster
smoke test, create the Secrets described in the Kubernetes deployment section
and apply the baseline resources:

```sh
kubectl apply -k deploy/k8s
kubectl -n mtr port-forward svc/mtr-web 8081:80
```

If you access the workbench at `http://localhost:8081`, leave `apiBaseUrl`
empty: the Web Pod proxies `/v1` to the in-cluster `mtr-server` Service, and the
client uses the preconfigured public token automatically. Override
`MTR_SERVER_URL` on the Web container if the internal Server address differs.

The baseline Caddy proxy overwrites `X-Mtr-Proxy-Client-Ip` with its direct
peer address. Server trusts that header only because `mtr-server-ingress`
allows HTTP traffic solely from the Web Pod. This trust model is safe only when
the cluster CNI actually supports and enforces NetworkPolicy. If it does not,
replace the `0.0.0.0/0` and `::/0` trusted ranges with the real Web Pod CIDR or
another narrowly scoped proxy range before deployment. If you add an Ingress,
an external Agent, or another Server client, update the NetworkPolicy and
trusted proxy chain together; never expose this baseline broad trusted-proxy
setting without the network isolation. An Ingress is not a drop-in addition:
the baseline policy blocks a controller from reaching Server directly, and
Caddy derives the client header from its direct peer. To retain the end-user IP
behind an Ingress, explicitly configure Caddy to trust only that controller's
forwarding headers before extending the NetworkPolicy.

The `deploy/k8s/agent.yaml` DaemonSet can derive per-node Agent metadata from
Kubernetes Node annotations without teaching the Agent binary about Kubernetes.
An init container reads the current Node annotations, renders a normal
`agent.yaml` into an `emptyDir`, and the Agent container starts with that
generated config. The ServiceAccount only needs read-only `get nodes` permission
for the init container. By default, the init container builds the API address
from Kubernetes' injected `KUBERNETES_SERVICE_HOST` and
`KUBERNETES_SERVICE_PORT_HTTPS` variables. Set the optional
`MTR_KUBERNETES_API_SERVER` override only when the cluster requires a different
address. The example maps these annotations by default:

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

Change `render-agent-config.sh` in `mtr-agent-config` if your cluster already
uses different annotation names. Schedule labels are managed on the server, not
reported by the Agent. `protocols` uses the same bitmask as Agent config: `1`
for IPv4, `2` for IPv6, and `3` for both. `capabilities` is a comma-separated
tool list. If an annotation is missing, the init container writes the fallback
value into the generated config.

The workbench can create `ping`, `traceroute`, `mtr`, `http`, `dns`, and
`port` jobs, list Agents, and stream structured job events from
`/v1/jobs/<job-id>/stream`.
Fan-out tools expose a node-set selector backed by managed Agent tags; an empty
selection means every eligible node, while multiple tags use OR semantics.
Ad-hoc `traceroute` and `mtr` jobs require an explicit Agent selection because
the server requires `agent_id` for those tools. Scheduled jobs use
`schedule_targets` labels instead of a single `agent_id`.

## HTTP Access Logs

The Server writes one structured JSON access log after every HTTP request,
including public endpoints, authenticated APIs, rejected authentication,
rate-limited requests, unmatched routes, `/metrics`, and long-lived streams when
they close. Access records use the `http access` message and include:

- `request_id`, method, route template, escaped path, status, and duration;
- request bytes read, response bytes written, declared content length, and
  request/response content types;
- resolved client IP, remote address, host, protocol, user agent, authentication
  result, and the hashed API-token subject;
- a `panicked` flag so failed handlers remain visible even if the HTTP server
  recovers the panic outside the router.

The Server accepts a safe `X-Request-ID` value and returns it in the response;
otherwise it generates a UUID. Query strings are intentionally excluded from
access records because arbitrary application parameters may contain sensitive
values. Set `log_level: info` or `debug` to emit access logs.

## Prometheus Watch Metrics

The Server exposes Prometheus metrics at `/metrics` so Watch history and alerts
can move to Prometheus and Grafana. Each scrape exports the latest **completed**
result for every Agent in the current schedule revision. A new run that is still
active does not temporarily hide the previous completed result. Agent
online/enabled state, schedule crontab metadata, and configured node labels are
also exported.

The same endpoint exports HTTP access statistics:

- `mtr_http_requests_total` by normalized method, route template, status code,
  and authentication result;
- `mtr_http_request_duration_seconds`, `mtr_http_request_size_bytes`, and
  `mtr_http_response_size_bytes` histograms by method and route;
- `mtr_http_requests_in_flight` by method.

Unknown methods are labeled `OTHER` and unmatched routes are labeled
`unmatched`, preventing raw paths from creating unbounded Prometheus series.
Standard `go_*` runtime and `process_*` process metrics are exported as well.

When API tokens are configured, Prometheus must send a token with
`schedule_access: read` (or `write`). For example:

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

Set `scrape_interval` often enough to capture the Watch cron cadence. The
exporter exposes the latest completed state, so Prometheus cannot backfill
intermediate runs when several finish between two scrapes.

The primary metric groups are:

- Common result state: `mtr_watch_result_success`,
  `mtr_watch_result_timestamp_seconds`, and
  `mtr_watch_result_duration_seconds`.
- Ping packet loss/count/RTT, HTTP phase timing/status/download size, DNS record
  count, and TCP port state/connect timing.
- Traceroute/MTR per-hop RTT/loss/probe count plus complete route information.

Prometheus sample values are numeric only. Text is represented by info metrics
whose value is always `1`: `mtr_watch_dns_record_info` stores DNS records,
`mtr_watch_route_path_info` stores complete paths, and
`mtr_watch_route_hop_info` stores hop addresses. Text labels include a stable
hash; values longer than 2048 characters are shortened and marked with
`truncated="true"`. Per-run `job_id` values are intentionally never used as
labels.

## Agent/Server mTLS

`register_token` covers Agent registration authorization, while
`tls.ca_files`, `tls.cert_file`, and `tls.key_file` protect the gRPC control
plane between Agent and Server. To enable mutual TLS:

- Agent may set `tls.enabled: true` without `ca_files` to use the operating
  system trust store. This is useful when connecting to a public TLS endpoint
  such as a Cloudflare-proxied hostname.
- Server must set `tls.ca_files`, `tls.cert_file`, and `tls.key_file` together.
  If only the server certificate/key are configured, the channel is TLS but
  Agents are not required to present a client certificate.
- Agent must also set `tls.ca_files`, `tls.cert_file`, and `tls.key_file`
  together. If only `ca_files` is set, the Agent verifies the Server but does
  not present its own client certificate.
- In production, use both `register_token` and mTLS: token-based registration
  for logical authorization, mTLS for transport-level mutual authentication.

The current implementation verifies that the client certificate chains back to
the configured CA, but it does not bind one certificate to one fixed Agent ID.
That means multiple Agents may share the same client certificate, and the
Kubernetes example manifests do exactly that. Shared certificates are fine as
long as each Agent still has a unique `id` and you accept that certificate
rotation becomes an all-Agents operation if one pod is compromised.

Generate a CA, a Server certificate, and one shared client certificate with
OpenSSL:

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

If Agents connect to the Server with a different DNS name or an IP address,
add the matching `DNS:` or `IP:` entries to `subjectAltName`.

Server and Agent TLS config example:

```yaml
tls:
  enabled: true
  ca_files:
    - "/var/run/mtr/tls/ca.crt"
  cert_file: "/var/run/mtr/tls/tls.crt"
  key_file: "/var/run/mtr/tls/tls.key"
```

### Cloudflare gRPC reverse proxy

The gRPC control plane can be placed behind [Cloudflare's proxied gRPC support](https://developers.cloudflare.com/network/grpc-connections/)
when the Cloudflare requirements are met: enable gRPC for the zone, use a
proxied hostname, keep SSL/TLS mode at least Full, and expose the origin gRPC
endpoint on port 443 with TLS and HTTP/2/ALPN. This project uses grpc-go over
HTTP/2 and sends `application/grpc+json`, which matches Cloudflare's accepted
gRPC content-type pattern.

One practical layout:

```yaml
# server.yaml on the origin
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

To support both direct Agents and Cloudflare-proxied Agents on the same origin,
configure Cloudflare Authenticated Origin Pulls (prefer a zone-level or
per-hostname custom certificate for account-specific authentication), then trust
both the direct Agent CA and the Cloudflare origin-pull CA:

```yaml
# server.yaml on the origin
grpc_addr: ":443"
tls:
  enabled: true
  ca_files:
    - "/var/run/mtr/tls/agent-ca.crt"
    - "/var/run/mtr/tls/cloudflare-origin-pull-ca.crt"
  cert_file: "/var/run/mtr/tls/tls.crt"
  key_file: "/var/run/mtr/tls/tls.key"
```

Direct Agents keep their client certificate config and connect to the origin
address. Cloudflare-proxied Agents connect to the proxied hostname and normally
do not set Agent `cert_file`/`key_file`; Cloudflare presents the origin-pull
client certificate to Server instead. Keep `register_token` strong because
Cloudflare terminates the Agent-facing TLS connection and normal Agent client
certificates are not passed through the reverse proxy. Cloudflare Access does
not protect gRPC traffic through this reverse-proxy mode; use another
authentication layer for sensitive origins. Cloudflare Tunnel public hostnames
are also not the same thing as proxied gRPC here; [Cloudflare documents gRPC support for Tunnel](https://developers.cloudflare.com/cloudflare-one/networks/connectors/cloudflare-tunnel/use-cases/grpc/)
via private subnet routing, not public hostname deployments.

## Security Notes

Production deployments should place Anubis or another browser-facing gateway in front of the REST API for PoW/challenge handling, set mTLS certificate paths in both configs, rotate API and Agent tokens, and tune per-tool policies before exposing the service.

Agents execute diagnostics with native Go implementations and only upload structured result events.

Connection-oriented probes reject targets that resolve to loopback, private,
link-local, multicast, carrier-grade NAT, documentation, benchmarking, or other
special-use address ranges at both Server and Agent. The `dns` tool is
deliberately different: it queries the requested record name without first
connecting to the returned address, so it can reveal private records visible to
the Agent's resolver. Treat DNS tool permission as access to that resolver's
namespace and scope public tokens accordingly.

The Agent image only needs the compiled Agent binary and CA certificates; diagnostic tools are implemented in Go.

## Docker Hardening

For container deployments, prefer a non-root user, use a read-only root
filesystem, disable privilege escalation, and explicitly reduce Linux
capabilities. Server does not need extra capabilities. Agent needs `NET_RAW`
for ICMP ping and for ICMP or TCP traceroute/MTR; UDP probes and TCP ping do
not need it.

The Server example uses `65532:65532` as an unprivileged container UID/GID. The
ICMP Agent example uses UID 0 with every capability dropped except `NET_RAW`,
because Docker does not place `--cap-add NET_RAW` in a non-root process's
permitted/effective sets. An Agent restricted to UDP/TCP ping can instead use
the image default UID/GID `65532:65532` and omit `--cap-add`. Mounted config and
certificate files must be readable by the selected UID/GID. For private keys,
prefer mode `0640` with the matching group, or use Docker/Compose secrets. The
published container images do not include Server or Agent runtime config; mount
those files explicitly. Before the first Server start, place a random token in
`/etc/mtr/bootstrap/admin-token`, make it readable by UID/GID 65532 but not by
other users, and remove it after the persistent store is initialized. The
sample PostgreSQL DSN in `configs/server.yaml` names
`/var/run/mtr/postgres/ca.crt` as `sslrootcert`, so the example also mounts that
CA at the exact path; make the host file readable by UID/GID 65532. Omit this
mount only when using SQLite or a PostgreSQL setup whose DSN uses another
verified CA location.

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

If Server uses SQLite, add a writable state mount and point `database_url` at a
file under that directory:

```sh
  -v /var/lib/mtr:/var/lib/mtr \
  --workdir /var/lib/mtr
```

The Agent in gRPC mode initiates the long-lived connection to Server and does
not need an exposed port. Set the Agent's unique `id`, `server_addr`, and
`register_token` in `/etc/mtr/agent.yaml`; config file values take precedence
over `MTR_` environment variables. Only add `-p 9000:9000` when running
`mode: http` or `mode: grpc,http` and intentionally exposing `/invoke`, ideally
behind a controlled gateway. Avoid `--network host` unless you explicitly need
the host network perspective.

## systemd Hardening

The units in `systemd/mtr-server.service` and `systemd/mtr-agent.service` now
include a baseline sandbox. Both run as `User=mtr`/`Group=mtr`, use read-only
system paths, private `/tmp`, no privilege escalation, restricted address
families, and a system call filter. Server keeps an empty capability set.
Agent keeps only `CAP_NET_RAW` through `CapabilityBoundingSet` and
`AmbientCapabilities`.

Create a dedicated system user and keep configs/certificates readable only by
root and the `mtr` group:

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

The Server unit points at this bootstrap file. After the persistent store is
initialized and the required managed tokens are saved, remove
`/etc/mtr/bootstrap/admin-token`; an initialized store continues to start
without it.

If Server uses SQLite, put the database under `/var/lib/mtr/`. The Server unit
sets `WorkingDirectory=/var/lib/mtr` and `StateDirectory=mtr`, making that the
intended persistent writable state directory.

Use systemd's analyzer to inspect the resulting sandbox score and remaining
risk:

```sh
systemd-analyze security mtr-server.service
systemd-analyze security mtr-agent.service
```

## Kubernetes Deployment

The repository now includes a baseline manifest set under `deploy/k8s/`:

- `server.yaml`: single-replica Server `Deployment` plus `Service`
- `agent.yaml`: Agent `DaemonSet`, with each pod using its own pod name as `MTR_ID`
- `networkpolicy.yaml`: denies inbound Agent traffic and restricts Server HTTP
  and gRPC ingress to the baseline Web and Agent pods
- `secrets.example.yaml`: Secret templates with placeholders you should replace
- `kustomization.yaml`: baseline resources for `kubectl apply -k deploy/k8s`; it
  intentionally does not include `secrets.example.yaml`

These manifests assume:

- PostgreSQL is provided externally. Its TLS-verified `database-url` is injected
  through `mtr-server-env`, and its CA certificate is mounted from
  `mtr-server-postgres-ca`.
- The gRPC control plane uses `mtr-server.mtr.svc.cluster.local:8443`, so the example Server certificate SANs match that service DNS name.
- All Agents share one client certificate, while each pod keeps a unique logical identity through `MTR_ID=metadata.name`.
- Agent pods read their registration token from the `mtr-agent-env` Secret, and
  that value must match an Agent register token stored in Server managed
  settings.
- Server stays at `replicas: 1`. The scheduler hub holds in-process connection state, so this baseline does not claim horizontal control-plane scaling.
- API tokens, Agent register tokens, rate limits, global settings, label-based
  scheduler/runtime settings, label policies, and outbound Agents are
  stored in Server managed settings. They are not read from the Kubernetes
  Server ConfigMap or Secret.
- A fresh store reads its initial admin API token once from the optional
  `mtr-server-bootstrap` Secret. The token is never printed to Server logs.
- The CNI supports and enforces NetworkPolicy. Without that enforcement, replace
  the Server ConfigMap's broad trusted-proxy ranges with the actual Web Pod CIDR
  or another narrowly scoped proxy range before applying the manifests.

Bootstrap is intentionally staged: create only the Server prerequisites and
start Server first. Use the initial admin token from a trusted local client to
create both a public Web diagnostics token and an Agent register token. Only
then configure Web and deploy the Web and Agent workloads. This avoids
publishing an admin token to browsers or requiring managed tokens before Server
exists. You can adapt the placeholders in `deploy/k8s/secrets.example.yaml`, or
create the first-stage Secrets directly:

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

Apply only Server in the first stage, then keep a local port-forward open in
one terminal:

```sh
kubectl apply -f deploy/k8s/server.yaml
kubectl -n mtr rollout status deployment/mtr-server
kubectl -n mtr port-forward service/mtr-server 8080:8080
```

The value in the local `admin-token` file is the initial admin API token. In
another terminal, create a deliberately narrow public token (this example
allows only `ping`) and the rollout's Agent register token:

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

The generated values are revealed once as `.token.secret` in the first response
and `.token.token` in the second. Adjust the public token's tools and Agent scope
to the deployment, and set `remaining_uses` and `allowed_agent_ids` for the
actual rollout. Put the public value into a private copy or overlay of
`mtr-web-config`; never put the admin token there. Store the register value in
the Agent Secret, then deploy the remaining workloads:

```sh
kubectl -n mtr create secret generic mtr-agent-env \
  --from-literal=register-token='<managed-register-token>'
kubectl apply -f '<private-web-manifest>' \
  -f deploy/k8s/agent.yaml \
  -f deploy/k8s/networkpolicy.yaml
kubectl -n mtr rollout status deployment/mtr-web
```

After the persistent database has been initialized and the required managed
tokens have been saved, delete the local admin and one-time response files and
the bootstrap Secret; the optional volume may remain absent on later restarts.
Future reconciliations may use a private Kustomize overlay based on
`deploy/k8s`; do not reapply the checked-in Web placeholder over the deployed
public token.

The Agent manifest is intentionally tight on privileges:

- the main Agent container does not mount a ServiceAccount token
- no host network/PID/IPC and `privileged: false`
- read-only root filesystem, with only read-only config/cert mounts and one
  `emptyDir` mounted at `/tmp`
- all Linux capabilities dropped except `NET_RAW`
- privilege escalation disabled and the runtime-default seccomp profile enabled
- a NetworkPolicy denies inbound traffic to Agent pods

`NET_RAW` is the one deliberate exception because the current ICMP-backed
implementations of `ping`, `traceroute`, and `mtr` need raw sockets. The main
Agent container runs as UID 0 so `NET_RAW` can enter the permitted and effective
capability sets, but it is not privileged and all other capabilities are
dropped. If your cluster enforces strict Pod Security Admission, plan for an
explicit exception for this Agent security context. The Agent logs `CapEff`,
`CapBnd`, `NoNewPrivs`, and `Seccomp` at startup to make the effective runtime
privileges visible.

The DaemonSet example has one narrow RBAC rule for the init container that
renders per-node config from Node annotations: `get nodes`. The generated config
is written to an `emptyDir`; the main Agent container then runs without the
ServiceAccount token mounted.

The baseline does not include a default-deny egress `NetworkPolicy`. The Agent
is supposed to probe arbitrary external targets, and standard Kubernetes
`NetworkPolicy` support for ICMP is limited; if your CNI offers richer ICMP or
egress controls, it is worth tightening that layer further.

## FaaS Deployment

FaaS deployment assets live in `deploy/fc/`. The checked-in `targets.json`
describes public target metadata and placeholder runtime settings; `generate.mjs`
turns that file into provider-specific deployment templates.

The public template's top-level HTTP settings select `header` authentication
with `X-MTR-Agent-Token`. Set its secret only in your local targets file, then
use the same header name and secret in the Server outbound Agent's `auth`
object. Huawei Cloud FunctionGraph is the exception: that provider overrides
these settings with `event` mode behind the FunctionGraph IAM invocation
boundary and does not use this application-level HTTP secret.

Supported targets:

- `aliyun`: Alibaba Cloud Function Compute, using the Serverless Devs `fc3`
  component.
- `ctyun`: CTYun Function Service, using the Serverless Devs `faas-cf`
  component.
- `huaweicloud`: Huawei Cloud FunctionGraph, automatically pulling a source
  image, pushing it to SWR, and deploying through the official KooCLI.
- `qcloud`: Tencent Cloud SCF, using Serverless Cloud Framework.
- `gcloud`: Google Cloud Run, using `gcloud alpha run deploy`.
- `aws`: AWS Lambda with Lambda Web Adapter, private ECR images, and streaming
  Function URLs, using AWS CLI v2.
- `azure`: Azure Container Apps Consumption, using the existing Agent image and Azure CLI.

Keep real deployment values out of `targets.json`. For production, copy it to a
local file such as `.targets.json`, fill in values like `MTR_HTTP_AUTH_SECRET`,
`MTR_HTTP_PATH_PREFIX`, Alibaba Cloud log project IDs, and private image
locations, then generate templates from that local file:

```bash
cd deploy/fc
node generate.mjs --config .targets.json
```

You can also select the config with `FC_TARGETS_FILE`. When the generator reads
the default `targets.json`, it rejects values that look like real tokens, path
prefixes, Alibaba Cloud log project IDs, Huawei Cloud project IDs, or agency
names, as well as Huawei Cloud SWR, VPC, logging, and KMS deployment
identifiers. This keeps the public template safe even if someone edits it by
accident.

Set `binary.path` to point at a built local Agent binary, or configure
`binary.fromImage` to extract the binary from a Docker image. Generated output
is written under `deploy/fc/output/`:

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
output/aws/targets.json
output/aws/aws-lambda-deploy.mjs
output/aws/deploy.sh
output/azure/targets.json
output/azure/azure-deploy.mjs
output/azure/deploy.sh
```

The FaaS Agent does not read `config.yaml`. It is configured entirely through
environment variables. Every generated target receives `MTR_COUNTRY`, `MTR_ID`,
`MTR_ISP`, `TZ`, and `MTR_REGION`; runtime settings come from top-level `env`
and may be overridden by provider-level or target-level `env` objects.

After deployment, generate PostgreSQL inserts for outbound Agents from the repository root:

```bash
node deploy/fc/generate.mjs sql --provider all
```

The script reads `deploy/fc/.targets.json` when present, otherwise public `targets.json`,
and writes `deploy/fc/output/outbound-agents.sql`. Use `--config`, `--output`,
`--provider`, and `--regions` to select inputs and targets. Config paths resolve
relative to `deploy/fc/`; output paths resolve relative to the current working
directory. The SQL subcommand does not clear deployment output or extract binaries,
and existing SQL files are not overwritten. The original
`node deploy/fc/generate.mjs --config .targets.json` still generates deployment
assets only. Alibaba Cloud and CTYun use `s <key> info`; Tencent Cloud runs
`scf info` once in the parent output directory; Google Cloud runs
`gcloud alpha run services list --format=json` once; Huawei Cloud queries IAM project IDs;
AWS reads each function's URL with `aws lambda get-function-url-config`.
HTTP Agents use the configured path prefix and merged header/bearer credentials.
Huawei Cloud reuses AK/SK from the existing database Agent `hwc.cn-north-1.fc` by default.
Use `--source-id aliyun=ID,ctyun=ID,huaweicloud=ID` to reuse other database credentials.
If CTYun returns only a private domain, the Server needs network access to it;
set `target.agentUrl` for an explicit address when necessary. The SQL file may contain
HTTP tokens and is written with mode `0600`; do not commit it.
The script only reads deployment information and never executes SQL. Existing Agent IDs
are skipped. Import with `\i deploy/fc/output/outbound-agents.sql` in a psql client
that can access the generated file.

To extract the Agent binary from the published container image:

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

When `binary.fromImage` is set, `generate.mjs` runs `docker pull`, creates a
temporary container, copies the binary out, and writes it to
`output/binary/agent` by default. Use `binary.path` when you already have a
local binary:

```json
{
  "binary": {
    "path": ".cache/mtr-agent"
  }
}
```

`binary.path` and `binary.fromImage` are mutually exclusive. Image extraction
always stages the executable under `output/binary`; every provider output is
also required to remain below `output/`.

Set `binary.fromImage.pull` to `false` when the image is already present
locally.

Deploy Alibaba Cloud and CTYun templates with Serverless Devs:

```bash
cd output
s deploy -t s.aliyun.yaml
s deploy -t s.ctyun.yaml
```

Tencent Cloud SCF is generated as one directory per target because Serverless
Cloud Framework expects each function to own its `serverless.yml`. Shared code
lives in `output/qcloud/code`; the Agent binary is copied to `scf_bootstrap`,
the entry filename expected by SCF:

```text
output/qcloud/code/scf_bootstrap
output/qcloud/example/serverless.yml
```

Deploy Tencent Cloud targets through the generated script, which enters each
target directory and runs `scf deploy`:

```bash
cd output
qcloud/deploy.sh
```

Google Cloud Run also uses a generated script:

```bash
cd output
gcloud/deploy.sh
```

Alibaba Cloud logging is opt-in per target. Set `log: true` and provide
`providers.aliyun.logProjectId` in the local config:

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

If `log` is omitted or false, no `logConfig` is generated. When enabled, the SLS
project name is `serverless-{region}-{providers.aliyun.logProjectId}`. Set
`logStore` on a target to override the default `default-logs` logstore.

Alibaba Cloud targets use the Go official layer by default:

```text
acs:fc:{region}:official:layers/Go1/versions/1
```

Set `layer` to `python-flask` in regions where the Go layer is unavailable:

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

The runtime remains `custom.debian10`; only the generated layer ARN changes:

```text
acs:fc:{region}:official:layers/Python3-Flask2x/versions/2
```

Set `layer` to `null`, `false`, or an empty string to omit the `layers` field:

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

Minimal CTYun and Tencent Cloud targets look like this:

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

Google Cloud Run is image-based. A minimal provider block is:

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

The generator expands that config into a `gcloud alpha run deploy` command with
the image, region, function name, runtime settings, and Agent env vars:

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

Cloud Run defaults are port `9000`, concurrency `1`, timeout `60`, CPU `0.08`,
memory `128Mi`, min instances `0`, max instances `4`, `--allow-unauthenticated`,
`--public`, and `--no-cpu-boost`. Override them with `providers.gcloud.run` or
`target.run`.

Tencent Cloud SCF can deploy from a container image instead of the copied binary.
Build and publish the referenced source tag first; it is not bundled by the
deployment generator. Set `providers.qcloud.image` to enable image mode for all
Tencent Cloud targets:

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

`imageUrl` supports `{registry}`, `{key}`, `{region}`, and `{functionName}`.
`sourceImage` supports `{key}`, `{region}`, and `{functionName}`. A target may
override the provider image object, or set `"image": false` to fall back to the
code package.

`{registry}` is selected from the Tencent Cloud region. Mainland China defaults
to `ccr.ccs.tencentyun.com`; overseas regions use local registry hosts such as
`hkccr.ccs.tencentyun.com` for `ap-hongkong` and
`sgccr.ccs.tencentyun.com` for `ap-singapore`. Use `image.registry` for one
image object, or `image.registries` for selected regions:

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

In image mode, generated Tencent Cloud `serverless.yml` files contain:

```yaml
image:
  imageType: personal
  imageUrl: ccr.ccs.tencentyun.com/sls-scf/mtr-agent:latest
  containerImageAccelerate: true
```

`output/qcloud/deploy.sh` mirrors the source image into the selected Tencent
Cloud registry before deployment:

```bash
docker pull ghcr.io/ztelliot/mtr-agent:0.1.0
docker tag ghcr.io/ztelliot/mtr-agent:0.1.0 ccr.ccs.tencentyun.com/sls-scf/mtr-agent:latest
docker push ccr.ccs.tencentyun.com/sls-scf/mtr-agent:latest
```

Each target image is mirrored once per generated target tag. With the example
above, mainland targets share `ccr.ccs.tencentyun.com/sls-scf/mtr-agent:latest`;
an overseas target such as `ap-singapore` uses
`sgccr.ccs.tencentyun.com/sls-scf/mtr-agent:latest`.

### Huawei Cloud FunctionGraph custom images

`providers.huaweicloud` deploys FunctionGraph v2 custom-image event functions.
The checked-in `targets.json` contains public provider and target templates;
put the agency name, source image, and destination image URL in your local
`.targets.json`. Set your organization in `image.imageUrl`; project IDs are
automatically resolved by default.
For each target, the generator writes
three KooCLI JSON requests for inspecting, creating, and updating the function,
plus a single deployment script:

```text
output/huaweicloud/<target-key>.list.json
output/huaweicloud/<target-key>.create.json
output/huaweicloud/<target-key>.update.json
output/huaweicloud/deploy.sh
output/huaweicloud/hcloud-helper.mjs
```

A minimal provider with one target looks like this; use the checked-in public
template as the source for additional regions:

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

`projectId` is optional. For each region, the script queries
`hcloud IAM KeystoneListProjects/v3` and selects the single
enabled project whose name exactly matches each region. The identity needs
permission to query IAM projects. To select a subproject or avoid that lookup,
set `projectId` explicitly on the provider or target. The script first pushes all
images configured with `image.sync: true`, allowing more time for Huawei Cloud's
cross-region image synchronization. It then completes each region's project
lookup and function deployment before starting the next region. A failed image
push skips only its corresponding region. A failed region prints its error
without blocking later regions; the script summarizes failures and exits with a nonzero status at the
end. Missing or ambiguous projects stop only the current region. The script uses
`ListFunctions` with exact function-name and package matching, updates an
existing function using its returned URN, and creates a missing function.
Failed queries, malformed responses, and ambiguous matches stop the current region
without creating a function. Node.js is required to parse the query response.
The complete `path.project_id` and `path.function_urn` are written to a private
temporary JSON request, avoiding KooCLI's restrictions on mixing CLI and JSON
path parameters. CLI errors such as `[USE_ERROR]` fail deployment even when the
process exits with status zero, and the error type is displayed. Temporary
requests are removed on exit. Keep the generated `hcloud-helper.mjs` beside
`deploy.sh`. See the [IAM project list API](https://support.huaweicloud.com/intl/en-us/api-iam/iam_06_0001.html).
The optional `profile` also supports provider defaults and target overrides;
if omitted, KooCLI uses its default profile.
The image format matches Tencent Cloud: `image.sourceImage` specifies the source
and `image.imageUrl` specifies the destination SWR URL. Replace `<organization>`
with your organization. Both support `{key}`, `{region}`, and `{functionName}`;
`imageUrl` also supports `{registry}`. A target's `image` overrides the provider's
corresponding fields.

`image.sync` controls whether the script uploads the image for that target and
defaults to `true` when omitted. Set it to `false` to skip that region's Docker
pull/tag/push while still creating or updating the function with its `imageUrl`.
The example and public template set the provider default to `false` and override
Beijing 4 to `true` as an example only; adapt this to your platform replication
rules and enable uploads for any other regions that need them. Selected images
are uploaded first, then all functions are deployed. Skipped destinations must
already contain the image. Platform replication may be asynchronous; the script
does not wait for or poll it. If every target disables synchronization, no
`sourceImage` is required and the generated script does not require Docker.

Registry defaults are built in:
`swr.{region}.myhuaweicloud.com` for most regions and
`swr.eu-west-101.myhuaweicloud.eu` for `eu-west-101`. The example needs no
`registry` or `registries` configuration. Use `image.registry` for a single
custom registry or `image.registries` for regional overrides when needed.
The script pulls `linux/amd64` by default; override it with `image.platform`
when needed. For releases, use a unique,
immutable tag; do not repoint the same tag after a release.
Huawei Cloud deployment cannot opt out with
`"image": false`; FunctionGraph requires `agencyName` for every container-image
function, including images marked public. FunctionGraph does not allow root or
other reserved IDs, so the generated `custom_image` uses its recommended
UID/GID `1003` by default; overrides in `image.uid` and `image.gid` are checked
against Huawei Cloud's valid range. A non-root FunctionGraph runtime cannot be
assumed to have raw-socket privileges. Prefer the Agent's `protocol=auto` for
ICMP requests so it can fall back to UDP/TCP, and verify a real invocation
before production rollout.

Every region in the public template has FunctionGraph v2 and SWR endpoints,
but Huawei Cloud does not publish a complete per-region custom-image matrix
and directs users to the console for current feature availability. Before a
region's first deployment, confirm that FunctionGraph offers the container
image creation method there. The deployment script surfaces an unsupported
Create/Update error and does not silently fall back.

Huawei Cloud functions default to a `600`-second timeout, `400` concurrent
requests per instance (`strategy_config.concurrent_num`), and at most `1`
instance (`strategy_config.concurrency`). Override these in the provider's or
target's `function` object. Only `UpdateFunctionConfig` accepts `strategy_config`,
so create requests omit it and the deployment script applies the configuration
after creating a function, as well as when updating an existing one. These are
cloud-side settings; the Agent and Server application-level job concurrency
limits remain `4`.

For regions that reject `concurrent_num`, set it to `null` in that target:

```json
{
  "function": {
    "strategy_config": {
      "concurrent_num": null
    }
  }
}
```

Strategies merge in this order: defaults, provider, then target. Numbers override
inherited values; `null` omits the field from the API request. The example removes
only `concurrent_num` and retains the inherited `concurrency` instance limit.
Use `"concurrency": null` to omit that parameter, or `"strategy_config": null`
to omit the entire strategy. An empty strategy is omitted as well. Omission does
not send an instruction to clear or reset the existing cloud-side setting.

The generator sets `network_controller.disable_public_network=false` by
default so the Agent can reach public probe targets. Provider- or target-level
`function` overrides may tune memory, timeout, VPC, logging, and other fields
accepted by both CreateFunction and UpdateFunctionConfig. Apart from the
separately handled `strategy_config`, fields supported by only one API are
rejected during generation, and common value types and ranges
are validated up front. Configuring `function.func_vpc` disables
FunctionGraph's default network interface, so that VPC needs NAT/EIP for
continued public Internet access.

Complete these prerequisites before deployment:

- For regions with `image.sync: true`, install Docker and log in to the registry
  using the SWR console's login command. The script pushes to the organization
  in `image.imageUrl`; ensure it is available and the identity can push there.
  For `image.sync: false` regions, complete platform replication beforehand;
  no local Docker login is needed for those destinations.
- Install Huawei Cloud KooCLI, run `hcloud configure init`, and ensure that the
  selected profile can inspect, create, and update FunctionGraph functions.
  Keep the KooCLI AK/SK in its local configuration, not in the targets file.
- Create a FunctionGraph cloud service agency with the `SWR Admin` permission,
  then set its name as the provider's `agencyName` (rendered as `xrole` in the
  FunctionGraph request). FunctionGraph needs this agency for container-image
  functions; it is distinct from the KooCLI identity that runs the deployment.

Generate and deploy from `deploy/fc`:

```bash
node generate.mjs --config .targets.json
bash output/huaweicloud/deploy.sh
```

`deploy.sh` runs `docker pull`, `docker tag`, and `docker push` only for targets
with `image.sync: true`, then calls the official
`hcloud FunctionGraph` commands to create a missing function or update the
configuration and image of an existing one.
The JSON files and script are generated artifacts: do not edit or commit them.
Consecutive targets using the same source and platform share a single pull, and
each destination tag is pushed once. Generation fails if different sources or
platforms map to the same upload destination tag, preventing an incorrect deployment.

Environment variables are merged in this order: global `env`, provider `env`,
provider `envs[region]` (or `envs[key]` if the region is absent), then target `env`.
Later values override matching keys, unspecified keys are inherited, and `null`
removes a key. The generator enforces the FunctionGraph protocol settings below
after that merge.

Huawei Cloud targets override the top-level HTTP settings, run in `event` mode,
and listen on port `8000`, as required for FunctionGraph custom images. The
container serves `POST /init` and `POST /invoke`. Its
`MTR_HTTP_AUTH_TYPE` is `none` because the FunctionGraph IAM API boundary
authenticates external invocation; do not expose the container port
anonymously by another route. On the Server side, use the Huawei Cloud Event
Agent configuration described above: `transport: event`,
`auth.type: huaweicloud_aksk`, the `X-CFF-Request-Version: v1` header, and
`event_response: result_field`. The outbound Agent `id` must match the target
`id`.

Huawei Cloud provides an official Serverless Devs `fgs` component, but its
current [YAML reference](https://support.huaweicloud.com/intl/en-us/devg-functiongraph/functiongraph_02_0725.html)
only lists `inline`, `zip`, `obs`, and `jar` for `codeType`; it does not expose
the `Custom-Image-Swr` mode required here. Huawei Cloud image deployment
therefore cannot use `s deploy` and this repository uses KooCLI instead. This
can be unified with the other Serverless Devs providers if the component gains
custom-image support. See also Huawei Cloud's official documentation for
[creating a function with an image](https://support.huaweicloud.com/intl/en-us/usermanual-functiongraph/functiongraph_01_1047.html),
[FunctionGraph's region availability guidance](https://support.huaweicloud.com/intl/en-us/productdesc-functiongraph/functiongraph_01_0141.html),
[managing functions using KooCLI](https://support.huaweicloud.com/intl/en-us/usermanual-functiongraph/functiongraph_01_0396.html),
the [CreateFunction API](https://support.huaweicloud.com/intl/en-us/api-functiongraph/functiongraph_06_0108.html),
the [UpdateFunctionConfig API](https://support.huaweicloud.com/intl/en-us/api-functiongraph/functiongraph_06_0111.html),
and [configuring agency permissions](https://support.huaweicloud.com/intl/en-us/usermanual-functiongraph/functiongraph_01_0920.html).

### AWS Lambda

`providers.aws` deploys HTTP Agents with [Lambda Web Adapter](https://github.com/aws/aws-lambda-web-adapter).
The separate image `ghcr.io/ztelliot/mtr-agent-lambda` provides `dev-amd64` and
`dev-arm64`, plus corresponding version and `latest` tags. Each tag contains
one architecture. Defaults are 128 MB and `linux/arm64`; targets may override
`image.platform` to `linux/amd64`. Image sync copies only each target's selected
architecture. Build it manually with:

```bash
docker buildx build -f Dockerfile.agent --target lambda \
  --platform linux/arm64 --provenance=false --sbom=false \
  -t ghcr.io/ztelliot/mtr-agent-lambda:dev-arm64 --push .
```

Install Docker, Node.js, and AWS CLI v2, and configure AWS credentials locally.
Use the `providers.aws` template in `targets.json`; fill in `roleArn`, the
same-region ECR `image.imageUrl`, and the HTTP token in your local `.targets.json`.
The execution role must trust Lambda. CloudWatch logging is optional; omit
`AWSLambdaBasicExecutionRole` and other CloudWatch Logs permissions when logs
are not needed. The deployment identity needs Lambda/ECR permissions and
permission to pass that role.

```bash
cd deploy/fc
node generate.mjs --config .targets.json
bash output/aws/deploy.sh
node generate.mjs sql --config .targets.json --provider aws
```

Deployment syncs the image to ECR and creates or updates the function and its
streaming Function URL. Server uses `transport: http` and the Agent token.
For route probes, use `udp` or `auto`; ICMP and the current TCP traceroute/MTR
implementation require raw sockets.

### Azure Container Apps

`providers.azure` runs `ghcr.io/ztelliot/mtr-agent:dev` directly on the Consumption
profile. No Storage Account, Functions host, or extra image build is needed.
Defaults are 0.25 CPU, 0.5 GiB, `run.minReplicas: 0`, and `run.maxReplicas: 1`.
Logs are not stored; no Log Analytics workspace or Application Insights is created.

Install Azure CLI and Node.js. In your local `.targets.json`, configure
`subscription`, `resourceGroup`, `environment`, and the HTTP token. Templates
use names like `mob-hongkong` and IDs like `azr.ap-east-1.fc`.

```bash
az login
cd deploy/fc
node generate.mjs --config .targets.json
bash output/azure/deploy.sh
node generate.mjs sql --config .targets.json --provider azure
```

Targets deploy one at a time; failures don't block later targets. The script
creates or reuses a log-free environment and uses single revision mode.
Server uses `transport: http` and the existing Agent token. `run.httpConcurrency`
is the HTTP autoscaling threshold, not a hard request concurrency limit.

## Acknowledgements

Thanks to OpenAI Codex for collaborative support during development, refactoring, testing, and documentation.
