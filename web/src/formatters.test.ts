import { describe, expect, it } from "vitest";
import { formatActualProtocol, formatHTTPVersion } from "./formatters";

describe("protocol formatters", () => {
  it("uses compact HTTP family labels", () => {
    expect(formatHTTPVersion("auto")).toBe("Auto");
    expect(formatHTTPVersion("HTTP/1.1")).toBe("H1");
    expect(formatHTTPVersion("2")).toBe("H2");
    expect(formatHTTPVersion("h3")).toBe("H3");
  });

  it("formats actual probe protocols by tool", () => {
    expect(formatActualProtocol("http", "1.1")).toBe("H1");
    expect(formatActualProtocol("ping", "icmp")).toBe("ICMP");
    expect(formatActualProtocol("traceroute", "udp")).toBe("UDP");
    expect(formatActualProtocol("mtr", "tcp")).toBe("TCP");
    expect(formatActualProtocol("http", undefined)).toBe("-");
  });
});
