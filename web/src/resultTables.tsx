import { ScrollArea, Table, Text } from "@mantine/core";
import type React from "react";
import { ProviderCell, RegionCell, StatusBadge } from "./agentDisplay";
import { formatMS, formatPercent, formatSpeed, routeRTT } from "./formatters";
import { formatASN, formatLocation, ipFromDNSRecord, normalizeIPAddress, splitDNSRecord, uniqueIPAddresses } from "./geoip";
import { buildMtrRows, buildNodeRows } from "./pingRows";
import type { Agent, GeoIPInfo, Tool } from "./types";

export type GeoIPLookup = Record<string, GeoIPInfo | null | undefined>;

export function NodeResultTable({
  tool,
  rows,
  compact,
  geoIPByIP,
  onTraceIP,
  showHTTPDownloadMetrics = false,
  t
}: {
  tool: Tool;
  rows: ReturnType<typeof buildNodeRows>;
  compact: boolean;
  geoIPByIP: GeoIPLookup;
  onTraceIP: (ip: string, agentId: string) => void;
  showHTTPDownloadMetrics?: boolean;
  t: (key: string) => string;
}) {
  const hasIPv6 = rowsContainIPv6(rows);
  const ipWidthClass = hasIPv6 ? "has-ipv6" : "has-ipv4";
  if (tool === "ping") {
    return (
      <ScrollArea>
        <Table className={tableClass(ipWidthClass, compact)} striped highlightOnHover verticalSpacing="xs" miw={pingTableMinWidth(hasIPv6, compact)}>
          <Table.Thead>
            <Table.Tr>
              <Table.Th className="region-column">{t("results.region")}</Table.Th>
              <Table.Th className="provider-column">{t("results.provider")}</Table.Th>
              <Table.Th className="ip-column">{t("results.ip")}</Table.Th>
              <Table.Th className="metric-column">{t("results.loss")}</Table.Th>
              <Table.Th className="sent-column">{t("results.sent")}</Table.Th>
              <Table.Th className="time-column">{t("results.last")}</Table.Th>
              <Table.Th className="time-column">{t("results.avg")}</Table.Th>
              <Table.Th className="time-column">{t("results.best")}</Table.Th>
              <Table.Th className="time-column">{t("results.worst")}</Table.Th>
              <Table.Th className="time-column">{t("results.stdev")}</Table.Th>
              <Table.Th className="chart-column">{t("results.chart")}</Table.Th>
            </Table.Tr>
          </Table.Thead>
          <Table.Tbody>{nodeRows(rows, 11, t("results.noPingRows"), (row) => (
            <>
              <Table.Td className="region-column"><RegionCell country={row.country} region={row.region} protocols={row.protocols} t={t} /></Table.Td>
              <Table.Td className="provider-column"><ProviderCell provider={row.provider} isp={row.isp} protocols={row.protocols} t={t} /></Table.Td>
              <Table.Td className="ip-column"><GeoIPValue value={row.ip} geoIPByIP={geoIPByIP} onTraceIP={(ip) => onTraceIP(ip, row.agentId)} /></Table.Td>
              <Table.Td className="metric-column">{formatPercent(row.lossPct)}</Table.Td>
              <Table.Td className="sent-column">{row.sent ?? "-"}</Table.Td>
              <Table.Td className="time-column">{formatMS(row.lastMS)}</Table.Td>
              <Table.Td className="time-column">{formatMS(row.avgMS)}</Table.Td>
              <Table.Td className="time-column">{formatMS(row.bestMS)}</Table.Td>
              <Table.Td className="time-column">{formatMS(row.worstMS)}</Table.Td>
              <Table.Td className="time-column">{formatMS(row.stdevMS)}</Table.Td>
              <Table.Td className="chart-column"><PingRTTChart samples={row.rttSamples} lossPct={row.lossPct} /></Table.Td>
            </>
          ))}</Table.Tbody>
        </Table>
      </ScrollArea>
    );
  }

  const tableClassName = [
    tool === "dns" ? "dns-result-table" : undefined,
    tool === "http" ? "http-result-table" : undefined,
    ipWidthClass,
    compact ? "compact-result-table" : undefined
  ].filter(Boolean).join(" ");
  return (
    <ScrollArea>
      <Table
        className={tableClassName}
        striped
        highlightOnHover
        verticalSpacing="xs"
        miw={tableMinWidth(tool, hasIPv6, showHTTPDownloadMetrics, compact)}
      >
        <Table.Thead>
          <Table.Tr>
            <Table.Th className="region-column">{t("results.region")}</Table.Th>
            <Table.Th className="provider-column">{t("results.provider")}</Table.Th>
            {tool === "dns" && <Table.Th className="ip-column">{t("results.records")}</Table.Th>}
            {tool === "port" && <Table.Th className="ip-column">{t("results.ip")}</Table.Th>}
            {tool === "port" && <Table.Th className="time-column">{t("results.rtt")}</Table.Th>}
            {tool === "port" && <Table.Th className="status-column">{t("results.reachable")}</Table.Th>}
            {tool === "http" && <Table.Th className="ip-column">{t("results.ip")}</Table.Th>}
            {tool === "http" && <Table.Th className="time-column">{t("results.dnsTime")}</Table.Th>}
            {tool === "http" && <Table.Th className="time-column">{t("results.connect")}</Table.Th>}
            {tool === "http" && <Table.Th className="time-column">{t("results.tls")}</Table.Th>}
            {tool === "http" && <Table.Th className="time-column">{t("results.firstByte")}</Table.Th>}
            {tool === "http" && showHTTPDownloadMetrics && <Table.Th className="time-column">{t("results.download")}</Table.Th>}
            {tool === "http" && showHTTPDownloadMetrics && <Table.Th className="time-column">{t("results.speed")}</Table.Th>}
            {tool === "http" && <Table.Th className="time-column">{t("results.total")}</Table.Th>}
            {tool === "http" && <Table.Th className="status-column">{t("results.status")}</Table.Th>}
          </Table.Tr>
        </Table.Thead>
        <Table.Tbody>
          {nodeRows(rows, tool === "dns" ? 3 : tool === "port" ? 5 : showHTTPDownloadMetrics ? 11 : 9, t("results.noPingRows"), (row) => (
            <>
              <Table.Td className="region-column"><RegionCell country={row.country} region={row.region} protocols={row.protocols} t={t} /></Table.Td>
              <Table.Td className="provider-column"><ProviderCell provider={row.provider} isp={row.isp} protocols={row.protocols} t={t} /></Table.Td>
              {tool === "dns" && <Table.Td className="ip-column"><DNSRecordsCell records={row.records} geoIPByIP={geoIPByIP} onTraceIP={(ip) => onTraceIP(ip, row.agentId)} /></Table.Td>}
              {tool === "port" && <Table.Td className="ip-column"><GeoIPValue value={row.ip} geoIPByIP={geoIPByIP} onTraceIP={(ip) => onTraceIP(ip, row.agentId)} /></Table.Td>}
              {tool === "port" && <Table.Td className="time-column">{formatMS(row.connectMS)}</Table.Td>}
              {tool === "port" && <Table.Td className="status-column"><StatusBadge status={row.status} t={t} /></Table.Td>}
              {tool === "http" && <Table.Td className="ip-column"><GeoIPValue value={row.ip} geoIPByIP={geoIPByIP} onTraceIP={(ip) => onTraceIP(ip, row.agentId)} /></Table.Td>}
              {tool === "http" && <Table.Td className="time-column">{formatMS(row.dnsMS)}</Table.Td>}
              {tool === "http" && <Table.Td className="time-column">{formatMS(row.connectMS)}</Table.Td>}
              {tool === "http" && <Table.Td className="time-column">{formatMS(row.tlsMS)}</Table.Td>}
              {tool === "http" && <Table.Td className="time-column">{formatMS(row.firstByteMS)}</Table.Td>}
              {tool === "http" && showHTTPDownloadMetrics && <Table.Td className="time-column">{formatMS(row.downloadMS)}</Table.Td>}
              {tool === "http" && showHTTPDownloadMetrics && <Table.Td className="time-column">{formatSpeed(row.downloadSpeed)}</Table.Td>}
              {tool === "http" && <Table.Td className="time-column">{formatMS(row.totalMS)}</Table.Td>}
              {tool === "http" && <Table.Td className="status-column"><StatusBadge status={row.status} t={t} /></Table.Td>}
            </>
          ))}
        </Table.Tbody>
      </Table>
    </ScrollArea>
  );
}

export function MtrResultTable({
  tool,
  agent,
  targetIP,
  rows,
  compact,
  geoIPByIP,
  t
}: {
  tool: Tool;
  agent?: Agent;
  targetIP: string;
  rows: ReturnType<typeof buildMtrRows>;
  compact: boolean;
  geoIPByIP: GeoIPLookup;
  t: (key: string) => string;
}) {
  const hasIPv6 = rowsContainIPv6(rows) || isIPv6Value(targetIP);
  const ipWidthClass = hasIPv6 ? "has-ipv6" : "has-ipv4";
  const isTraceroute = tool === "traceroute";
  const colSpan = isTraceroute ? 3 : 10;
  const minWidth = routeTableMinWidth(isTraceroute, hasIPv6, compact);
  return (
    <>
      <div className="mtr-summary">
        <div className="mtr-summary-item">
          <Text className="mtr-summary-label">{t("results.targetIp")}</Text>
          <div className="mtr-summary-value">
            <GeoIPValue value={targetIP} geoIPByIP={geoIPByIP} />
          </div>
        </div>
        <div className="mtr-summary-item">
          <Text className="mtr-summary-label">{t("results.testNode")}</Text>
          <div className="mtr-agent-value">
            {agent ? (
              <>
                <RegionCell country={agent.country} region={agent.region || "-"} protocols={agent.protocols} t={t} />
                <ProviderCell provider={agent.provider} isp={agent.isp} fallback={agent.name || agent.id} protocols={agent.protocols} t={t} />
              </>
            ) : (
              <span>-</span>
            )}
          </div>
        </div>
      </div>
      <ScrollArea>
        <Table className={tableClass(`route-result-table ${isTraceroute ? "traceroute-result-table" : "mtr-result-table"} ${ipWidthClass}`, compact)} striped highlightOnHover verticalSpacing="xs" miw={minWidth}>
          <Table.Thead>
            <Table.Tr>
              <Table.Th className="hop-column">{t("results.hop")}</Table.Th>
              <Table.Th className="ip-column">{t("results.ip")}</Table.Th>
              {!isTraceroute && <Table.Th className="metric-column">{t("results.loss")}</Table.Th>}
              {!isTraceroute && <Table.Th className="sent-column">{t("results.sent")}</Table.Th>}
              <Table.Th className="time-column">{isTraceroute ? t("results.delay") : t("results.last")}</Table.Th>
              {!isTraceroute && <Table.Th className="time-column">{t("results.avg")}</Table.Th>}
              {!isTraceroute && <Table.Th className="time-column">{t("results.best")}</Table.Th>}
              {!isTraceroute && <Table.Th className="time-column">{t("results.worst")}</Table.Th>}
              {!isTraceroute && <Table.Th className="time-column">{t("results.stdev")}</Table.Th>}
              {!isTraceroute && <Table.Th className="chart-column">{t("results.chart")}</Table.Th>}
            </Table.Tr>
          </Table.Thead>
          <Table.Tbody>
            {rows.length === 0 ? (
              <Table.Tr>
                <Table.Td colSpan={colSpan}>
                  <Text c="dimmed" ta="center" py="xl">
                    {t("results.empty")}
                  </Text>
                </Table.Td>
              </Table.Tr>
            ) : (
              rows.map((row) => (
                <Table.Tr key={row.hop}>
                  <Table.Td className="mono-label hop-column">{row.hop}</Table.Td>
                  <Table.Td className="ip-column"><GeoIPValue value={row.ip} fallback="*" geoIPByIP={geoIPByIP} /></Table.Td>
                  {!isTraceroute && <Table.Td className="metric-column">{formatPercent(row.lossPct)}</Table.Td>}
                  {!isTraceroute && <Table.Td className="sent-column">{row.sent ?? "-"}</Table.Td>}
                  <Table.Td className="time-column">{formatMS(isTraceroute ? routeRTT(row) : row.lastMS)}</Table.Td>
                  {!isTraceroute && <Table.Td className="time-column">{formatMS(row.avgMS)}</Table.Td>}
                  {!isTraceroute && <Table.Td className="time-column">{formatMS(row.bestMS)}</Table.Td>}
                  {!isTraceroute && <Table.Td className="time-column">{formatMS(row.worstMS)}</Table.Td>}
                  {!isTraceroute && <Table.Td className="time-column">{formatMS(row.stdevMS)}</Table.Td>}
                  {!isTraceroute && <Table.Td className="chart-column"><PingRTTChart samples={row.rttSamples} lossPct={row.lossPct} /></Table.Td>}
                </Table.Tr>
              ))
            )}
          </Table.Tbody>
        </Table>
      </ScrollArea>
    </>
  );
}

function tableClass(className: string, compact: boolean): string {
  return compact ? `${className} compact-result-table` : className;
}

export function collectGeoIPTargets(
  isFanout: boolean,
  rows: ReturnType<typeof buildNodeRows>,
  mtrRows: ReturnType<typeof buildMtrRows>,
  targetIP: string
): string[] {
  if (!isFanout) {
    return uniqueIPAddresses([targetIP, ...mtrRows.map((row) => row.ip)]);
  }
  const values = rows.flatMap((row) => [
    row.ip,
    ...(row.records ?? []).map((record) => ipFromDNSRecord(record))
  ]);
  return uniqueIPAddresses(values);
}

function nodeRows(
  rows: ReturnType<typeof buildNodeRows>,
  colSpan: number,
  emptyText: string,
  render: (row: ReturnType<typeof buildNodeRows>[number]) => React.ReactNode
) {
  if (rows.length === 0) {
    return (
      <Table.Tr>
        <Table.Td colSpan={colSpan}>
          <Text c="dimmed" ta="center" py="xl">
            {emptyText}
          </Text>
        </Table.Td>
      </Table.Tr>
    );
  }
  return rows.map((row) => <Table.Tr key={row.agentId}>{render(row)}</Table.Tr>);
}

function PingRTTChart({ samples, lossPct }: { samples?: Array<number | null>; lossPct?: number }) {
  const width = 76;
  const height = 34;
  const isPlaceholder = !samples?.length;
  const visibleSamples = (isPlaceholder ? [null] : samples).slice(-18);
  const values = visibleSamples.flatMap((sample) => (typeof sample === "number" ? [sample] : []));
  const hasValues = values.length > 0;
  if (!hasValues && isPlaceholder) {
    return (
      <svg className="ping-rtt-chart ping-rtt-chart-empty" role="img" viewBox={`0 0 ${width} ${height}`}>
        <rect x="26" y="3" width="28" height="27" rx="2" />
      </svg>
    );
  }
  const max = hasValues ? Math.max(...values, 1) : 1;
  const gap = 2;
  const barWidth = Math.max(2, Math.floor((width - gap * (visibleSamples.length - 1)) / visibleSamples.length) - 1);
  const chartWidth = visibleSamples.length * barWidth + (visibleSamples.length - 1) * gap;
  const startX = Math.max(0, Math.floor((width - chartWidth) / 2));
  const failed = lossPct !== undefined && lossPct >= 100;
  return (
    <svg className={`ping-rtt-chart ${failed ? "ping-rtt-chart-failed" : ""}`} role="img" viewBox={`0 0 ${width} ${height}`}>
      {visibleSamples.map((sample, index) => {
        const x = startX + index * (barWidth + gap);
        if (typeof sample !== "number") {
          return <rect className="ping-rtt-timeout" key={index} x={x} y="3" width={barWidth} height="27" rx="1" />;
        }
        const normalized = sample / max;
        const barHeight = Math.max(7, Math.round(9 + normalized * 18));
        return <rect key={index} x={x} y={height - barHeight - 4} width={barWidth} height={barHeight} rx="1" />;
      })}
    </svg>
  );
}

export function DNSRecordsCell({
  records,
  geoIPByIP,
  onTraceIP
}: {
  records?: string[];
  geoIPByIP: GeoIPLookup;
  onTraceIP?: (ip: string) => void;
}) {
  if (!records?.length) {
    return <span>-</span>;
  }
  return (
    <div className="dns-records-cell">
      {records.map((record, index) => {
        const { type, value } = splitDNSRecord(record);
        const ip = normalizeIPAddress(value);
        if (!ip) {
          return (
            <div className="dns-record-line" key={`${record}-${index}`}>
              {record}
            </div>
          );
        }
        return <GeoIPValue key={`${record}-${index}`} value={ip} prefix={type} geoIPByIP={geoIPByIP} onTraceIP={onTraceIP} />;
      })}
    </div>
  );
}

function GeoIPValue({
  value,
  prefix,
  fallback = "-",
  geoIPByIP,
  onTraceIP
}: {
  value?: string;
  prefix?: string;
  fallback?: string;
  geoIPByIP: GeoIPLookup;
  onTraceIP?: (ip: string) => void;
}) {
  const ip = normalizeIPAddress(value);
  const displayValue = ip || value?.trim() || fallback;
  if (!ip) {
    return <span>{displayValue}</span>;
  }
  const info = geoIPByIP[ip];
  const asn = formatASN(info);
  const location = formatLocation(info);
  return (
    <span className="geoip-cell">
      <span className="geoip-primary" title={asn ? `${ip} (${asn})` : ip}>
        {onTraceIP ? (
          <button className="geoip-ip-button" type="button" onClick={() => onTraceIP(ip)}>
            {prefix ? `${prefix} ${ip}` : ip}
          </button>
        ) : (
          <span>{prefix ? `${prefix} ${ip}` : ip}</span>
        )}
        {asn && (
          <span className="geoip-asn">
            {" ("}
            {info?.asn ? (
              <a className="geoip-asn-link" href={`https://bgp.tools/as/${info.asn}`} rel="noreferrer" target="_blank">
                {asn}
              </a>
            ) : (
              asn
            )}
            {")"}
          </span>
        )}
      </span>
      {info?.reverse && <span className="geoip-detail" title={info.reverse}>{info.reverse}</span>}
      {location && <span className="geoip-detail" title={location}>{location}</span>}
    </span>
  );
}

function pingTableMinWidth(hasIPv6: boolean, compact: boolean): number {
  if (compact) {
    return hasIPv6 ? 1110 : 900;
  }
  return hasIPv6 ? 1490 : 1270;
}

function routeTableMinWidth(isTraceroute: boolean, hasIPv6: boolean, compact: boolean): string | number {
  if (isTraceroute) {
    return compact ? (hasIPv6 ? 520 : 390) : (hasIPv6 ? 700 : 540);
  }
  if (!hasIPv6 && !compact) {
    return "100%";
  }
  return compact ? (hasIPv6 ? 820 : 650) : (hasIPv6 ? 1130 : 950);
}

function tableMinWidth(tool: Tool, hasIPv6: boolean, showHTTPDownloadMetrics = false, compact = false): number {
  if (tool === "http") {
    if (showHTTPDownloadMetrics) {
      return compact ? (hasIPv6 ? 1160 : 960) : (hasIPv6 ? 1540 : 1320);
    }
    return compact ? (hasIPv6 ? 980 : 800) : (hasIPv6 ? 1380 : 1180);
  }
  if (tool === "dns") {
    return compact ? (hasIPv6 ? 760 : 560) : (hasIPv6 ? 1120 : 880);
  }
  return compact ? (hasIPv6 ? 760 : 570) : (hasIPv6 ? 1040 : 820);
}

function rowsContainIPv6(rows: Array<{ ip?: string; records?: string[] }>): boolean {
  return rows.some((row) => isIPv6Value(row.ip) || row.records?.some((record) => isIPv6Value(ipFromDNSRecord(record))));
}

function isIPv6Value(value: string | undefined): boolean {
  return normalizeIPAddress(value)?.includes(":") ?? false;
}
