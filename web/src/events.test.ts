import { describe, expect, it } from "vitest";
import {
  eventDetail,
  extractHops,
  latestParsed,
  normalizeDisplayEvent,
  resultRows
} from "./events";
import type { JobEvent } from "./types";

const baseEvent: JobEvent = {
  id: "event-1",
  job_id: "job-1",
  stream: "progress",
  created_at: "2026-04-25T00:00:00Z"
};

describe("stream events", () => {
  it("normalizes display events", () => {
    expect(
      normalizeDisplayEvent({
        ...baseEvent,
        event: { type: "progress", message: "started" }
      })
    ).toMatchObject({
      id: "event-1",
      stream: "progress",
      title: "progress",
      detail: "started"
    });
  });

  it("extracts latest hop per index from live and parsed events", () => {
    const events: JobEvent[] = [
      {
        ...baseEvent,
        id: "live-hop",
        stream: "hop",
        event: { type: "hop", hop: { index: 2, ip: "2.2.2.2", avg_ms: 20 } }
      },
      {
        ...baseEvent,
        id: "parsed",
        stream: "parsed",
        parsed: {
          tool: "mtr",
          target: "example.com",
          exit_code: 0,
          hops: [
            { index: 1, ip: "1.1.1.1", avg_ms: 10 },
            { index: 2, ip: "2.2.2.3", avg_ms: 18 }
          ]
        }
      }
    ];

    expect(extractHops(events)).toEqual([
      { index: 1, ip: "1.1.1.1", avg_ms: 10 },
      { index: 2, ip: "2.2.2.3", avg_ms: 18 }
    ]);
    expect(latestParsed(events)?.exit_code).toBe(0);
  });

  it("does not let final summary results override route data", () => {
    const events: JobEvent[] = [
      {
        ...baseEvent,
        id: "live-hop",
        stream: "hop",
        event: { type: "hop", hop: { index: 1, probes: [{ ip: "59.43.67.5", rtt_ms: 7 }] } }
      },
      {
        ...baseEvent,
        id: "summary",
        stream: "parsed",
        parsed: {
          type: "summary",
          tool: "traceroute",
          target: "example.com",
          exit_code: 0,
          summary: { hop_count: 1 }
        }
      }
    ];

    expect(latestParsed(events)).toBeUndefined();
    expect(resultRows(latestParsed(events), extractHops(events))).toEqual([
      {
        label: "#1",
        value: "59.43.67.5",
        meta: "7.0 ms avg · 1 probes",
        status: "0.0% loss"
      }
    ]);
  });

  it("summarizes metrics as compact JSON", () => {
    expect(
      eventDetail({
        ...baseEvent,
        event: { type: "metric", metric: { latency_ms: 12.3 } }
      })
    ).toBe('{"latency_ms":12.3}');
  });

  it("normalizes unified summary events from the current server envelope", () => {
    const event: JobEvent = {
      ...baseEvent,
      stream: undefined,
      event: {
        type: "summary",
        exit_code: 0,
        metric: { record_count: 2 },
        records: [
          { type: "A", value: "93.184.216.34" },
          { type: "A", value: "93.184.216.35" }
        ]
      }
    };

    expect(normalizeDisplayEvent(event)).toMatchObject({
      stream: "summary",
      title: "summary"
    });
    expect(resultRows(latestParsed([event], { tool: "dns", target: "example.com" }))).toEqual([]);
  });

  it("extracts hop_summary events from the current server envelope", () => {
    const events: JobEvent[] = [
      {
        ...baseEvent,
        stream: undefined,
        event: {
          type: "hop_summary",
          hop: {
            index: 2,
            ip: "203.0.113.1",
            sent: 10,
            loss_pct: 20,
            avg_ms: 8,
            best_ms: 7,
            worst_ms: 9,
            last_ms: 8.5,
            stdev_ms: 0.5
          }
        }
      }
    ];

    expect(extractHops(events)).toEqual([
      {
        index: 2,
        ip: "203.0.113.1",
        sent: 10,
        loss_pct: 20,
        avg_ms: 8,
        best_ms: 7,
        worst_ms: 9,
        last_ms: 8.5,
        stdev_ms: 0.5
      }
    ]);
  });

  it("uses hop_summary as an aggregate replacement instead of another probe", () => {
    const events: JobEvent[] = [
      {
        ...baseEvent,
        id: "live-hop",
        stream: "hop",
        event: {
          type: "hop",
          hop: { index: 7, ip: "59.43.39.118", probes: [{ ip: "59.43.39.118", rtt_ms: 18.2 }] }
        }
      },
      {
        ...baseEvent,
        id: "hop-summary",
        stream: "hop_summary",
        created_at: "2026-04-26T15:07:57.778398527Z",
        event: {
          type: "hop_summary",
          hop: {
            index: 7,
            ip: "59.43.39.118",
            rtt_ms: 17.766,
            loss_pct: 40,
            sent: 10,
            avg_ms: 17.612333333333332,
            best_ms: 17.372,
            worst_ms: 17.768,
            last_ms: 17.514,
            stdev_ms: 0.16
          }
        }
      }
    ];

    expect(extractHops(events)).toEqual([
      {
        index: 7,
        ip: "59.43.39.118",
        loss_pct: 40,
        sent: 10,
        avg_ms: 17.612333333333332,
        best_ms: 17.372,
        worst_ms: 17.768,
        last_ms: 17.514,
        stdev_ms: 0.16
      }
    ]);
  });

  it("uses final MTR summary hops as the authoritative indexed list", () => {
    const events: JobEvent[] = [
      {
        ...baseEvent,
        id: "stale-live-hop",
        stream: "hop",
        event: {
          type: "hop",
          hop: { index: 3, ip: "192.0.2.3", rtt_ms: 30 }
        }
      },
      {
        ...baseEvent,
        id: "summary",
        stream: "summary",
        created_at: "2026-04-25T00:00:01Z",
        event: {
          type: "summary",
          exit_code: 0,
          metric: { hop_count: 2 },
          hops: [
            { index: 2, ip: "192.0.2.2", sent: 10, loss_pct: 0, avg_ms: 20 },
            { index: 1, ip: "192.0.2.1", sent: 10, loss_pct: 0, avg_ms: 10 }
          ]
        }
      }
    ];

    expect(extractHops(events)).toEqual([
      { index: 1, ip: "192.0.2.1", sent: 10, loss_pct: 0, avg_ms: 10 },
      { index: 2, ip: "192.0.2.2", sent: 10, loss_pct: 0, avg_ms: 20 }
    ]);
  });

  it("drops live MTR hops past the server summary hop count", () => {
    const events: JobEvent[] = [
      {
        ...baseEvent,
        id: "live-hop-1",
        stream: "hop",
        event: {
          type: "hop",
          hop: { index: 1, ip: "192.0.2.1", rtt_ms: 10 }
        }
      },
      {
        ...baseEvent,
        id: "live-hop-3",
        stream: "hop",
        event: {
          type: "hop",
          hop: { index: 3, ip: "192.0.2.3", rtt_ms: 30 }
        }
      },
      {
        ...baseEvent,
        id: "summary",
        stream: "summary",
        created_at: "2026-04-25T00:00:01Z",
        event: {
          type: "summary",
          exit_code: 0,
          metric: { hop_count: 1 }
        }
      }
    ];

    expect(extractHops(events).map((hop) => hop.index)).toEqual([1]);
  });

  it("summarizes traceroute live hop probes for route display", () => {
    const events: JobEvent[] = [
      {
        ...baseEvent,
        id: "hop-probes",
        stream: "hop",
        event: {
          type: "hop",
          hop: {
            index: 4,
            probes: [
              { ip: "59.43.67.5", rtt_ms: 7.145 },
              { ip: "59.43.67.5", rtt_ms: 6.906 },
              { ip: "59.43.67.5", rtt_ms: 6.787 }
            ]
          }
        }
      }
    ];

    expect(extractHops(events)).toEqual([
      {
        index: 4,
        ip: "59.43.67.5",
        probes: [
          { ip: "59.43.67.5", rtt_ms: 7.145 },
          { ip: "59.43.67.5", rtt_ms: 6.906 },
          { ip: "59.43.67.5", rtt_ms: 6.787 }
        ],
        sent: 3,
        loss_pct: 0,
        avg_ms: expect.closeTo(6.946, 3),
        best_ms: 6.787,
        worst_ms: 7.145,
        last_ms: 6.787,
        stdev_ms: expect.closeTo(0.1489, 3)
      }
    ]);
    expect(resultRows(undefined, extractHops(events))[0]).toMatchObject({
      label: "#4",
      value: "59.43.67.5",
      status: "0.0% loss"
    });
  });

  it("summarizes flat live hops for route display", () => {
    const events: JobEvent[] = [
      {
        ...baseEvent,
        id: "hop-flat",
        stream: "hop",
        event: {
          type: "hop",
          hop: { index: 4, ip: "59.43.67.5", rtt_ms: 7.145 }
        }
      }
    ];

    expect(extractHops(events)).toEqual([
      {
        index: 4,
        ip: "59.43.67.5",
        rtt_ms: 7.145,
        probes: [{ ip: "59.43.67.5", rtt_ms: 7.145 }],
        sent: 1,
        loss_pct: 0,
        avg_ms: 7.145,
        best_ms: 7.145,
        worst_ms: 7.145,
        last_ms: 7.145,
        stdev_ms: 0
      }
    ]);
    expect(resultRows(undefined, extractHops(events))[0]).toMatchObject({
      label: "#4",
      value: "59.43.67.5",
      meta: "7.1 ms avg · 1 probes",
      status: "0.0% loss"
    });
  });

  it("summarizes timeout probes as loss", () => {
    const events: JobEvent[] = [
      {
        ...baseEvent,
        id: "hop-timeout",
        stream: "hop",
        event: {
          type: "hop",
          hop: {
            index: 2,
            probes: [{ timeout: true }, { ip: "10.0.0.1", rtt_ms: 3 }]
          }
        }
      }
    ];

    expect(extractHops(events)[0]).toMatchObject({
      ip: "10.0.0.1",
      sent: 2,
      loss_pct: 50,
      avg_ms: 3
    });
  });

  it("keeps historical MTR probes and recomputes aggregate metrics", () => {
    const events: JobEvent[] = [
      {
        ...baseEvent,
        id: "hop-1",
        stream: "hop",
        event: {
          type: "hop",
          hop: { index: 3, probes: [{ ip: "10.0.0.1", rtt_ms: 4 }] }
        }
      },
      {
        ...baseEvent,
        id: "hop-2",
        stream: "hop",
        created_at: "2026-04-25T00:00:01Z",
        event: {
          type: "hop",
          hop: { index: 3, probes: [{ ip: "10.0.0.1", rtt_ms: 8 }] }
        }
      },
      {
        ...baseEvent,
        id: "hop-3",
        stream: "hop",
        created_at: "2026-04-25T00:00:02Z",
        event: {
          type: "hop",
          hop: { index: 3, probes: [{ timeout: true }] }
        }
      }
    ];

    expect(extractHops(events)).toEqual([
      {
        index: 3,
        ip: "10.0.0.1",
        probes: [
          { ip: "10.0.0.1", rtt_ms: 4 },
          { ip: "10.0.0.1", rtt_ms: 8 },
          { timeout: true }
        ],
        sent: 3,
        loss_pct: expect.closeTo(33.333, 3),
        avg_ms: 6,
        best_ms: 4,
        worst_ms: 8,
        last_ms: 8,
        stdev_ms: 2
      }
    ]);
  });

  it("keeps historical flat MTR hops and recomputes aggregate metrics", () => {
    const events: JobEvent[] = [
      {
        ...baseEvent,
        id: "hop-1",
        stream: "hop",
        event: {
          type: "hop",
          hop: { index: 3, ip: "10.0.0.1", rtt_ms: 4 }
        }
      },
      {
        ...baseEvent,
        id: "hop-2",
        stream: "hop",
        created_at: "2026-04-25T00:00:01Z",
        event: {
          type: "hop",
          hop: { index: 3, ip: "10.0.0.1", rtt_ms: 8 }
        }
      },
      {
        ...baseEvent,
        id: "hop-3",
        stream: "hop",
        created_at: "2026-04-25T00:00:02Z",
        event: {
          type: "hop",
          hop: { index: 3, timeout: true }
        }
      }
    ];

    expect(extractHops(events)[0]).toMatchObject({
      index: 3,
      ip: "10.0.0.1",
      sent: 3,
      loss_pct: expect.closeTo(33.333, 3),
      avg_ms: 6,
      best_ms: 4,
      worst_ms: 8,
      last_ms: 8,
      stdev_ms: 2
    });
  });

  it("does not let timeout peer placeholders overwrite the previous hop IP", () => {
    const events: JobEvent[] = [
      {
        ...baseEvent,
        id: "answered",
        stream: "hop",
        event: {
          type: "hop",
          hop: { index: 5, ip: "192.0.2.1", probes: [{ ip: "192.0.2.1", rtt_ms: 12 }] }
        }
      },
      {
        ...baseEvent,
        id: "timeout",
        stream: "hop",
        created_at: "2026-04-25T00:00:01Z",
        event: {
          type: "hop",
          hop: { index: 5, ip: "*", probes: [{ ip: "*", timeout: true }] }
        }
      }
    ];

    expect(extractHops(events)[0]).toMatchObject({
      index: 5,
      ip: "192.0.2.1",
      sent: 2,
      loss_pct: 50,
      avg_ms: 12
    });
  });

  it("builds DNS rows from parsed records", () => {
    expect(
      resultRows({
        tool: "dns",
        target: "example.com",
        exit_code: 0,
        records: [{ type: "A", value: "93.184.216.34" }]
      })
    ).toEqual([{ label: "A", value: "93.184.216.34" }]);
  });

  it("builds port rows from parsed summary", () => {
    expect(
      resultRows({
        tool: "port",
        target: "example.com",
        exit_code: 0,
        summary: {
          port: 443,
          status: "open",
          connect_ms: 12.4
        }
      })
    ).toEqual([
      { label: "port", value: "443", status: undefined },
      { label: "status", value: "open", status: "open" },
      { label: "connect_ms", value: "12.4", status: undefined }
    ]);
  });
});
