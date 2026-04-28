import { describe, expect, it } from "vitest";
import i18n, { resources } from "./i18n";

describe("i18n", () => {
  it("ships Chinese and English resources", () => {
    expect(resources["zh-CN"].translation.form.target).toBe("目标");
    expect(resources["en-US"].translation.form.target).toBe("Target");
    expect(resources["zh-CN"].translation.form.remoteDns).toBe("远程 DNS");
    expect(resources["en-US"].translation.form.remoteDns).toBe("Remote DNS");
    expect(resources["zh-CN"].translation.errors.targetBlocked).toBe("所选节点不支持该目标或 IP 版本");
    expect(resources["en-US"].translation.errors.targetBlocked).toBe("The selected agent does not support this target or IP version");
    expect(resources["zh-CN"].translation.results.parameters).toBe("参数");
    expect(resources["en-US"].translation.results.parameters).toBe("Parameters");
    expect(resources["zh-CN"].translation.apiErrors.noOnlineAgentsSupportJob).toBe("没有在线节点支持该任务");
    expect(resources["en-US"].translation.apiErrors.noOnlineAgentsSupportJob).toBe("No online agents support this job");
    expect(resources["zh-CN"].translation.jobErrorTypes.unsupported_protocol).toBe("所选节点不支持请求的协议或 IP 版本");
    expect(resources["zh-CN"].translation.jobErrorTypes.job_timeout).toBe("任务超时");
    expect(resources["en-US"].translation.jobErrorTypes.unsupported_tool).toBe("The selected agent does not support this tool");
    expect(resources["zh-CN"].translation.nav.schedules).toBe("Watch");
    expect(resources["en-US"].translation.schedule.create).toBe("Add task");
  });

  it("falls back to English", () => {
    expect(i18n.t("results.empty", { lng: "fr-FR" })).toBe("Run a diagnostic to see results here.");
  });
});
