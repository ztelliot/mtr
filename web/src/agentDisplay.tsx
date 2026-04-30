import { Badge, Stack, Text } from "@mantine/core";
import type { Agent } from "./types";

export function ProviderCell({ provider, isp, fallback = "-", protocols }: { provider?: string; isp?: string; fallback?: string; protocols?: number }) {
  const label = primaryISPLabel(isp, fallback);
  const note = providerNote(provider, label);
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

export function agentSelectLabel(agent: Agent): string {
  return `${agent.region || agent.status} · ${agentISPLabel(agent)}`;
}

export function agentLocationProviderLabel(agent: Agent): string {
  return `${agentLocationLabel(agent.region || agent.id, agent.protocols)} · ${agentISPProviderLabel(agent)}`;
}

export function agentISPProviderLabel(agent: Agent): string {
  return withProviderNote(primaryISPLabel(agent.isp, agent.name || agent.id), agent.provider);
}

export function ispProviderLabel({ isp, provider, fallback = "-" }: { isp?: string; provider?: string; fallback?: string }): string {
  return withProviderNote(primaryISPLabel(isp, fallback), provider);
}

export function ispProtocolLabel({ isp, fallback = "-", protocols }: { isp?: string; fallback?: string; protocols?: number }): string {
  return withProtocolSuffix(primaryISPLabel(isp, fallback), protocols);
}

export function ispProtocolProviderLabel({ isp, provider, fallback = "-", protocols }: { isp?: string; provider?: string; fallback?: string; protocols?: number }): string {
  const label = primaryISPLabel(isp, fallback);
  return withProviderNote(withProtocolSuffix(label, protocols), provider, label);
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

function agentISPLabel(agent: Agent): string {
  return withProtocolSuffix(primaryISPLabel(agent.isp, agent.name || agent.id), agent.protocols);
}

function ProtocolSuffix({ protocols }: { protocols?: number }) {
  const suffix = agentProtocolSuffix(protocols);
  return suffix ? <span className="agent-protocol-suffix">[{suffix}]</span> : null;
}

function primaryISPLabel(isp: string | undefined, fallback: string): string {
  return cleanLabel(isp) || cleanLabel(fallback) || "-";
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

export function RegionCell({ country, region, protocols }: { country?: string; region: string; protocols?: number }) {
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
        {region}
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
