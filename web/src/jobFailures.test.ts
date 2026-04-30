import { describe, expect, it } from "vitest";
import { fanoutFailureState, jobEventFailureType, shouldSuppressFanoutFailure } from "./jobFailures";
import type { Agent, Job, JobEvent, Tool } from "./types";

const createdAt = "2026-04-25T00:00:00Z";

function agent(id: string, tool: Tool): Agent {
  return {
    id,
    capabilities: [tool],
    protocols: 3,
    status: "online",
    last_seen_at: createdAt,
    created_at: createdAt
  };
}

function job(tool: Tool, agentID?: string): Job {
  return {
    id: "job-1",
    tool,
    target: tool === "http" ? "https://example.com" : "example.com",
    agent_id: agentID,
    status: "running",
    created_at: createdAt,
    updated_at: createdAt
  };
}

function failureEvent(id: string, agentID: string, message = "job_timeout"): JobEvent {
  return {
    id,
    job_id: "job-1",
    agent_id: agentID,
    stream: "message",
    event: { type: "message", message },
    created_at: createdAt
  };
}

function summaryEvent(id: string, agentID: string, exitCode: number): JobEvent {
  return {
    id,
    job_id: "job-1",
    agent_id: agentID,
    stream: "summary",
    event: { type: "summary", exit_code: exitCode },
    exit_code: exitCode,
    created_at: createdAt
  };
}

describe("job failure helpers", () => {
  it("detects known failure types from event messages and target-blocked streams", () => {
    expect(jobEventFailureType(failureEvent("timeout", "edge-1"))).toBe("job_timeout");
    expect(
      jobEventFailureType({
        ...failureEvent("blocked", "edge-1", ""),
        stream: "target_blocked",
        event: { type: "target_blocked" }
      })
    ).toBe("target_blocked");
  });

  it("suppresses a single failed node in a multi-node http task", () => {
    const currentJob = job("http");
    const agents = [agent("edge-1", "http"), agent("edge-2", "http")];
    const events = [failureEvent("event-1", "edge-1")];

    expect(fanoutFailureState(currentJob, events, agents)).toBe("partial");
    expect(shouldSuppressFanoutFailure(currentJob, events, agents)).toBe(true);
  });

  it("does not suppress when every fanout node failed", () => {
    const currentJob = job("dns");
    const agents = [agent("edge-1", "dns"), agent("edge-2", "dns")];
    const events = [failureEvent("event-1", "edge-1"), failureEvent("event-2", "edge-2")];

    expect(fanoutFailureState(currentJob, events, agents)).toBe("all");
    expect(shouldSuppressFanoutFailure(currentJob, events, agents)).toBe(false);
  });

  it("counts non-zero node summaries as fanout node failures", () => {
    const currentJob = job("port");
    const agents = [agent("edge-1", "port"), agent("edge-2", "port")];
    const events = [summaryEvent("event-1", "edge-1", 1), summaryEvent("event-2", "edge-2", 0)];

    expect(fanoutFailureState(currentJob, events, agents)).toBe("partial");
    expect(shouldSuppressFanoutFailure(currentJob, events, agents)).toBe(true);
  });

  it("does not suppress pinned single-node failures", () => {
    const currentJob = job("port", "edge-1");
    const agents = [agent("edge-1", "port"), agent("edge-2", "port")];

    expect(shouldSuppressFanoutFailure(currentJob, [failureEvent("event-1", "edge-1")], agents)).toBe(false);
  });
});
