import type { Tool, VersionInfo } from "./types";
import { formatVersionLabel } from "./versionLabel";

export function formatServerVersion(info: VersionInfo | null, t: (key: string) => string): string {
  if (!info?.version) {
    return t("footer.versionUnknown");
  }
  return formatVersionLabel(info.version, info.commit);
}

export function formatHTTPVersion(value?: string): string {
  const trimmed = value?.trim();
  if (!trimmed) {
    return "-";
  }
  switch (trimmed.toLowerCase()) {
    case "auto":
      return "Auto";
    case "1":
    case "1.0":
    case "1.1":
    case "h1":
    case "http/1":
    case "http/1.0":
    case "http/1.1":
      return "H1";
    case "2":
    case "2.0":
    case "h2":
    case "http/2":
    case "http/2.0":
      return "H2";
    case "3":
    case "3.0":
    case "h3":
    case "http/3":
    case "http/3.0":
      return "H3";
    default:
      return trimmed;
  }
}

export function formatActualProtocol(tool: Tool, value?: string): string {
  const trimmed = value?.trim();
  if (!trimmed) {
    return "-";
  }
  if (tool === "http") {
    return formatHTTPVersion(trimmed);
  }
  return trimmed.toLowerCase() === "auto" ? "Auto" : trimmed.toUpperCase();
}

export function formatMS(value?: number): string {
  return value === undefined ? "-" : `${value.toFixed(2)}ms`;
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
  return date.toLocaleString(undefined, { hour12: false });
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
    hour12: false,
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
  return date.toLocaleString(undefined, { month: "2-digit", day: "2-digit", hour: "2-digit", hour12: false, minute: "2-digit" });
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
