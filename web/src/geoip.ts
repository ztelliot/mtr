import type { GeoIPInfo } from "./types";

const ipv4Pattern =
  /^(25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)(\.(25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)){3}$/;
const defaultGeoIPConcurrency = 3;

export function normalizeIPAddress(value: string | null | undefined): string | undefined {
  const candidate = value?.trim();
  if (!candidate || candidate === "-" || candidate === "*") {
    return undefined;
  }
  const host = hostFromAddress(candidate);
  if (isIPv4(host) || isIPv6(host)) {
    return host;
  }
  return undefined;
}

export function ipFromDNSRecord(record: string): string | undefined {
  return normalizeIPAddress(splitDNSRecord(record).value);
}

export function splitDNSRecord(record: string): { type?: string; value: string } {
  const trimmed = record.trim();
  const match = trimmed.match(/^(\S+)\s+(.+)$/);
  if (!match) {
    return { value: trimmed };
  }
  return { type: match[1], value: match[2].trim() };
}

export function uniqueIPAddresses(values: Array<string | null | undefined>): string[] {
  return [...new Set(values.flatMap((value) => normalizeIPAddress(value) ?? []))];
}

export function formatASN(info: GeoIPInfo | null | undefined): string | undefined {
  if (!info) {
    return undefined;
  }
  const org = info.org?.trim();
  if (info.asn && org) {
    return `AS${info.asn} ${org}`;
  }
  if (info.asn) {
    return `AS${info.asn}`;
  }
  return org || undefined;
}

export function formatLocation(info: GeoIPInfo | null | undefined): string | undefined {
  if (!info) {
    return undefined;
  }
  return [info.city, info.region, info.country]
    .map((part) => part?.trim())
    .filter(Boolean)
    .join(", ") || undefined;
}

export async function fetchGeoIPQueued<T>(
  ips: string[],
  fetchOne: (ip: string) => Promise<T>,
  concurrency = defaultGeoIPConcurrency
): Promise<Array<readonly [string, T | null]>> {
  const queue = uniqueIPAddresses(ips);
  const limit = Math.max(1, Math.floor(concurrency));
  const out: Array<readonly [string, T | null]> = [];
  let index = 0;

  async function worker() {
    for (;;) {
      const current = index;
      index += 1;
      const ip = queue[current];
      if (!ip) {
        return;
      }
      try {
        out[current] = [ip, await fetchOne(ip)] as const;
      } catch {
        out[current] = [ip, null] as const;
      }
    }
  }

  await Promise.all(Array.from({ length: Math.min(limit, queue.length) }, () => worker()));
  return out.filter((entry): entry is readonly [string, T | null] => Boolean(entry));
}

function isIPv4(value: string): boolean {
  return ipv4Pattern.test(value);
}

function isIPv6(value: string): boolean {
  if (!value.includes(":") || !/^[0-9a-fA-F:.]+$/.test(value)) {
    return false;
  }
  const compressed = value.includes("::");
  if ((value.match(/::/g) ?? []).length > 1) {
    return false;
  }
  const halves = compressed ? value.split("::") : [value];
  const left = halves[0] ? halves[0].split(":") : [];
  const right = halves[1] ? halves[1].split(":") : [];
  const groups = countIPv6Groups([...left, ...right]);
  if (groups === undefined) {
    return false;
  }
  return compressed ? groups < 8 : groups === 8;
}

function hostFromAddress(value: string): string {
  if (value.startsWith("[")) {
    const end = value.indexOf("]");
    if (end > 0) {
      return value.slice(1, end);
    }
  }
  const ipv4HostPort = value.match(/^([^:]+):\d+$/);
  if (ipv4HostPort) {
    return ipv4HostPort[1];
  }
  return value;
}

function countIPv6Groups(parts: string[]): number | undefined {
  let groups = 0;
  for (let index = 0; index < parts.length; index += 1) {
    const part = parts[index];
    if (!part) {
      return undefined;
    }
    if (part.includes(".")) {
      if (index !== parts.length - 1 || !isIPv4(part)) {
        return undefined;
      }
      groups += 2;
      continue;
    }
    if (!/^[0-9a-fA-F]{1,4}$/.test(part)) {
      return undefined;
    }
    groups += 1;
  }
  return groups;
}
