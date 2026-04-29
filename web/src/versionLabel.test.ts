import { describe, expect, it } from "vitest";
import { formatVersionLabel } from "./versionLabel";

describe("formatVersionLabel", () => {
  it("shows release versions without the commit", () => {
    expect(formatVersionLabel("v1.2.3", "abcdef123456")).toBe("v1.2.3");
  });

  it("shows the commit for moving channels", () => {
    expect(formatVersionLabel("main", "abcdef123456")).toBe("abcdef12");
    expect(formatVersionLabel("dev", "1234567890ab")).toBe("12345678");
  });

  it("falls back to the version when no commit is available", () => {
    expect(formatVersionLabel("main")).toBe("main");
  });
});
