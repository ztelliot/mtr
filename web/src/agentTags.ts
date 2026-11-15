import type { Agent } from "./types";

type Translate = (key: string, options?: Record<string, unknown>) => string;

export interface AgentTagCount {
  tag: string;
  count: number;
}

export function isAgentPolicyLabel(label: string): boolean {
  const value = label.trim();
  return value === "agent" || value.startsWith("agent:");
}

export function isPolicyLabel(label: string): boolean {
  return isAgentPolicyLabel(label) || label.trim().startsWith("id:");
}

export function normalizeAgentTags(tags: string[]): string[] {
  const seen = new Set<string>();
  const normalized: string[] = [];
  for (const tag of tags) {
    const value = tag.trim();
    if (!value || seen.has(value)) {
      continue;
    }
    seen.add(value);
    normalized.push(value);
  }
  return normalized;
}

export function normalizeSelectableAgentTags(tags: string[]): string[] {
  return normalizeAgentTags(tags).filter(isSelectableAgentTag);
}

export function agentTagCounts(agents: Agent[]): AgentTagCount[] {
  const counts = new Map<string, number>();
  for (const agent of agents) {
    const agentTags = new Set(normalizeSelectableAgentTags(agent.labels ?? []));
    for (const tag of agentTags) {
      counts.set(tag, (counts.get(tag) ?? 0) + 1);
    }
  }
  return [...counts]
    .map(([tag, count]) => ({ tag, count }))
    .sort((left, right) => left.tag.localeCompare(right.tag));
}

export function filterAgentsByTags(agents: Agent[], tags: string[]): Agent[] {
  const selected = new Set(normalizeAgentTags(tags));
  if (selected.size === 0) {
    return agents;
  }
  return agents.filter((agent) => (agent.labels ?? []).some((label) => selected.has(label.trim())));
}

export function isSelectableAgentTag(tag: string): boolean {
  const normalized = tag.trim().toLowerCase();
  return Boolean(normalized) &&
    normalized !== "agent" &&
    !normalized.includes(":") &&
    !normalized.includes("-");
}

export function agentTagLabel(tag: string, t?: Translate): string {
  const label = tag.trim();
  const labelKey = agentTagLabelKey(label);
  if (!label || !labelKey || !t) {
    return label;
  }
  const key = `agentTags.${labelKey}`;
  const translated = t(key);
  return translated === key ? label : translated;
}

function agentTagLabelKey(tag: string): string {
  return tag
    .toLowerCase()
    .replace(/&/g, "and")
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/^-+|-+$/g, "");
}
