import { describe, expect, it } from "vitest";
import { applyStoredApiBaseUrl, applyStoredApiToken, apiBaseUrlStorageKey, apiTokenStorageKey, brandUrlTargetsCurrentApp, loadConfig, normalizeBaseUrl, normalizeBrandUrl, saveStoredApiBaseUrl, saveStoredApiToken } from "./config";

describe("runtime config", () => {
  it("normalizes configured API base URL", () => {
    expect(normalizeBaseUrl(" https://api.example.com/// ")).toBe("https://api.example.com");
  });

  it("loads config with safe defaults", async () => {
    const fetcher = async () =>
      new Response(JSON.stringify({ apiBaseUrl: "http://localhost:8080/", apiToken: "token", brand: "  Edge MTR  ", brandUrl: " https://mtr.example.com/ " }));

    await expect(loadConfig(fetcher as typeof fetch)).resolves.toEqual({
      apiBaseUrl: "http://localhost:8080",
      apiToken: "token",
      brand: "Edge MTR",
      brandUrl: "https://mtr.example.com/"
    });
  });

  it("prefers stored API overrides over config.json", async () => {
    const fetcher = async () =>
      new Response(JSON.stringify({ apiBaseUrl: "http://localhost:8080/", apiToken: "file-token" }));
    const storage = {
      getItem: (key: string) => {
        if (key === apiTokenStorageKey) {
          return "stored-token";
        }
        if (key === apiBaseUrlStorageKey) {
          return "https://stored.example.com///";
        }
        return null;
      }
    };

    await expect(loadConfig(fetcher as typeof fetch, storage)).resolves.toEqual({
      apiBaseUrl: "https://stored.example.com",
      apiToken: "stored-token"
    });
  });

  it("stores trimmed API tokens", () => {
    const calls: Array<[string, string]> = [];
    const storage = {
      setItem: (key: string, value: string) => calls.push([key, value])
    };

    saveStoredApiToken("  next-token  ", storage);

    expect(calls).toEqual([[apiTokenStorageKey, "next-token"]]);
  });

  it("stores normalized API base URLs", () => {
    const calls: Array<[string, string]> = [];
    const storage = {
      setItem: (key: string, value: string) => calls.push([key, value])
    };

    saveStoredApiBaseUrl(" https://api.example.com/// ", storage);

    expect(calls).toEqual([[apiBaseUrlStorageKey, "https://api.example.com"]]);
  });

  it("keeps config token when no stored token exists", () => {
    const storage = {
      getItem: () => null
    };

    expect(applyStoredApiToken({ apiBaseUrl: "/api", apiToken: "file-token" }, storage)).toEqual({
      apiBaseUrl: "/api",
      apiToken: "file-token"
    });
  });

  it("keeps config API base URL when no stored override exists", () => {
    const storage = {
      getItem: () => null
    };

    expect(applyStoredApiBaseUrl({ apiBaseUrl: "/api", apiToken: "file-token" }, storage)).toEqual({
      apiBaseUrl: "/api",
      apiToken: "file-token"
    });
  });

  it("allows disabling the brand link", async () => {
    const fetcher = async () => new Response(JSON.stringify({ apiBaseUrl: "", apiToken: "token", brandUrl: "" }));

    await expect(loadConfig(fetcher as typeof fetch)).resolves.toEqual({
      apiBaseUrl: "",
      apiToken: "token",
      brandUrl: null
    });
  });

  it("normalizes configured brand URLs", () => {
    expect(normalizeBrandUrl(undefined)).toBeUndefined();
    expect(normalizeBrandUrl(null)).toBeNull();
    expect(normalizeBrandUrl(false)).toBeNull();
    expect(normalizeBrandUrl("  ")).toBeNull();
    expect(normalizeBrandUrl(" https://mtr.example.com/ ")).toBe("https://mtr.example.com/");
  });

  it("detects brand URLs that point at the current app", () => {
    expect(brandUrlTargetsCurrentApp("https://mtr.example.com", "https://mtr.example.com/schedules?tab=1")).toBe(true);
    expect(brandUrlTargetsCurrentApp("https://mtr.example.com/schedules", "https://mtr.example.com/schedules?tab=1")).toBe(true);
    expect(brandUrlTargetsCurrentApp("https://other.example.com", "https://mtr.example.com/schedules")).toBe(false);
    expect(brandUrlTargetsCurrentApp("https://mtr.example.com/other", "https://mtr.example.com/schedules")).toBe(false);
  });

  it("falls back when config cannot be fetched", async () => {
    const fetcher = async () => new Response("", { status: 404 });

    await expect(loadConfig(fetcher as typeof fetch)).resolves.toEqual({
      apiBaseUrl: "",
      apiToken: ""
    });
  });
});
