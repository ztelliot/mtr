import { parseHostPort, requiresAgent } from "./jobForm";
import type { Agent, IPVersion, JobFormState, Permissions, Tool } from "./types";

export function toolAllowed(permissions: Permissions | null, tool: Tool): boolean {
  return Boolean(permissions?.tools?.[tool]);
}

export function canReadSchedules(permissions: Permissions | null): boolean {
  return permissions?.schedule_access === "read" || permissions?.schedule_access === "write";
}

export function canWriteSchedules(permissions: Permissions | null): boolean {
  return permissions?.schedule_access === "write";
}

export function canReadManage(permissions: Permissions | null): boolean {
  return permissions?.manage_access === "read" || permissions?.manage_access === "write";
}

export function canWriteManage(permissions: Permissions | null): boolean {
  return permissions?.manage_access === "write";
}

export function requiresAgentForTool(permissions: Permissions | null, tool: Tool): boolean {
  return permissions?.tools?.[tool]?.requires_agent ?? requiresAgent(tool);
}

export function filterAgentsByPermissions(agents: Agent[], permissions: Permissions | null): Agent[] {
  const allowedAgents = permissions?.agents ?? [];
  if (!permissions) {
    return [];
  }
  const allowedIDs = new Set(allowedAgents.filter((value) => !value.startsWith("!") && !value.startsWith("tag:")));
  const allowedTags = new Set(allowedAgents.filter((value) => value.startsWith("tag:")).map((value) => value.slice(4)));
  const deniedIDs = new Set(allowedAgents.filter((value) => value.startsWith("!") && !value.startsWith("!tag:")).map((value) => value.slice(1)));
  const deniedTags = new Set(allowedAgents.filter((value) => value.startsWith("!tag:")).map((value) => value.slice(5)));
  const hasPositiveScope = allowedIDs.size > 0 || allowedTags.size > 0;
  const allowAll = allowedAgents.length === 0 || allowedAgents.includes("*") || !hasPositiveScope;
  return agents.filter((agent) => {
    if (deniedIDs.has(agent.id) || agentHasAnyLabel(agent, deniedTags)) {
      return false;
    }
    return allowAll || allowedIDs.has(agent.id) || agentHasAnyLabel(agent, allowedTags);
  });
}

export function permissionFormError(
  form: JobFormState,
  permissions: Permissions | null,
  agents: Agent[],
  t: (key: string, options?: Record<string, unknown>) => string
): string | null {
  if (!toolAllowed(permissions, form.tool)) {
    return t("errors.toolNotAllowed", { tool: form.tool });
  }
  const tool = permissions?.tools?.[form.tool];
  if (!tool) {
    return null;
  }
  if (tool.ip_versions?.length && !tool.ip_versions.includes(form.ipVersion)) {
    return t("errors.ipVersionNotAllowed", { version: form.ipVersion });
  }
  if (form.agentId && !agentAllowedByPermissions(permissions, form.agentId, agents)) {
    return t("errors.agentNotAllowed");
  }
  if ((form.tool === "ping" || form.tool === "mtr" || form.tool === "traceroute") && !permissionAllowsArg(permissions, form.tool, "protocol", form.protocol)) {
    return t("errors.optionNotAllowed", { option: t("form.protocol") });
  }
  if (form.tool === "dns" && !permissionAllowsArg(permissions, "dns", "type", form.dnsType)) {
    return t("errors.optionNotAllowed", { option: t("form.recordType") });
  }
  if (form.tool === "http" && !permissionAllowsArg(permissions, "http", "method", form.method)) {
    return t("errors.optionNotAllowed", { option: t("form.method") });
  }
  if (form.tool === "port") {
    const parsed = parseHostPort(form.target);
    if (parsed && !permissionAllowsArg(permissions, "port", "port", parsed.port)) {
      return t("errors.optionNotAllowed", { option: t("form.port") });
    }
  }
  return null;
}

export function localizedFormError(
  form: JobFormState,
  permissions: Permissions | null,
  t: (key: string, options?: Record<string, unknown>) => string
) {
  if (!form.target.trim()) {
    return t("errors.targetRequired");
  }
  if (requiresAgentForTool(permissions, form.tool) && !form.agentId) {
    return t("errors.agentRequired", { tool: form.tool });
  }
  if (form.tool === "port" && !parseHostPort(form.target)) {
    return t("errors.portRequired");
  }
  return null;
}

export function formWithPermissionDefaults(form: JobFormState, permissions: Permissions | null): JobFormState {
  const forcedRemoteDNS = permissions?.tools?.[form.tool]?.resolve_on_agent;
  return forcedRemoteDNS === undefined ? form : { ...form, resolveOnAgent: forcedRemoteDNS };
}

export function resolveOnAgentValue(permissions: Permissions | null, form: JobFormState): boolean {
  return permissions?.tools?.[form.tool]?.resolve_on_agent ?? form.resolveOnAgent;
}

export function ipVersionOptions(permissions: Permissions | null, tool: Tool): Array<{ value: string; label: string }> {
  const toolPermission = permissions?.tools?.[tool];
  if (permissions && !toolPermission) {
    return [];
  }
  const allowed = toolPermission?.ip_versions;
  const options = [
    { value: "0", label: "Auto" },
    { value: "4", label: "IPv4" },
    { value: "6", label: "IPv6" }
  ];
  return allowed?.length ? options.filter((option) => allowed.includes(Number(option.value) as IPVersion)) : options;
}

export function protocolOptions(permissions: Permissions | null, tool: Tool): Array<{ value: JobFormState["protocol"]; label: string }> {
  const options: Array<{ value: JobFormState["protocol"]; label: string }> = [
    { value: "icmp", label: "ICMP" },
    { value: "tcp", label: "TCP" }
  ];
  return options.filter((option) => permissionAllowsArg(permissions, tool, "protocol", option.value));
}

export function dnsTypeOptions(permissions: Permissions | null): string[] {
  return ["A", "AAAA", "CNAME", "MX", "TXT", "NS"].filter((value) => permissionAllowsArg(permissions, "dns", "type", value));
}

export function httpMethodOptions(permissions: Permissions | null): Array<{ value: JobFormState["method"]; label: string }> {
  const options: Array<{ value: JobFormState["method"]; label: string }> = [
    { value: "HEAD", label: "HEAD" },
    { value: "GET", label: "GET" }
  ];
  return options.filter((option) => permissionAllowsArg(permissions, "http", "method", option.value));
}

function agentAllowedByPermissions(permissions: Permissions | null, agentID: string, agents: Agent[] = []): boolean {
  const allowedAgents = permissions?.agents ?? [];
  const agent = agents.find((item) => item.id === agentID);
  const allowedIDs = new Set(allowedAgents.filter((value) => !value.startsWith("!") && !value.startsWith("tag:")));
  const allowedTags = new Set(allowedAgents.filter((value) => value.startsWith("tag:")).map((value) => value.slice(4)));
  const deniedIDs = new Set(allowedAgents.filter((value) => value.startsWith("!") && !value.startsWith("!tag:")).map((value) => value.slice(1)));
  const deniedTags = new Set(allowedAgents.filter((value) => value.startsWith("!tag:")).map((value) => value.slice(5)));
  const hasPositiveScope = allowedIDs.size > 0 || allowedTags.size > 0;
  const allowAll = allowedAgents.length === 0 || allowedAgents.includes("*") || !hasPositiveScope;
  const tagAllowed = agent ? agentHasAnyLabel(agent, allowedTags) : false;
  const tagDenied = agent ? agentHasAnyLabel(agent, deniedTags) : false;
  return Boolean(
    permissions &&
      !deniedIDs.has(agentID) &&
      !tagDenied &&
      (allowAll || allowedIDs.has(agentID) || tagAllowed)
  );
}

function permissionAllowsArg(permissions: Permissions | null, tool: Tool, arg: string, value: string): boolean {
  if (!permissions) {
    return true;
  }
  const toolPermission = permissions.tools?.[tool];
  if (!toolPermission) {
    return false;
  }
  if (!toolPermission.allowed_args) {
    return true;
  }
  if (!(arg in toolPermission.allowed_args)) {
    return false;
  }
  const rule = toolPermission.allowed_args[arg];
  return arg === "port" ? portAllowed(rule, value) : csvValueAllowed(rule, value);
}

function csvValueAllowed(rule: string | undefined, value: string): boolean {
  if (rule === undefined || rule.trim() === "") {
    return true;
  }
  return rule.split(",").map((item) => item.trim()).includes(value);
}

function portAllowed(rule: string | undefined, value: string): boolean {
  const portValue = value.trim();
  if (!/^\d+$/.test(portValue)) {
    return false;
  }
  const port = Number(portValue);
  if (!Number.isInteger(port) || port < 1 || port > 65535) {
    return false;
  }
  return parsePortRanges(rule).some(([start, end]) => port >= start && port <= end);
}

function parsePortRanges(rule: string | undefined): Array<[number, number]> {
  const normalized = rule?.trim() || "1-65535";
  const ranges: Array<[number, number]> = [];

  for (const rawPart of normalized.split(",")) {
    const part = rawPart.trim();
    if (!part) {
      continue;
    }
    const bounds = part.split("-");
    if (bounds.length > 2 || !bounds[0]) {
      return [];
    }
    const start = Number(bounds[0].trim());
    const end = bounds.length === 2 ? Number(bounds[1].trim()) : start;
    if (!Number.isInteger(start) || !Number.isInteger(end) || start < 1 || end > 65535 || start > end) {
      return [];
    }
    ranges.push([start, end]);
  }

  return ranges;
}

function agentHasAnyLabel(agent: Agent, labels: Set<string>): boolean {
  if (labels.size === 0) {
    return false;
  }
  return (agent.labels ?? []).some((label) => labels.has(label));
}
