import { Badge, Stack, Text } from "@mantine/core";
import type { Agent } from "./types";

export function ProviderCell({ provider, isp }: { provider: string; isp?: string }) {
  return (
    <Stack gap={0}>
      <Text className="mono-label">{provider}</Text>
      {isp && (
        <Text c="dimmed" size="xs">
          {isp}
        </Text>
      )}
    </Stack>
  );
}

export function agentSelectLabel(agent: Agent): string {
  return `${agent.provider || agent.id} · ${agentLocationLabel(agent.region || agent.status, agent.protocols)}`;
}

export function agentLocationProviderLabel(agent: Agent): string {
  return `${agentLocationLabel(agent.region || agent.id, agent.protocols)} · ${agent.provider || agent.name || agent.id}`;
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

export function RegionCell({ country, region, protocols }: { country?: string; region: string; protocols?: number }) {
  const flagCountry = flagCountryCode(country);
  const suffix = agentProtocolSuffix(protocols);
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
        {suffix && <span className="agent-protocol-suffix">[{suffix}]</span>}
      </span>
    </span>
  );
}

function flagCountryCode(value: string | undefined): string | null {
  const country = value?.trim().toUpperCase();
  if (!country || !/^[A-Z]{2}$/.test(country)) {
    return null;
  }
  return ["HK", "MO", "TW"].includes(country) ? "CN" : country;
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
