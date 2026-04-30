import { isFanoutTool } from "./pingRows";
import { knownJobErrorType } from "./streamEvents";
import type { Job, JobEvent } from "./types";

export function jobEventFailureType(event: JobEvent): string | null {
  if (isTargetBlockedEvent(event)) {
    return "target_blocked";
  }
  const eventType = typeof event.event?.type === "string" ? event.event.type : event.stream;
  const eventMessage = typeof event.event?.message === "string" ? event.event.message : "";
  return knownJobErrorType(eventMessage) || knownJobErrorType(eventType);
}

export function shouldSuppressFanoutNodeFailure(job: Job, event: JobEvent): boolean {
  return isFanoutJob(job) && Boolean(event.agent_id) && Boolean(jobEventFailureType(event));
}

function isFanoutJob(job: Job): boolean {
  return isFanoutTool(job.tool) && !job.agent_id;
}

function isTargetBlockedEvent(event: JobEvent): boolean {
  return event.event?.type === "target_blocked" || event.event?.message === "target_blocked" || event.stream === "target_blocked";
}
