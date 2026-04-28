import { parseHostPort, requiresAgent } from "./jobForm";
import type { Agent, IPVersion, JobFormState, Permissions, Tool } from "./types";

export function toolAllowed(permissions: Permissions | null, tool: Tool): boolean {
  return !permissions || Boolean(permissions.tools?.[tool]);
}

export function canReadSchedules(permissions: Permissions | null): boolean {
  return permissions?.schedule_access === "read" || permissions?.schedule_access === "write";
}

export function canWriteSchedules(permissions: Permissions | null): boolean {
  return permissions?.schedule_access === "write";
}

export function requiresAgentForTool(permissions: Permissions | null, tool: Tool): boolean {
  return permissions?.tools?.[tool]?.requires_agent ?? requiresAgent(tool);
}

export function filterAgentsByPermissions(agents: Agent[], permissions: Permissions | null): Agent[] {
  const allowedAgents = permissions?.agents ?? [];
  if (!permissions || allowedAgents.length === 0 || allowedAgents.includes("*")) {
    return agents;
  }
  const allowed = new Set(allowedAgents);
  return agents.filter((agent) => allowed.has(agent.id));
}

export function permissionFormError(
  form: JobFormState,
  permissions: Permissions | null,
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
  if (form.agentId && !agentAllowedByPermissions(permissions, form.agentId)) {
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

function agentAllowedByPermissions(permissions: Permissions | null, agentID: string): boolean {
  const allowedAgents = permissions?.agents ?? [];
  return !permissions || allowedAgents.length === 0 || allowedAgents.includes("*") || allowedAgents.includes(agentID);
}

function permissionAllowsArg(permissions: Permissions | null, tool: Tool, arg: string, value: string): boolean {
  if (!permissions) {
    return true;
  }
  const toolPermission = permissions.tools?.[tool];
  if (!toolPermission?.allowed_args || !(arg in toolPermission.allowed_args)) {
    return false;
  }
  return valueAllowed(toolPermission.allowed_args[arg], value);
}

function valueAllowed(pattern: string | undefined, value: string): boolean {
  if (pattern === undefined || pattern === "") {
    return true;
  }
  try {
    return new RegExp(pattern).test(value);
  } catch {
    return true;
  }
}
