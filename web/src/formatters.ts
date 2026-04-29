import type { VersionInfo } from "./types";
import { formatVersionLabel } from "./versionLabel";

export function formatServerVersion(info: VersionInfo | null, t: (key: string) => string): string {
  if (!info?.version) {
    return t("footer.versionUnknown");
  }
  return formatVersionLabel(info.version, info.commit);
}

export function formatMS(value?: number): string {
  return value === undefined ? "-" : `${value.toFixed(2)} ms`;
}

export function routeRTT(row: { lastMS?: number; avgMS?: number; bestMS?: number; worstMS?: number }): number | undefined {
  return row.lastMS ?? row.avgMS ?? row.bestMS ?? row.worstMS;
}

export function formatSpeed(value?: number): string {
  if (value === undefined) {
    return "-";
  }
  if (value >= 1024 * 1024) {
    return `${(value / 1024 / 1024).toFixed(2)} MB/s`;
  }
  if (value >= 1024) {
    return `${(value / 1024).toFixed(2)} KB/s`;
  }
  return `${value.toFixed(0)} B/s`;
}

export function formatBytes(value?: number): string {
  if (value === undefined) {
    return "-";
  }
  if (value >= 1024 * 1024) {
    return `${(value / 1024 / 1024).toFixed(2)} MB`;
  }
  if (value >= 1024) {
    return `${(value / 1024).toFixed(2)} KB`;
  }
  return `${value.toFixed(0)} B`;
}

export function formatPercent(value?: number): string {
  return value === undefined ? "-" : `${value.toFixed(1)}%`;
}

export function formatDateTime(value?: string): string {
  if (!value) {
    return "-";
  }
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) {
    return value;
  }
  return date.toLocaleString();
}

export function formatHistoryDateTime(value?: string): string {
  if (!value) {
    return "-";
  }
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) {
    return value;
  }
  return date.toLocaleString(undefined, {
    day: "2-digit",
    hour: "2-digit",
    minute: "2-digit",
    month: "2-digit",
    second: "2-digit"
  });
}

export function formatShortDateTime(value: number): string {
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) {
    return "-";
  }
  return date.toLocaleString(undefined, { month: "2-digit", day: "2-digit", hour: "2-digit", minute: "2-digit" });
}

export function formatInterval(seconds: number): string {
  if (seconds % 3600 === 0) {
    return `${seconds / 3600}h`;
  }
  if (seconds % 60 === 0) {
    return `${seconds / 60}m`;
  }
  return `${seconds}s`;
}
