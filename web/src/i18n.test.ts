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
    expect(resources["zh-CN"].translation.errors.requestFailed).toBe("请求失败");
    expect(resources["en-US"].translation.errors.requestFailed).toBe("Request failed");
    expect(resources["zh-CN"].translation.actions.add).toBe("添加");
    expect(resources["en-US"].translation.actions.add).toBe("Add");
    expect(resources["zh-CN"].translation.manage.resolveOnAgent).toBe("节点侧解析");
    expect(resources["en-US"].translation.manage.resolveOnAgent).toBe("Resolve on agent");
  });

  it("keeps Chinese and English resource keys aligned", () => {
    expect(flattenKeys(resources["zh-CN"].translation)).toEqual(flattenKeys(resources["en-US"].translation));
  });

  it("defines every static translation key used by the source", () => {
    const keys = new Set(flattenKeys(resources["en-US"].translation));
    expect(staticTranslationKeys().filter((key) => !keys.has(key))).toEqual([]);
  });

  it("falls back to English", () => {
    expect(i18n.t("results.empty", { lng: "fr-FR" })).toBe("Run a diagnostic to see results here.");
  });
});

function flattenKeys(value: object, prefix = ""): string[] {
  return Object.entries(value).flatMap(([key, item]) => {
    const nextPrefix = prefix ? `${prefix}.${key}` : key;
    return item && typeof item === "object" ? flattenKeys(item, nextPrefix) : [nextPrefix];
  }).sort();
}

function staticTranslationKeys(): string[] {
  const files = import.meta.glob("./*.{ts,tsx}", { eager: true, import: "default", query: "?raw" }) as Record<string, string>;
  const keys = new Set<string>();

  for (const source of Object.values(files)) {
    for (const match of source.matchAll(/\b(?:i18n\.)?t\(\s*["`]([A-Za-z0-9_.-]+)["`]/g)) {
      keys.add(match[1]);
    }
  }

  return [...keys].sort();
}
