# MTR Agent/Server

Distributed network diagnostics service written in Go.

Chinese version: [README.zh-CN.md](README.zh-CN.md)

## Components

- `cmd/server`: cloud-side REST API, gRPC Agent control plane, PostgreSQL storage, policy, and rate limiting.
- `cmd/agent`: edge-side worker with two modes: `grpc` connects to Server over the long-lived control plane, and `http` exposes an HTTP invoke endpoint for gateway-forwarded or function deployments.

## Quick Start

For local testing without PostgreSQL:

```sh
go run ./cmd/server -config configs/server.sqlite.yaml
```

Server reads `/etc/mtr/server.yaml` by default when present, otherwise
`configs/server.yaml`. Use `-config` to point at another file.

On first startup, Server will create the SQLite database file automatically,
and for PostgreSQL it will try to create the target database and required
tables when the configured account has permission to do so.

Agents read config from `-config`, matching Server startup:

```sh
./mtr-agent -config /etc/mtr/agent.yaml
```

Both binaries support `-version`. Build metadata can be injected with Go
ldflags, for example:

```sh
go build -ldflags "-X github.com/ztelliot/mtr/internal/version.Version=v1.2.3 -X github.com/ztelliot/mtr/internal/version.Commit=$(git rev-parse --short HEAD)" ./cmd/server
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

Agent runtime settings, including `mode`, `http_addr`, identity, register token,
HTTP token, capabilities, protocols, TLS, and speed-test limits, live in the Agent YAML.
`protocols` is a bitmask: `1` means IPv4, `2` means IPv6, and `3` means both.
Jobs may set `ip_version` to `4` or `6`; tasks are only dispatched to Agents
whose protocol mask supports the requested protocol.

Every Server and Agent config field can also be supplied through environment
variables. Environment variables use the YAML path with a `MTR_` prefix and
upper-case underscores, for example `tls.ca_file` becomes `MTR_TLS_CA_FILE`,
`runtime.http_timeout_sec` becomes `MTR_RUNTIME_HTTP_TIMEOUT_SEC`, and
`speedtest.max_bytes` becomes `MTR_SPEEDTEST_MAX_BYTES`. Config file values win
when both sources set the same field. Lists of strings may be comma-separated
or YAML/JSON arrays, and map/list fields such as `outbound_agents`,
`tool_policies`, and `rate_limit.tools` may be supplied as YAML/JSON fragments.

Server scheduling controls live in the server config under `scheduler`: `agent_offline_after_sec` marks stale gRPC Agents offline, `grpc_max_inflight_per_agent` controls concurrent jobs per connected gRPC Agent, and `outbound_max_inflight_per_agent` does the same for HTTP outbound Agents. Runtime knobs live under `runtime`, including probe `count`, `max_hops`, per-hop `probe_step_timeout_sec`, tool timeout presets, DNS resolve timeout, HTTP outbound invoke attempts, and outbound recovery health-check backoff. Set `log_level: debug` in the Server or Agent config for verbose scheduling and execution logs.

Server can also actively invoke HTTP Agents configured under `outbound_agents`
with `id`, `base_url`, and `http_token`. Each outbound Agent exposes `/invoke`
when Agent config sets `mode: http` or `mode: grpc,http`; Server probes
`/healthz` once at startup to learn version, region, provider, capabilities,
protocols, and redaction settings, then uses `/healthz` again only during
recovery after connection failures.

HTTP Agent contract:

```sh
mode: "http"
id: "edge-fc-1"
http_addr: ":9000"
http_token: "change-me-http-token"
```

To run both the long-lived gRPC Agent and HTTP Agent in one process, use
`mode: "grpc,http"`.

```sh
curl -N -X POST http://localhost:9000/invoke \
  -H 'Authorization: Bearer change-me-http-token' \
  -H 'Accept: application/x-ndjson' \
  -H 'Content-Type: application/json' \
  -d '{"id":"job-1","tool":"ping","target":"1.1.1.1","ip_version":4}'
```

The response is compact newline-delimited JSON (`application/x-ndjson`): each line is a structured progress/hop/metric event or the final summary event. Server outbound mode restores only the browser-facing envelope needed for routing and replay, such as `job_id` and `agent_id`; it does not re-add repeated task-level fields like `tool`, `target`, or protocol to incremental or summary events. gRPC mode uses the same compact idea on the Agent-to-Server path. For Alibaba Cloud FC, Tencent Cloud SCF, or Huawei FunctionGraph, deploy `Dockerfile.agent` as a custom container or HTTP function and route trigger traffic to `/invoke` on port `9000`; the image still runs the unified `mtr-agent` binary.

Agent speed test endpoint:

```sh
curl -o /dev/null 'http://localhost:9000/speedtest/random?bytes=10485760&token=change-me-http-token'
```

The speed test endpoint is only available when HTTP mode is enabled, on
`http_addr`. It is protected by the Agent `http_token`, supplied as the `token`
query parameter. Configure limits under `speedtest` in the Agent config, or set
`speedtest.max_bytes: 0` to disable the endpoint.

Create a job:

```sh
curl -X POST http://localhost:8080/v1/jobs \
  -H 'Authorization: Bearer developer-token' \
  -H 'Content-Type: application/json' \
  -d '{"tool":"ping","target":"1.1.1.1"}'
```

`ping`, `traceroute`, and `mtr` accept `args.protocol` as `icmp` or `tcp`; `count` and `max_hops` are owned by Server runtime config and are not user-controlled. `dns` replaces the old `nslookup` tool name. `port` performs a native TCP connect probe and requires `args.port`. By default Server resolves target hostnames before queuing jobs; set `resolve_on_agent: true` to defer DNS resolution and resolved-IP policy checks to the Agent.

Create a scheduled detection task:

```sh
curl -X POST http://localhost:8080/v1/schedules \
  -H 'Authorization: Bearer developer-token' \
  -H 'Content-Type: application/json' \
  -d '{"name":"cf-ping","tool":"ping","target":"1.1.1.1","interval_seconds":60}'
```

Query scheduled task history:

```sh
curl -H 'Authorization: Bearer developer-token' \
  http://localhost:8080/v1/schedules/<schedule-id>/history
```

Stream structured job events for browser UI:

```js
const events = new EventSource('/v1/jobs/<job-id>/stream?access_token=developer-token')
events.addEventListener('hop', (event) => console.log(JSON.parse(event.data)))
events.addEventListener('metric', (event) => console.log(JSON.parse(event.data)))
events.addEventListener('progress', (event) => console.log(JSON.parse(event.data)))
events.addEventListener('parsed', (event) => console.log(JSON.parse(event.data)))
```

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
  "apiToken": "frontend-token"
}
```

An empty `apiBaseUrl` works with the Vite development proxy. Start the Go server
and the frontend in separate terminals:

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

The frontend image serves the Vite build with Caddy on port `80`. At runtime it
loads `/config.json`, so deployments can replace that single file without
rebuilding the image. Use an empty `apiBaseUrl` only when the same public origin
also reverse-proxies `/v1` to Server. Otherwise set it to the browser-reachable
Server API origin:

```json
{
  "apiBaseUrl": "https://mtr-api.example.com",
  "apiToken": "frontend-token"
}
```

The Kubernetes examples include `deploy/web.yaml`, which deploys
`ghcr.io/ztelliot/mtr-web:latest`, exposes it as the `mtr-web` Service, and
mounts `mtr-web-config` over `/usr/share/caddy/config.json`. For a local
cluster smoke test:

```sh
kubectl apply -k deploy
kubectl -n mtr port-forward svc/mtr-server 8080:8080
kubectl -n mtr port-forward svc/mtr-web 8081:80
```

If you access the workbench at `http://localhost:8081`, set
`apiBaseUrl` in `mtr-web-config` to `http://localhost:8080`, or put an Ingress
or gateway in front of both services and route `/v1` to `mtr-server` and `/` to
`mtr-web`.

The `deploy/agent.yaml` DaemonSet can derive per-node Agent metadata from
Kubernetes Node labels without teaching the Agent binary about Kubernetes. An
init container reads the current Node labels, renders a normal `agent.yaml` into
an `emptyDir`, and the Agent container starts with that generated config. The
ServiceAccount only needs read-only `get nodes` permission for the init
container. The example maps these labels by default:

```sh
kubectl label node <node> \
  mtr.ztelliot.dev/country=JP \
  mtr.ztelliot.dev/region=tokyo \
  mtr.ztelliot.dev/provider=kubernetes \
  mtr.ztelliot.dev/isp=example-net \
  mtr.ztelliot.dev/protocols=3 \
  mtr.ztelliot.dev/hide-first-hops=0 \
  mtr.ztelliot.dev/capabilities=ping,traceroute,mtr,http,dns,port
```

Change `render-agent-config.sh` in `mtr-agent-config` if your cluster already
uses different label names. `protocols` uses the same bitmask as Agent config:
`1` for IPv4, `2` for IPv6, and `3` for both. `capabilities` is a
comma-separated tool list. If a label is missing, the init container writes the
fallback value into the generated config.

The workbench can create `ping`, `traceroute`, `mtr`, `http`, and `dns` jobs,
list Agents, and stream structured job events from `/v1/jobs/<job-id>/stream`.
`traceroute` and `mtr` require an explicit Agent selection because the server
requires `agent_id` for those tools.

## Agent/Server mTLS

`register_token` covers Agent registration authorization, while
`tls.ca_file`, `tls.cert_file`, and `tls.key_file` protect the gRPC control
plane between Agent and Server. To enable mutual TLS:

- Server must set `tls.ca_file`, `tls.cert_file`, and `tls.key_file` together.
  If only the server certificate/key are configured, the channel is TLS but
  Agents are not required to present a client certificate.
- Agent must also set `tls.ca_file`, `tls.cert_file`, and `tls.key_file`
  together. If only `ca_file` is set, the Agent verifies the Server but does
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
  ca_file: "/var/run/mtr/tls/ca.crt"
  cert_file: "/var/run/mtr/tls/tls.crt"
  key_file: "/var/run/mtr/tls/tls.key"
```

## Security Notes

Production deployments should place Anubis or another browser-facing gateway in front of the REST API for PoW/challenge handling, set mTLS certificate paths in both configs, rotate API and Agent tokens, and tune per-tool policies before exposing the service.

Agents execute diagnostics with native Go implementations and only upload structured result events.

Targets resolving to loopback, private, link-local, multicast, carrier-grade NAT, documentation, benchmarking, or other special-use address ranges are rejected by both Server and Agent.

The Agent image only needs the compiled Agent binary and CA certificates; diagnostic tools are implemented in Go.

## Docker Hardening

For container deployments, run as a non-root user, use a read-only root
filesystem, disable privilege escalation, and explicitly reduce Linux
capabilities. Server does not need extra capabilities. Agent needs `NET_RAW`
for ICMP-backed `ping`, `traceroute`, and `mtr`.

The examples use `65532:65532` as an unprivileged container UID/GID. Mounted
config and certificate files must be readable by that UID/GID. For private
keys, prefer mode `0640` with group `65532`, or use Docker/Compose secrets.

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

If Server uses SQLite, add a writable state mount and point `database_url` at a
file under that directory:

```sh
  -v /var/lib/mtr:/var/lib/mtr \
  --workdir /var/lib/mtr
```

The Agent in gRPC mode initiates the long-lived connection to Server and does
not need an exposed port. Only add `-p 9000:9000` when running `mode: http` or
`mode: grpc,http` and intentionally exposing `/invoke`, ideally behind a
controlled gateway. Avoid `--network host` unless you explicitly need the host
network perspective.

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

The repository now includes a baseline manifest set under `deploy/`:

- `server.yaml`: single-replica Server `Deployment` plus `Service`
- `agent.yaml`: Agent `DaemonSet`, with each pod using its own pod name as `MTR_ID`
- `networkpolicy.yaml`: denies inbound traffic to Agent pods
- `secrets.example.yaml`: Secret templates with placeholders you should replace
- `kustomization.yaml`: ready for `kubectl apply -k deploy`

These manifests assume:

- PostgreSQL is provided externally and injected through the `database-url` secret value.
- The gRPC control plane uses `mtr-server.mtr.svc.cluster.local:8443`, so the example Server certificate SANs match that service DNS name.
- All Agents share one client certificate, while each pod keeps a unique logical identity through `MTR_ID=metadata.name`.
- Server stays at `replicas: 1`. The scheduler hub holds in-process connection state, so this baseline does not claim horizontal control-plane scaling.

You can either edit the placeholders in `deploy/secrets.example.yaml` or
create the Secrets directly:

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

Then apply the baseline resources:

```sh
kubectl apply -k deploy
```

The Agent manifest is intentionally tight on privileges:

- `automountServiceAccountToken: false`
- non-root runtime, no host network/PID/IPC, `privileged: false`, and
  `allowPrivilegeEscalation: false`
- read-only root filesystem, with only read-only config/cert mounts and one
  `emptyDir` mounted at `/tmp`
- all Linux capabilities dropped except `NET_RAW`
- a NetworkPolicy denies inbound traffic to Agent pods

`NET_RAW` is the one deliberate exception because the current ICMP-backed
implementations of `ping`, `traceroute`, and `mtr` need raw sockets. No extra
RBAC is shipped with the Agent. If your cluster enforces strict Pod Security
Admission, plan for an explicit exception for `NET_RAW` in the Agent namespace.

The baseline does not include a default-deny egress `NetworkPolicy`. The Agent
is supposed to probe arbitrary external targets, and standard Kubernetes
`NetworkPolicy` support for ICMP is limited; if your CNI offers richer ICMP or
egress controls, it is worth tightening that layer further.

## Acknowledgements

Thanks to OpenAI Codex for collaborative support during development, refactoring, testing, and documentation.
