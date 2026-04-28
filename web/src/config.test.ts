import { describe, expect, it } from "vitest";
import { applyStoredApiToken, apiTokenStorageKey, loadConfig, normalizeBaseUrl, saveStoredApiToken } from "./config";

describe("runtime config", () => {
  it("normalizes configured API base URL", () => {
    expect(normalizeBaseUrl(" https://api.example.com/// ")).toBe("https://api.example.com");
  });

  it("loads config with safe defaults", async () => {
    const fetcher = async () =>
      new Response(JSON.stringify({ apiBaseUrl: "http://localhost:8080/", apiToken: "token" }));

    await expect(loadConfig(fetcher as typeof fetch)).resolves.toEqual({
      apiBaseUrl: "http://localhost:8080",
      apiToken: "token"
    });
  });

  it("prefers the stored API token over config.json", async () => {
    const fetcher = async () =>
      new Response(JSON.stringify({ apiBaseUrl: "http://localhost:8080/", apiToken: "file-token" }));
    const storage = {
      getItem: (key: string) => (key === apiTokenStorageKey ? "stored-token" : null)
    };

    await expect(loadConfig(fetcher as typeof fetch, storage)).resolves.toEqual({
      apiBaseUrl: "http://localhost:8080",
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

  it("keeps config token when no stored token exists", () => {
    const storage = {
      getItem: () => null
    };

    expect(applyStoredApiToken({ apiBaseUrl: "/api", apiToken: "file-token" }, storage)).toEqual({
      apiBaseUrl: "/api",
      apiToken: "file-token"
    });
  });

  it("falls back when config cannot be fetched", async () => {
    const fetcher = async () => new Response("", { status: 404 });

    await expect(loadConfig(fetcher as typeof fetch)).resolves.toEqual({
      apiBaseUrl: "",
      apiToken: ""
    });
  });
});
