import { isFanoutTool } from "./pingRows";
import type { Agent, Job, JobEvent } from "./types";

const terminalJobStatuses = new Set(["succeeded", "failed", "canceled"]);
const terminalEventValues = new Set(["completed", "complete", "done", "succeeded", "failed", "canceled", "cancelled"]);

export function jobHasTerminalEvent(job: Job, events: JobEvent[], agents: Agent[]): boolean {
  if (terminalJobStatuses.has(job.status)) {
    return true;
  }
  if (isFanoutTool(job.tool) && !job.agent_id) {
    return events.some((event) => !event.agent_id && isTerminalEvent(event));
  }
  return events.some(isTerminalEvent);
}

export function isTerminalEvent(event: JobEvent): boolean {
  return isProgressEvent(event) && [event.event?.message, event.event?.status].some(isTerminalText);
}

function isProgressEvent(event: JobEvent): boolean {
  return event.event?.type === "progress" || event.stream === "progress";
}

function isTerminalText(value: unknown): boolean {
  return typeof value === "string" && terminalEventValues.has(value.trim().toLowerCase());
}
