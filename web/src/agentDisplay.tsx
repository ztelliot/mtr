import { Badge, Stack, Text } from "@mantine/core";
import type { Agent } from "./types";

type Translate = (key: string, options?: Record<string, unknown>) => string;

export function ProviderCell({ provider, isp, fallback = "-", protocols, t }: { provider?: string; isp?: string; fallback?: string; protocols?: number; t?: Translate }) {
  const label = primaryISPLabel(isp, fallback, t);
  const note = providerNote(provider, cleanLabel(isp) || fallback);
  return (
    <Stack className="provider-cell" gap={0}>
      <Text className="mono-label provider-primary">
        {label}
        <ProtocolSuffix protocols={protocols} />
      </Text>
      {note && (
        <Text c="dimmed" className="provider-note">
          {note}
        </Text>
      )}
    </Stack>
  );
}

export function agentSelectLabel(agent: Agent, t?: Translate): string {
  return `${agentRegionOrFallback(agent.region, agent.status, t)} · ${agentISPLabel(agent, t)}`;
}

export function agentLocationProviderLabel(agent: Agent, t?: Translate): string {
  return `${agentLocationLabel(agentRegionOrFallback(agent.region, agent.id, t), agent.protocols)} · ${agentISPProviderLabel(agent, t)}`;
}

export function agentISPProviderLabel(agent: Agent, t?: Translate): string {
  return withProviderNote(primaryISPLabel(agent.isp, agent.name || agent.id, t), agent.provider, cleanLabel(agent.isp) || agent.name || agent.id);
}

export function ispProviderLabel({ isp, provider, fallback = "-", t }: { isp?: string; provider?: string; fallback?: string; t?: Translate }): string {
  return withProviderNote(primaryISPLabel(isp, fallback, t), provider, cleanLabel(isp) || fallback);
}

export function ispProtocolLabel({ isp, fallback = "-", protocols, t }: { isp?: string; fallback?: string; protocols?: number; t?: Translate }): string {
  return withProtocolSuffix(primaryISPLabel(isp, fallback, t), protocols);
}

export function ispProtocolProviderLabel({ isp, provider, fallback = "-", protocols, t }: { isp?: string; provider?: string; fallback?: string; protocols?: number; t?: Translate }): string {
  const label = primaryISPLabel(isp, fallback, t);
  return withProviderNote(withProtocolSuffix(label, protocols), provider, cleanLabel(isp) || fallback);
}

export function agentLocationLabel(label: string, protocols?: number): string {
  const suffix = agentProtocolSuffix(protocols);
  return suffix ? `${label} [${suffix}]` : label;
}

function agentProtocolSuffix(protocols?: number): "v4" | "v6" | null {
  const mask = protocols || 3;
  const hasV4 = (mask & 1) !== 0;
  const hasV6 = (mask & 2) !== 0;
  if (hasV4 && !hasV6) {
    return "v4";
  }
  if (hasV6 && !hasV4) {
    return "v6";
  }
  return null;
}

export function agentRegionLabel(region: string | undefined, t?: Translate): string {
  return localizedAgentLabel("agentRegions", region, t);
}

export function agentISPDisplayLabel(isp: string | undefined, t?: Translate): string {
  return localizedAgentLabel("agentISPs", isp, t);
}

function agentRegionOrFallback(region: string | undefined, fallback: string, t?: Translate): string {
  const label = cleanLabel(region);
  return label ? agentRegionLabel(label, t) : cleanLabel(fallback) || "-";
}

function agentISPLabel(agent: Agent, t?: Translate): string {
  return withProtocolSuffix(primaryISPLabel(agent.isp, agent.name || agent.id, t), agent.protocols);
}

function ProtocolSuffix({ protocols }: { protocols?: number }) {
  const suffix = agentProtocolSuffix(protocols);
  return suffix ? <span className="agent-protocol-suffix">[{suffix}]</span> : null;
}

function primaryISPLabel(isp: string | undefined, fallback: string, t?: Translate): string {
  const label = cleanLabel(isp);
  return label ? agentISPDisplayLabel(label, t) : cleanLabel(fallback) || "-";
}

function providerNote(provider: string | undefined, label: string): string | null {
  const normalizedProvider = cleanLabel(provider);
  if (!normalizedProvider || normalizedProvider.toLowerCase() === cleanLabel(label).toLowerCase()) {
    return null;
  }
  return normalizedProvider;
}

function withProtocolSuffix(label: string, protocols?: number): string {
  const suffix = agentProtocolSuffix(protocols);
  return suffix ? `${label} [${suffix}]` : label;
}

function withProviderNote(label: string, provider: string | undefined, compareLabel = label): string {
  const note = providerNote(provider, compareLabel);
  return note ? `${label} (${note})` : label;
}

function cleanLabel(value: string | undefined): string {
  return value?.trim() ?? "";
}

function localizedAgentLabel(namespace: "agentRegions" | "agentISPs", value: string | undefined, t?: Translate): string {
  const label = cleanLabel(value);
  if (!label || !t) {
    return label;
  }
  const key = `${namespace}.${agentLabelKey(label)}`;
  const translated = t(key);
  return translated === key ? label : translated;
}

function agentLabelKey(value: string): string {
  return value
    .trim()
    .toLowerCase()
    .replace(/&/g, "and")
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/^-+|-+$/g, "");
}

export function RegionCell({ country, region, protocols, t }: { country?: string; region: string; protocols?: number; t?: Translate }) {
  const flagCountry = flagCountryCode(country);
  return (
    <span className="region-cell">
      {flagCountry && (
        <img
          alt=""
          aria-hidden="true"
          className="region-flag"
          height={14}
          loading="lazy"
          onError={(event) => {
            event.currentTarget.hidden = true;
          }}
          src={`/flags/${flagCountry}.svg`}
          width={18}
        />
      )}
      <span>
        {agentRegionLabel(region, t)}
      </span>
    </span>
  );
}

export function flagCountryCode(value: string | undefined): string | null {
  const country = value?.trim().toUpperCase();
  if (!country || !/^[A-Z]{2}$/.test(country)) {
    return null;
  }
  return country;
}

export function StatusBadge({ status, t }: { status: string; t: (key: string) => string }) {
  const translated = t(`statusValues.${status}`);
  return (
    <Badge variant="light" color={statusColor(status)}>
      {translated === `statusValues.${status}` ? status : translated}
    </Badge>
  );
}

function statusColor(status: string): string {
  const code = Number(status);
  if (/^\d+$/.test(status)) {
    if (code >= 200 && code < 400) {
      return "green";
    }
    if (code >= 400) {
      return "red";
    }
    return "gray";
  }
  if (["succeeded", "open", "online", "enabled"].includes(status)) {
    return "green";
  }
  if (["failed", "closed", "error", "timeout"].includes(status)) {
    return "red";
  }
  return "gray";
}
