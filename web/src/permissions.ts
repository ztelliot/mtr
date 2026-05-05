import { parseHostPort, requiresAgent } from "./jobForm";
import type { Agent, IPVersion, JobFormState, PermissionTool, Permissions, Tool } from "./types";

const hostnamePattern = /^[a-zA-Z0-9.-]{1,253}$/;
const ipv4Pattern = /^(25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)(\.(25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)){3}$/;
const forbiddenTargetChars = /[ \t\r\n;&|`$<>]/;

const toolRequiredArg: Partial<Record<Tool, string>> = {
  dns: "type",
  http: "method",
  mtr: "protocol",
  ping: "protocol",
  port: "port",
  traceroute: "protocol"
};

export function effectivePermissions(permissions: Permissions | null, agent?: Agent | null): Permissions | null {
  if (!permissions || !agent) {
    return permissions;
  }
  return { ...permissions, tools: intersectToolPermissions(permissions.tools, agent.tools ?? {}) };
}

export function toolAllowed(permissions: Permissions | null, tool: Tool): boolean {
  return Boolean(permissions?.tools?.[tool]);
}

export function toolAllowedForAgent(permissions: Permissions | null, tool: Tool, agent?: Agent | null): boolean {
  return toolAllowed(effectivePermissions(permissions, agent), tool);
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
  if (!permissions) {
    return [];
  }
  return agents.filter((agent) => Object.keys(effectivePermissions(permissions, agent)?.tools ?? {}).length > 0);
}

function intersectToolPermissions(tokenTools: Permissions["tools"], agentTools: Permissions["tools"]): Permissions["tools"] {
  const out: Permissions["tools"] = {};
  for (const tool of Object.keys(tokenTools) as Tool[]) {
    const tokenTool = tokenTools[tool];
    const agentTool = agentTools[tool];
    const merged = tokenTool && agentTool ? intersectPermissionTool(tool, tokenTool, agentTool) : undefined;
    if (merged) {
      out[tool] = merged;
    }
  }
  return out;
}

function intersectPermissionTool(tool: Tool, tokenTool: PermissionTool, agentTool: PermissionTool): PermissionTool | null {
  if (agentTool.allowed_args === undefined) {
    return null;
  }
  const resolveOnAgent = intersectResolveOnAgent(tokenTool.resolve_on_agent, agentTool.resolve_on_agent);
  if (resolveOnAgent === "conflict") {
    return null;
  }
  const allowedArgs = intersectAllowedArgs(tokenTool.allowed_args, agentTool.allowed_args);
  const ipVersions = intersectIPVersions(tokenTool.ip_versions, agentTool.ip_versions);
  const merged: PermissionTool = {
    requires_agent: tokenTool.requires_agent || agentTool.requires_agent,
    ...(allowedArgs !== undefined ? { allowed_args: allowedArgs } : {}),
    ...(resolveOnAgent !== undefined ? { resolve_on_agent: resolveOnAgent } : {}),
    ...(ipVersions !== undefined ? { ip_versions: ipVersions } : {})
  };
  return toolPermissionUsable(tool, merged) ? merged : null;
}

function intersectResolveOnAgent(left: boolean | undefined, right: boolean | undefined): boolean | undefined | "conflict" {
  if (left === undefined) {
    return right;
  }
  if (right === undefined || left === right) {
    return left;
  }
  return "conflict";
}

function intersectIPVersions(left: IPVersion[] | undefined, right: IPVersion[] | undefined): IPVersion[] | undefined {
  if (left === undefined) {
    return right ? [...right] : undefined;
  }
  if (right === undefined) {
    return [...left];
  }
  return left.filter((version) => right.includes(version));
}

function intersectAllowedArgs(left: Record<string, string> | undefined, right: Record<string, string> | undefined): Record<string, string> | undefined {
  if (left === undefined) {
    return right ? { ...right } : undefined;
  }
  if (right === undefined) {
    return { ...left };
  }
  const out: Record<string, string> = {};
  for (const [arg, leftRule] of Object.entries(left)) {
    const rightRule = right[arg];
    if (rightRule === undefined) {
      continue;
    }
    if (arg === "port") {
      const rule = intersectPortRules(leftRule, rightRule);
      if (rule) {
        out[arg] = rule;
      }
      continue;
    }
    const values = intersectCSVValues(leftRule, rightRule);
    if (values.length > 0) {
      out[arg] = values.join(",");
    }
  }
  return out;
}

function toolPermissionUsable(tool: Tool, permission: PermissionTool): boolean {
  if (permission.ip_versions?.length === 0) {
    return false;
  }
  const requiredArg = toolRequiredArg[tool];
  if (!requiredArg || permission.allowed_args === undefined) {
    return true;
  }
  const rule = permission.allowed_args[requiredArg];
  if (rule === undefined) {
    return false;
  }
  return requiredArg === "port" ? parsePortRanges(rule).length > 0 : rule.trim() === "" || csvValues(rule).length > 0;
}

export function permissionFormError(
  form: JobFormState,
  permissions: Permissions | null,
  agents: Agent[],
  t: (key: string, options?: Record<string, unknown>) => string
): string | null {
  const agent = findAgent(agents, form.agentId);
  const effective = effectivePermissions(permissions, agent);
  if (!toolAllowed(effective, form.tool)) {
    return t("errors.toolNotAllowed", { tool: form.tool });
  }
  const tool = effective?.tools?.[form.tool];
  if (!tool) {
    return null;
  }
  if (tool.ip_versions && !tool.ip_versions.includes(form.ipVersion)) {
    return t("errors.ipVersionNotAllowed", { version: form.ipVersion });
  }
  const literalVersion = literalTargetIPVersion(form);
  if (literalVersion !== null && form.tool !== "dns" && form.ipVersion !== 0 && form.ipVersion !== literalVersion) {
    return t("errors.ipVersionNotAllowed", { version: form.ipVersion });
  }
  const requiredVersion = literalVersion !== null && (form.tool === "dns" || form.ipVersion === 0) ? literalVersion : form.tool === "dns" ? 0 : form.ipVersion;
  if (agent && !agentSupportsIPVersion(agent, requiredVersion)) {
    return t("errors.ipVersionNotAllowed", { version: requiredVersion });
  }
  if (form.agentId && !agentAllowedByPermissions(permissions, form.agentId, agents)) {
    return t("errors.agentNotAllowed");
  }
  if ((form.tool === "ping" || form.tool === "mtr" || form.tool === "traceroute") && !permissionAllowsArg(effective, form.tool, "protocol", form.protocol)) {
    return t("errors.optionNotAllowed", { option: t("form.protocol") });
  }
  if (form.tool === "dns" && !permissionAllowsArg(effective, "dns", "type", form.dnsType)) {
    return t("errors.optionNotAllowed", { option: t("form.recordType") });
  }
  if (form.tool === "http" && !permissionAllowsArg(effective, "http", "method", form.method)) {
    return t("errors.optionNotAllowed", { option: t("form.method") });
  }
  if (form.tool === "port") {
    const parsed = parseHostPort(form.target);
    if (parsed && !permissionAllowsArg(effective, "port", "port", parsed.port)) {
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
  const target = form.target.trim();
  if (!target) {
    return t("errors.targetRequired");
  }
  if (form.tool === "port" && !parseHostPort(form.target)) {
    return t("errors.portRequired");
  }
  const targetError = targetFormError(form, t);
  if (targetError) {
    return targetError;
  }
  if (requiresAgentForTool(permissions, form.tool) && !form.agentId) {
    return t("errors.agentRequired", { tool: form.tool });
  }
  return null;
}

export function formWithPermissionDefaults(form: JobFormState, permissions: Permissions | null, agents: Agent[] = []): JobFormState {
  const forcedRemoteDNS = effectivePermissions(permissions, findAgent(agents, form.agentId))?.tools?.[form.tool]?.resolve_on_agent;
  return forcedRemoteDNS === undefined ? form : { ...form, resolveOnAgent: forcedRemoteDNS };
}

export function resolveOnAgentValue(permissions: Permissions | null, form: JobFormState, agents: Agent[] = []): boolean {
  return effectivePermissions(permissions, findAgent(agents, form.agentId))?.tools?.[form.tool]?.resolve_on_agent ?? form.resolveOnAgent;
}

export function ipVersionOptions(permissions: Permissions | null, tool: Tool, agent?: Agent | null): Array<{ value: string; label: string }> {
  const effective = effectivePermissions(permissions, agent);
  const toolPermission = effective?.tools?.[tool];
  if (effective && !toolPermission) {
    return [];
  }
  const allowed = toolPermission?.ip_versions;
  const options = [
    { value: "0", label: "Auto" },
    { value: "4", label: "IPv4" },
    { value: "6", label: "IPv6" }
  ];
  const permissionOptions = allowed ? options.filter((option) => allowed.includes(Number(option.value) as IPVersion)) : options;
  return agent ? permissionOptions.filter((option) => agentSupportsIPVersion(agent, Number(option.value) as IPVersion)) : permissionOptions;
}

export function protocolOptions(permissions: Permissions | null, tool: Tool, agent?: Agent | null): Array<{ value: JobFormState["protocol"]; label: string }> {
  const options: Array<{ value: JobFormState["protocol"]; label: string }> = [
    { value: "icmp", label: "ICMP" },
    { value: "tcp", label: "TCP" }
  ];
  return options.filter((option) => permissionAllowsArg(effectivePermissions(permissions, agent), tool, "protocol", option.value));
}

export function dnsTypeOptions(permissions: Permissions | null, agent?: Agent | null): string[] {
  return ["A", "AAAA", "CNAME", "MX", "TXT", "NS"].filter((value) => permissionAllowsArg(effectivePermissions(permissions, agent), "dns", "type", value));
}

export function httpMethodOptions(permissions: Permissions | null, agent?: Agent | null): Array<{ value: JobFormState["method"]; label: string }> {
  const options: Array<{ value: JobFormState["method"]; label: string }> = [
    { value: "HEAD", label: "HEAD" },
    { value: "GET", label: "GET" }
  ];
  return options.filter((option) => permissionAllowsArg(effectivePermissions(permissions, agent), "http", "method", option.value));
}

function agentAllowedByPermissions(permissions: Permissions | null, agentID: string, agents: Agent[] = []): boolean {
  if (!permissions) {
    return false;
  }
  return Boolean(findAgent(agents, agentID));
}

function findAgent(agents: Agent[], agentID?: string): Agent | null {
  if (!agentID) {
    return null;
  }
  return agents.find((agent) => agent.id === agentID) ?? null;
}

function literalTargetIPVersion(form: JobFormState): IPVersion | null {
  const ip = literalTargetIPAddress(form);
  if (!ip) {
    return null;
  }
  return ipVersionForIPAddress(ip);
}

function literalTargetIPAddress(form: JobFormState): string | null {
  const host = targetHostForTool(form);
  return normalizeStrictIPAddress(host) ?? null;
}

function targetHostForTool(form: JobFormState): string {
  if (form.tool === "http") {
    try {
      const url = new URL(form.target);
      return unbracketHost(url.hostname);
    } catch {
      return "";
    }
  }
  if (form.tool === "port") {
    return unbracketHost(parseHostPort(form.target)?.host ?? form.target.trim());
  }
  return form.target.trim();
}

function targetFormError(form: JobFormState, t: (key: string, options?: Record<string, unknown>) => string): string | null {
  if (form.tool === "http") {
    if (form.target.trim().length > 512) {
      return t("errors.targetRequired");
    }
    const url = parseHTTPURL(form.target);
    if (!url) {
      return t("errors.httpTargetRequired");
    }
    return literalTargetFormError(form, unbracketHost(url.hostname), t);
  }

  if (form.tool === "port") {
    const parsed = parseHostPort(form.target);
    if (!parsed) {
      return null;
    }
    const hostError = plainTargetHostError(parsed.host, t);
    return hostError ?? literalTargetFormError(form, parsed.host, t);
  }

  const host = form.target.trim();
  const hostError = plainTargetHostError(host, t);
  return hostError ?? literalTargetFormError(form, host, t);
}

function parseHTTPURL(value: string): URL | null {
  try {
    const url = new URL(value.trim());
    return (url.protocol === "http:" || url.protocol === "https:") && url.host ? url : null;
  } catch {
    return null;
  }
}

function plainTargetHostError(host: string, t: (key: string, options?: Record<string, unknown>) => string): string | null {
  if (!host || host.length > 512) {
    return t("errors.targetRequired");
  }
  if (host.includes("://") || forbiddenTargetChars.test(host)) {
    return t("errors.targetForbiddenChars");
  }
  if (normalizeStrictIPAddress(host)) {
    return null;
  }
  if (!hostnamePattern.test(host) || host.includes("..")) {
    return t("errors.targetHostRequired");
  }
  return null;
}

function literalTargetFormError(
  form: JobFormState,
  host: string,
  t: (key: string, options?: Record<string, unknown>) => string
): string | null {
  const ip = normalizeStrictIPAddress(host);
  if (!ip) {
    return null;
  }
  const literalVersion = ipVersionForIPAddress(ip);
  if (form.tool !== "dns" && form.ipVersion === 4 && literalVersion !== 4) {
    return t("errors.targetAddressNotIPv4", { address: ip });
  }
  if (form.tool !== "dns" && form.ipVersion === 6 && literalVersion !== 6) {
    return t("errors.targetAddressNotIPv6", { address: ip });
  }
  if (targetAddressNotAllowed(ip)) {
    return t("errors.targetAddressNotAllowed", { address: mappedIPv4Address(ip) ?? ip });
  }
  return null;
}

function agentSupportsIPVersion(agent: Agent, version: IPVersion): boolean {
  if (version === 0) {
    return true;
  }
  const protocols = agent.protocols || 3;
  const required = version === 4 ? 1 : 2;
  return (protocols & required) !== 0;
}

function isIPv4MappedIPv6(value: string): boolean {
  return mappedIPv4Address(value) !== null;
}

function mappedIPv4Address(value: string): string | null {
  const normalized = value.toLowerCase();
  const groups = parseIPv6Groups(normalized);
  if (!groups || groups.length !== 8 || !groups.slice(0, 5).every((group) => group === 0) || groups[5] !== 0xffff) {
    return null;
  }
  const high = groups[6];
  const low = groups[7];
  return [high >> 8, high & 255, low >> 8, low & 255].join(".");
}

function normalizeStrictIPAddress(value: string): string | undefined {
  const host = value.trim();
  return isStrictIPv4(host) || isStrictIPv6(host) ? host : undefined;
}

function ipVersionForIPAddress(value: string): IPVersion {
  return isIPv4MappedIPv6(value) ? 4 : value.includes(":") ? 6 : 4;
}

function targetAddressNotAllowed(value: string): boolean {
  const mapped = mappedIPv4Address(value);
  if (mapped) {
    return targetAddressNotAllowed(mapped);
  }
  if (isStrictIPv4(value)) {
    return ipv4AddressNotAllowed(value);
  }
  return ipv6AddressNotAllowed(value);
}

function ipv4AddressNotAllowed(value: string): boolean {
  const bytes = parseIPv4Bytes(value);
  if (!bytes) {
    return false;
  }
  const first = bytes[0];
  const second = bytes[1];
  return first === 0 ||
    first === 10 ||
    (first === 100 && second >= 64 && second <= 127) ||
    first === 127 ||
    (first === 169 && second === 254) ||
    (first === 172 && second >= 16 && second <= 31) ||
    (first === 192 && second === 0) ||
    (first === 192 && second === 168) ||
    (first === 198 && (second === 18 || second === 19)) ||
    (first === 198 && second === 51 && bytes[2] === 100) ||
    (first === 203 && second === 0 && bytes[2] === 113) ||
    first >= 224;
}

function ipv6AddressNotAllowed(value: string): boolean {
  const groups = parseIPv6Groups(value);
  if (!groups) {
    return false;
  }
  const first = groups[0];
  const unspecified = groups.every((group) => group === 0);
  const loopback = groups.slice(0, 7).every((group) => group === 0) && groups[7] === 1;
  return unspecified ||
    loopback ||
    (first & 0xfe00) === 0xfc00 ||
    (first & 0xffc0) === 0xfe80 ||
    (first & 0xff00) === 0xff00;
}

function isStrictIPv4(value: string): boolean {
  return ipv4Pattern.test(value);
}

function isStrictIPv6(value: string): boolean {
  return parseIPv6Groups(value) !== null;
}

function parseIPv4Bytes(value: string): number[] | null {
  if (!ipv4Pattern.test(value)) {
    return null;
  }
  return value.split(".").map((part) => Number(part));
}

function parseIPv6Groups(value: string): number[] | null {
  const normalized = value.toLowerCase();
  if (!normalized.includes(":") || !/^[0-9a-f:.]+$/.test(normalized) || (normalized.match(/::/g) ?? []).length > 1) {
    return null;
  }
  const compressed = normalized.includes("::");
  const [leftRaw, rightRaw = ""] = compressed ? normalized.split("::") : [normalized, ""];
  const left = parseIPv6Side(leftRaw);
  const right = parseIPv6Side(rightRaw);
  if (!left || !right) {
    return null;
  }
  const missing = 8 - left.length - right.length;
  if ((compressed && missing <= 0) || (!compressed && missing !== 0)) {
    return null;
  }
  return [...left, ...Array.from({ length: missing }, () => 0), ...right];
}

function parseIPv6Side(value: string): number[] | null {
  if (!value) {
    return [];
  }
  const groups: number[] = [];
  const parts = value.split(":");
  for (let index = 0; index < parts.length; index += 1) {
    const part = parts[index];
    if (!part) {
      return null;
    }
    if (part.includes(".")) {
      if (index !== parts.length - 1) {
        return null;
      }
      const bytes = parseIPv4Bytes(part);
      if (!bytes) {
        return null;
      }
      groups.push((bytes[0] << 8) + bytes[1], (bytes[2] << 8) + bytes[3]);
      continue;
    }
    if (!/^[0-9a-f]{1,4}$/.test(part)) {
      return null;
    }
    groups.push(parseInt(part, 16));
  }
  return groups;
}

function unbracketHost(value: string): string {
  return value.startsWith("[") && value.endsWith("]") ? value.slice(1, -1) : value;
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
  if (rule === undefined) {
    return true;
  }
  const values = csvValues(rule);
  return values.length === 0 || values.includes(value);
}

function csvValues(rule: string | undefined): string[] {
  return (rule ?? "").split(",").map((item) => item.trim()).filter(Boolean);
}

function intersectCSVValues(leftRule: string | undefined, rightRule: string | undefined): string[] {
  const left = csvValues(leftRule);
  const right = new Set(csvValues(rightRule));
  return left.filter((value) => right.has(value));
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

function intersectPortRules(leftRule: string | undefined, rightRule: string | undefined): string {
  const out: string[] = [];
  for (const [leftStart, leftEnd] of parsePortRanges(leftRule)) {
    for (const [rightStart, rightEnd] of parsePortRanges(rightRule)) {
      const start = Math.max(leftStart, rightStart);
      const end = Math.min(leftEnd, rightEnd);
      if (start <= end) {
        out.push(start === end ? String(start) : `${start}-${end}`);
      }
    }
  }
  return out.join(",");
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
