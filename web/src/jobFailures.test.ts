import { describe, expect, it } from "vitest";
import { jobEventFailureType, shouldSuppressFanoutNodeFailure } from "./jobFailures";
import type { Job, JobEvent, Tool } from "./types";

const createdAt = "2026-04-25T00:00:00Z";

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

  it("suppresses node-level failures in a fanout task", () => {
    const currentJob = job("http");

    expect(shouldSuppressFanoutNodeFailure(currentJob, failureEvent("event-1", "edge-1"))).toBe(true);
  });

  it("does not suppress parent-level fanout failures", () => {
    const currentJob = job("dns");

    expect(shouldSuppressFanoutNodeFailure(currentJob, { ...failureEvent("event-1", ""), agent_id: undefined })).toBe(false);
  });

  it("does not suppress pinned single-node failures", () => {
    const currentJob = job("port", "edge-1");

    expect(shouldSuppressFanoutNodeFailure(currentJob, failureEvent("event-1", "edge-1"))).toBe(false);
  });
});
