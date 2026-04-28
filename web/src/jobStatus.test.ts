import { describe, expect, it } from "vitest";
import { isTerminalEvent, jobHasTerminalEvent } from "./jobStatus";
import type { Agent, Job, JobEvent } from "./types";

const agents: Agent[] = [
  {
    id: "edge-1",
    country: "CN",
    region: "edge",
    provider: "kubernetes",
    capabilities: ["ping"],
    protocols: 3,
    status: "online",
    last_seen_at: "2026-04-25T00:00:00Z",
    created_at: "2026-04-25T00:00:00Z"
  },
  {
    id: "edge-stale",
    country: "US",
    region: "stale",
    provider: "offline",
    capabilities: ["ping"],
    protocols: 3,
    status: "online",
    last_seen_at: "2026-04-25T00:00:00Z",
    created_at: "2026-04-25T00:00:00Z"
  }
];

const job: Job = {
  id: "job-1",
  tool: "ping",
  target: "1.1.1.1",
  status: "running",
  created_at: "2026-04-25T00:00:00Z",
  updated_at: "2026-04-25T00:00:00Z"
};

function event(message: string, agentID = "edge-1"): JobEvent {
  return {
    id: `event-${message}`,
    job_id: "job-1",
    agent_id: agentID,
    created_at: "2026-04-25T00:00:00Z",
    event: { type: "progress", message }
  };
}

describe("job status helpers", () => {
  it("treats progress completed as the job terminal event", () => {
    const completed = event("completed");
    const singleJob = { ...job, agent_id: "edge-1" };

    expect(isTerminalEvent(completed)).toBe(true);
    expect(jobHasTerminalEvent(singleJob, [completed], agents)).toBe(true);
  });

  it("lets refreshed terminal job status finish even when the local agent list is stale", () => {
    expect(jobHasTerminalEvent({ ...job, status: "succeeded" }, [event("completed")], agents)).toBe(true);
  });

  it("does not treat agent summary as a job terminal event", () => {
    const summary: JobEvent = {
      id: "summary-1",
      job_id: "job-1",
      agent_id: "edge-1",
      created_at: "2026-04-25T00:00:00Z",
      event: { type: "summary", exit_code: 0 }
    };

    expect(isTerminalEvent(summary)).toBe(false);
    expect(jobHasTerminalEvent(job, [summary], agents)).toBe(false);
  });

  it("treats failed progress as terminal", () => {
    const failed = event("failed");
    const singleJob = { ...job, agent_id: "edge-1" };

    expect(isTerminalEvent(failed)).toBe(true);
    expect(jobHasTerminalEvent(singleJob, [failed], agents)).toBe(true);
  });

  it("requires fanout terminal progress to be job-level", () => {
    expect(jobHasTerminalEvent(job, [event("completed")], agents)).toBe(false);
    expect(jobHasTerminalEvent(job, [event("completed", "")], agents)).toBe(true);
  });
});
