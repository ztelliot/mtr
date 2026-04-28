import { describe, expect, it } from "vitest";
import { formatASN, formatLocation, ipFromDNSRecord, normalizeIPAddress, splitDNSRecord, uniqueIPAddresses } from "./geoip";

describe("geoip helpers", () => {
  it("recognizes IPv4 and IPv6 values", () => {
    expect(normalizeIPAddress("1.0.0.1")).toBe("1.0.0.1");
    expect(normalizeIPAddress("1.0.0.1:443")).toBe("1.0.0.1");
    expect(normalizeIPAddress("2606:4700:4700::1111")).toBe("2606:4700:4700::1111");
    expect(normalizeIPAddress("[2606:4700:4700::1111]")).toBe("2606:4700:4700::1111");
    expect(normalizeIPAddress("[2606:4700:4700::1111]:443")).toBe("2606:4700:4700::1111");
    expect(normalizeIPAddress("::1")).toBe("::1");
    expect(normalizeIPAddress("2001:4860:4860:0:0:0:0:8888")).toBe("2001:4860:4860:0:0:0:0:8888");
    expect(normalizeIPAddress("example.com")).toBeUndefined();
    expect(normalizeIPAddress("1:2:3:4:5:6:7:8:9")).toBeUndefined();
    expect(normalizeIPAddress("*")).toBeUndefined();
  });

  it("extracts IP values from DNS records", () => {
    expect(splitDNSRecord("A 1.0.0.1")).toEqual({ type: "A", value: "1.0.0.1" });
    expect(ipFromDNSRecord("AAAA 2606:4700:4700::1111")).toBe("2606:4700:4700::1111");
    expect(ipFromDNSRecord("CNAME one.one.one.one.")).toBeUndefined();
  });

  it("deduplicates normalized IPs", () => {
    expect(uniqueIPAddresses(["1.0.0.1", "1.0.0.1", "example.com", "[2606:4700:4700::1111]"])).toEqual([
      "1.0.0.1",
      "2606:4700:4700::1111"
    ]);
  });

  it("formats ASN and location lines", () => {
    const info = {
      asn: 13335,
      org: "Cloudflare, Inc.",
      reverse: "one.one.one.one.",
      city: "Brisbane",
      region: "Queensland",
      country: "Australia"
    };
    expect(formatASN(info)).toBe("AS13335 Cloudflare, Inc.");
    expect(formatLocation(info)).toBe("Brisbane, Queensland, Australia");
  });
});
