import type { JobEvent, StreamEvent } from "./types";

const jobErrorTypeSet = new Set([
  "target_blocked",
  "unsupported_tool",
  "unsupported_protocol",
  "agent_disconnected",
  "job_timeout",
  "tool_failed",
  "fanout_failed",
  "job_failed"
]);

type StreamPayload = StreamEvent & { agent_id?: unknown };

export function mergeEvent(events: JobEvent[], event: JobEvent): JobEvent[] {
  if (events.some((existing) => existing.id === event.id)) {
    return events;
  }
  return [...events, event].sort(
    (a, b) => new Date(a.created_at).getTime() - new Date(b.created_at).getTime()
  );
}

export function jobEventFromStreamMessage(message: MessageEvent<string>, jobID: string, fallbackAgentID?: string): JobEvent {
  const payload = JSON.parse(message.data) as StreamPayload;
  const { agent_id: rawAgentID, ...eventPayload } = payload;
  const stream = typeof eventPayload.type === "string" && eventPayload.type ? eventPayload.type : message.type || "message";
  const agentID = typeof rawAgentID === "string" && rawAgentID ? rawAgentID : fallbackAgentID;
  if (!eventPayload.type) {
    eventPayload.type = stream;
  }
  const event: JobEvent = {
    id: message.lastEventId || `${jobID}:${stream}:${Date.now()}:${Math.random()}`,
    job_id: jobID,
    stream,
    event: eventPayload,
    created_at: new Date().toISOString()
  };
  if (agentID) {
    event.agent_id = agentID;
  }
  return event;
}

export function targetResolvedIP(events: JobEvent[]): string | undefined {
  for (const event of events) {
    if (event.event?.type !== "target_resolved") {
      continue;
    }
    const targetIP = event.event.metric?.target_ip;
    if (typeof targetIP === "string" && targetIP.trim()) {
      return targetIP;
    }
  }
  return undefined;
}

export function knownJobErrorType(value: unknown): string | null {
  return typeof value === "string" && jobErrorTypeSet.has(value) ? value : null;
}
