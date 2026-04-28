import { describe, expect, it } from "vitest";
import { ApiError } from "./api";
import { apiErrorMessage, errorMessage } from "./errors";

describe("localized errors", () => {
  it("translates known API errors", () => {
    expect(apiErrorMessage("no online agents support this job", "zh-CN")).toBe("没有在线节点支持该任务");
    expect(apiErrorMessage("no online agents support this job", "en-US")).toBe("No online agents support this job");
    expect(apiErrorMessage("agent not found", "en-US")).toBe("Agent not found");
    expect(apiErrorMessage("agent is offline", "zh-CN")).toBe("节点离线");
  });

  it("translates patterned API errors", () => {
    expect(apiErrorMessage('agent "edge-1" is not allowed', "zh-CN")).toBe("无权限使用节点 edge-1");
    expect(apiErrorMessage('ip_version 6 is not allowed for traceroute', "en-US")).toBe("traceroute does not allow IP version 6");
  });

  it("keeps unknown API errors inspectable", () => {
    expect(errorMessage(new ApiError(500, "database exploded"))).toBe("500: database exploded");
  });
});
