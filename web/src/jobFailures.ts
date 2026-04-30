import { capableAgents, isFanoutTool } from "./pingRows";
import { knownJobErrorType } from "./streamEvents";
import type { Agent, Job, JobEvent } from "./types";

export type FanoutFailureState = "none" | "partial" | "all";

export function jobEventFailureType(event: JobEvent): string | null {
  if (isTargetBlockedEvent(event)) {
    return "target_blocked";
  }
  const eventType = typeof event.event?.type === "string" ? event.event.type : event.stream;
  const eventMessage = typeof event.event?.message === "string" ? event.event.message : "";
  return knownJobErrorType(eventMessage) || knownJobErrorType(eventType);
}

export function fanoutFailureState(job: Job, events: JobEvent[], agents: Agent[]): FanoutFailureState {
  if (!isFanoutJob(job)) {
    return "none";
  }
  const failedAgentIDs = new Set(
    events
      .filter((event) => Boolean(event.agent_id) && (Boolean(jobEventFailureType(event)) || isFailedResultEvent(event)))
      .map((event) => event.agent_id!)
  );
  if (failedAgentIDs.size === 0) {
    return "none";
  }

  const expectedAgentIDs = capableAgents(agents, job.tool).map((agent) => agent.id);
  if (expectedAgentIDs.length === 0) {
    return failedAgentIDs.size > 1 ? "all" : "partial";
  }
  if (expectedAgentIDs.every((id) => failedAgentIDs.has(id))) {
    return "all";
  }
  return "partial";
}

export function shouldSuppressFanoutFailure(job: Job, events: JobEvent[], agents: Agent[]): boolean {
  return fanoutFailureState(job, events, agents) === "partial";
}

function isFanoutJob(job: Job): boolean {
  return isFanoutTool(job.tool) && !job.agent_id;
}

function isTargetBlockedEvent(event: JobEvent): boolean {
  return event.event?.type === "target_blocked" || event.event?.message === "target_blocked" || event.stream === "target_blocked";
}

function isFailedResultEvent(event: JobEvent): boolean {
  return [event.exit_code, event.event?.exit_code, event.parsed?.exit_code].some(
    (exitCode) => typeof exitCode === "number" && exitCode !== 0
  );
}
