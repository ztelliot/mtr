import { describe, expect, it } from "vitest";
import { flagCountryCode } from "./agentDisplay";

describe("agent display helpers", () => {
  it("uses HK, MO, and TW flag assets directly", () => {
    expect(flagCountryCode("HK")).toBe("HK");
    expect(flagCountryCode("MO")).toBe("MO");
    expect(flagCountryCode("TW")).toBe("TW");
  });
});
