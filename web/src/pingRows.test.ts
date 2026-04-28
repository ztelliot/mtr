import { describe, expect, it } from "vitest";
import { buildMtrRows, buildNodeRows, buildPingRows, pingCapableAgents } from "./pingRows";
import type { Agent, Job, JobEvent } from "./types";

const agents: Agent[] = [
  {
    id: "edge-1",
    name: "Tokyo",
    country: "JP",
    region: "JP",
    provider: "Misaka",
    isp: "ExampleNet",
    capabilities: ["ping"],
    protocols: 3,
    status: "online",
    last_seen_at: "2026-04-25T00:00:00Z",
    created_at: "2026-04-25T00:00:00Z"
  },
  {
    id: "edge-2",
    name: "Offline",
    country: "US",
    region: "US",
    capabilities: ["ping"],
    protocols: 3,
    status: "offline",
    last_seen_at: "2026-04-25T00:00:00Z",
    created_at: "2026-04-25T00:00:00Z"
  }
];

const job: Job = {
  id: "job-1",
  tool: "ping",
  target: "1.1.1.1",
  agent_id: "edge-1",
  status: "running",
  created_at: "2026-04-25T00:00:00Z",
  updated_at: "2026-04-25T00:00:00Z"
};

describe("ping rows", () => {
  it("keeps only online ping-capable agents", () => {
    expect(pingCapableAgents(null)).toEqual([]);
    expect(pingCapableAgents(agents).map((agent) => agent.id)).toEqual(["edge-1"]);
  });

  it("builds node result rows from unified ping summary events", () => {
    const events: JobEvent[] = [
      {
        id: "event-1",
        job_id: "job-1",
        agent_id: "edge-1",
        stream: "summary",
        created_at: "2026-04-25T00:00:00Z",
        event: {
          type: "summary",
          exit_code: 0,
          metric: {
            packets_transmitted: 10,
            packets_received: 9,
            packet_loss_pct: 10,
            rtt_min_ms: 4.2,
            rtt_avg_ms: 4.5,
            rtt_max_ms: 5.1,
            rtt_mdev_ms: 0.3
          }
        }
      }
    ];

    expect(buildPingRows(agents, [job], { "job-1": events })).toEqual([
      {
        agentId: "edge-1",
        country: "JP",
        region: "JP",
        provider: "Misaka",
        isp: "ExampleNet",
        target: "1.1.1.1",
        ip: "-",
        sent: 10,
        bestMS: 4.2,
        avgMS: 4.5,
        worstMS: 5.1,
        stdevMS: 0.3,
        lossPct: 10,
        protocols: undefined,
        rttSamples: [],
        status: "succeeded",
        lastMS: undefined
      }
    ]);
  });

  it("uses live ping metrics before parsed summary arrives", () => {
    const events: JobEvent[] = [
      {
        id: "event-1",
        job_id: "job-1",
        agent_id: "edge-1",
        stream: "target_resolved",
        created_at: "2026-04-25T00:00:00Z",
        event: {
          type: "target_resolved",
          metric: { target_ip: "1.1.1.1", ip_version: 4 }
        }
      },
      {
        id: "event-2",
        job_id: "job-1",
        agent_id: "edge-1",
        stream: "metric",
        created_at: "2026-04-25T00:00:01Z",
        event: {
          type: "metric",
          tool: "ping",
          target: "1.1.1.1",
          metric: { latency_ms: 8.5, seq: 1 }
        }
      }
    ];

    expect(buildPingRows(agents, [job], { "job-1": events })[0]).toMatchObject({
      ip: "1.1.1.1",
      lastMS: 8.5,
      rttSamples: [8.5],
      sent: 1,
      lossPct: 0,
      status: "running"
    });
  });

  it("counts timeout ping metrics as sent packets before parsed summary arrives", () => {
    const events: JobEvent[] = [
      {
        id: "event-1",
        job_id: "job-1",
        agent_id: "edge-1",
        stream: "target_resolved",
        created_at: "2026-04-25T00:00:00Z",
        event: {
          type: "target_resolved",
          metric: { target_ip: "1.1.1.1", ip_version: 4 }
        }
      },
      {
        id: "event-2",
        job_id: "job-1",
        agent_id: "edge-1",
        stream: "metric",
        created_at: "2026-04-25T00:00:01Z",
        event: {
          type: "metric",
          metric: { seq: 1, timeout: true }
        }
      }
    ];

    expect(buildPingRows(agents, [job], { "job-1": events })[0]).toMatchObject({
      ip: "1.1.1.1",
      rttSamples: [null],
      sent: 1,
      lossPct: 100,
      status: "running"
    });
  });

  it("uses ping summary keys from parsed results", () => {
    const events: JobEvent[] = [
      {
        id: "event-1",
        job_id: "job-1",
        agent_id: "edge-1",
        stream: "target_resolved",
        created_at: "2026-04-25T00:00:00Z",
        event: {
          type: "target_resolved",
          metric: { target_ip: "1.1.1.1", ip_version: 4 }
        }
      },
      {
        id: "event-2",
        job_id: "job-1",
        agent_id: "edge-1",
        stream: "metric",
        created_at: "2026-04-25T00:00:01Z",
        event: {
          type: "metric",
          metric: { latency_ms: 7.2, seq: 1 }
        }
      },
      {
        id: "event-3",
        job_id: "job-1",
        agent_id: "edge-1",
        stream: "summary",
        created_at: "2026-04-25T00:00:02Z",
        event: {
          type: "summary",
          exit_code: 0,
          metric: {
            packets_transmitted: 10,
            packets_received: 10,
            rtt_avg_ms: 6.3,
            packet_loss_pct: 0
          }
        }
      }
    ];

    expect(buildPingRows(agents, [job], { "job-1": events })[0]).toMatchObject({
      lastMS: 7.2,
      avgMS: 6.3,
      sent: 10,
      lossPct: 0,
      status: "succeeded"
    });
  });

  it("fills all-timeout ping summary chart samples from sent count", () => {
    const events: JobEvent[] = [
      {
        id: "event-1",
        job_id: "job-1",
        agent_id: "edge-1",
        stream: "summary",
        created_at: "2026-04-25T00:00:00Z",
        event: {
          type: "summary",
          exit_code: 1,
          metric: {
            packets_transmitted: 4,
            packets_received: 0,
            packet_loss_pct: 100
          }
        }
      }
    ];

    expect(buildPingRows(agents, [job], { "job-1": events })[0]).toMatchObject({
      sent: 4,
      lossPct: 100,
      rttSamples: [null, null, null, null]
    });
  });

  it("groups unpinned multi-node job events by agent", () => {
    const aggregateJob: Job = {
      ...job,
      id: "aggregate-job",
      agent_id: undefined
    };
    const aggregateAgents: Agent[] = [
      agents[0],
      { ...agents[0], id: "edge-3", country: "US", region: "US", provider: "Zeta", isp: "OtherNet" }
    ];
    const events: JobEvent[] = [
      {
        id: "event-1",
        job_id: "aggregate-job",
        agent_id: "edge-3",
        stream: "target_resolved",
        created_at: "2026-04-25T00:00:00Z",
        event: {
          type: "target_resolved",
          metric: { target_ip: "1.1.1.1", ip_version: 4 }
        }
      },
      {
        id: "event-2",
        job_id: "aggregate-job",
        agent_id: "edge-3",
        stream: "metric",
        created_at: "2026-04-25T00:00:01Z",
        event: {
          type: "metric",
          metric: { latency_ms: 20, seq: 1 }
        }
      },
      {
        id: "event-3",
        job_id: "aggregate-job",
        agent_id: "edge-1",
        stream: "target_resolved",
        created_at: "2026-04-25T00:00:02Z",
        event: {
          type: "target_resolved",
          metric: { target_ip: "1.1.1.1", ip_version: 4 }
        }
      },
      {
        id: "event-4",
        job_id: "aggregate-job",
        agent_id: "edge-1",
        stream: "summary",
        created_at: "2026-04-25T00:00:03Z",
        event: {
          type: "summary",
          exit_code: 0,
          metric: {
            packets_transmitted: 3,
            packets_received: 3,
            rtt_avg_ms: 4.5
          }
        }
      }
    ];

    expect(buildNodeRows("ping", aggregateAgents, [aggregateJob], { "aggregate-job": events })).toMatchObject([
      {
        agentId: "edge-1",
        provider: "Misaka",
        ip: "1.1.1.1",
        avgMS: 4.5,
        status: "succeeded"
      },
      {
        agentId: "edge-3",
        provider: "Zeta",
        ip: "1.1.1.1",
        lastMS: 20,
        status: "running"
      }
    ]);
  });

  it("does not pre-render capable nodes before an unpinned multi-node job has events", () => {
    const aggregateJob: Job = {
      ...job,
      id: "aggregate-job",
      agent_id: undefined
    };
    const aggregateAgents: Agent[] = [
      agents[0],
      { ...agents[0], id: "edge-3", country: "US", region: "US", provider: "Zeta" }
    ];

    expect(buildNodeRows("ping", aggregateAgents, [aggregateJob], {})).toEqual([]);
  });

  it("adds unknown unpinned multi-node agents dynamically from backend events", () => {
    const aggregateJob: Job = {
      ...job,
      id: "aggregate-job",
      agent_id: undefined
    };
    const events: JobEvent[] = [
      {
        id: "event-1",
        job_id: "aggregate-job",
        agent_id: "edge-new",
        stream: "target_resolved",
        created_at: "2026-04-25T00:00:00Z",
        event: {
          type: "target_resolved",
          metric: { target_ip: "1.1.1.1", ip_version: 4 }
        }
      },
      {
        id: "event-2",
        job_id: "aggregate-job",
        agent_id: "edge-new",
        stream: "metric",
        created_at: "2026-04-25T00:00:01Z",
        event: {
          type: "metric",
          metric: { latency_ms: 12, seq: 1 }
        }
      }
    ];

    expect(buildNodeRows("ping", agents, [aggregateJob], { "aggregate-job": events })[0]).toMatchObject({
      agentId: "edge-new",
      region: "-",
      provider: "edge-new",
      ip: "1.1.1.1",
      lastMS: 12
    });
  });

  it("does not add a row for target_blocked agents", () => {
    const aggregateJob: Job = {
      ...job,
      id: "aggregate-job",
      agent_id: undefined
    };
    const events: JobEvent[] = [
      {
        id: "event-1",
        job_id: "aggregate-job",
        agent_id: "edge-1",
        stream: "progress",
        created_at: "2026-04-25T00:00:00Z",
        event: {
          type: "progress",
          message: "target_blocked"
        }
      },
      {
        id: "event-2",
        job_id: "aggregate-job",
        agent_id: "edge-1",
        stream: "summary",
        created_at: "2026-04-25T00:00:01Z",
        event: {
          type: "summary",
          exit_code: -1,
          metric: { error: "target blocked" }
        }
      }
    ];

    expect(buildNodeRows("ping", agents, [aggregateJob], { "aggregate-job": events })).toEqual([]);
  });

  it("keeps MTR chart samples from hop history when hop_summary overrides stats", () => {
    const events: JobEvent[] = [
      {
        id: "event-1",
        job_id: "mtr-job",
        agent_id: "edge-1",
        stream: "hop",
        created_at: "2026-04-25T00:00:00Z",
        event: {
          type: "hop",
          hop: { index: 1, ip: "10.0.0.1", probes: [{ ip: "10.0.0.1", rtt_ms: 4 }] }
        }
      },
      {
        id: "event-2",
        job_id: "mtr-job",
        agent_id: "edge-1",
        stream: "hop",
        created_at: "2026-04-25T00:00:01Z",
        event: {
          type: "hop",
          hop: { index: 1, ip: "10.0.0.1", probes: [{ timeout: true }, { ip: "10.0.0.1", rtt_ms: 6 }] }
        }
      },
      {
        id: "event-3",
        job_id: "mtr-job",
        agent_id: "edge-1",
        stream: "hop_summary",
        created_at: "2026-04-25T00:00:02Z",
        event: {
          type: "hop_summary",
          hop: {
            index: 1,
            ip: "10.0.0.1",
            sent: 10,
            loss_pct: 70,
            avg_ms: 20,
            best_ms: 18,
            worst_ms: 22,
            last_ms: 21,
            stdev_ms: 1.5,
            rtt_ms: 21
          }
        }
      }
    ];

    expect(buildMtrRows(agents, "edge-1", events)[0]).toMatchObject({
      avgMS: 20,
      sent: 10,
      lossPct: 70,
      rttSamples: [4, null, 6]
    });
  });

  it("fills all-timeout MTR summary chart samples from sent count", () => {
    const events: JobEvent[] = [
      {
        id: "event-1",
        job_id: "mtr-job",
        agent_id: "edge-1",
        stream: "hop_summary",
        created_at: "2026-04-25T00:00:00Z",
        event: {
          type: "hop_summary",
          hop: {
            index: 11,
            ip: "*",
            sent: 10,
            loss_pct: 100
          }
        }
      }
    ];

    expect(buildMtrRows(agents, "edge-1", events)[0]).toMatchObject({
      hop: 11,
      sent: 10,
      lossPct: 100,
      rttSamples: Array.from({ length: 10 }, () => null)
    });
  });

  it("formats port node rows with the peer IP", () => {
    const portJob: Job = {
      ...job,
      id: "port-job",
      tool: "port",
      target: "example.com",
      args: { port: "443" }
    };
    const events: JobEvent[] = [
      {
        id: "event-1",
        job_id: "port-job",
        agent_id: "edge-1",
        stream: "summary",
        created_at: "2026-04-25T00:00:00Z",
        event: {
          type: "summary",
          exit_code: 0,
          metric: {
            port: 443,
            peer: "93.184.216.34",
            status: "open",
            connect_ms: 5
          }
        }
      }
    ];

    expect(buildNodeRows("port", agents, [portJob], { "port-job": events })[0]).toMatchObject({
      ip: "93.184.216.34",
      connectMS: 5,
      status: "open"
    });
  });

  it("uses HTTP status code as status and shows only the remote IP", () => {
    const httpJob: Job = {
      ...job,
      id: "http-job",
      tool: "http",
      target: "https://example.com"
    };
    const events: JobEvent[] = [
      {
        id: "event-1",
        job_id: "http-job",
        agent_id: "edge-1",
        stream: "summary",
        created_at: "2026-04-25T00:00:00Z",
        event: {
          type: "summary",
          exit_code: 0,
          metric: {
            http_code: 204,
            remote_addr: "93.184.216.34:443",
            time_total_ms: 20
          }
        }
      }
    ];

    expect(buildNodeRows("http", agents, [httpJob], { "http-job": events })[0]).toMatchObject({
      ip: "93.184.216.34",
      httpCode: 204,
      status: "204",
      totalMS: 20
    });
  });

  it("sorts multi-node rows by country, region, provider, and isp", () => {
    const sortingAgents: Agent[] = [
      { ...agents[0], id: "agent-3", country: "US", region: "ca", provider: "Zeta", isp: "Beta" },
      { ...agents[0], id: "agent-1", country: "CN", region: "hk", provider: "Alpha", isp: "Zed" },
      { ...agents[0], id: "agent-2", country: "CN", region: "hk", provider: "Alpha", isp: "Beta" },
      { ...agents[0], id: "agent-4", country: "CN", region: "bj", provider: "Omega", isp: "CNC" }
    ];
    const sortingJobs: Job[] = sortingAgents.map((agent) => ({
      ...job,
      id: `job-${agent.id}`,
      agent_id: agent.id
    }));

    expect(buildNodeRows("dns", sortingAgents, sortingJobs, {}).map((row) => row.agentId)).toEqual([
      "agent-4",
      "agent-2",
      "agent-1",
      "agent-3"
    ]);
  });
});
