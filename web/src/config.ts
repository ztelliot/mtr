import type { RuntimeConfig } from "./types";

export const apiTokenStorageKey = "mtr.api-token";

const fallbackConfig: RuntimeConfig = {
  apiBaseUrl: "",
  apiToken: ""
};

export async function loadConfig(fetcher: typeof fetch = fetch, storage: Pick<Storage, "getItem"> | null = browserStorage()): Promise<RuntimeConfig> {
  try {
    const response = await fetcher("/config.json", { cache: "no-store" });
    if (!response.ok) {
      return applyStoredApiToken(fallbackConfig, storage);
    }
    const raw = (await response.json()) as Partial<RuntimeConfig>;
    return applyStoredApiToken({
      apiBaseUrl: normalizeBaseUrl(raw.apiBaseUrl ?? ""),
      apiToken: raw.apiToken ?? ""
    }, storage);
  } catch {
    return applyStoredApiToken(fallbackConfig, storage);
  }
}

export function normalizeBaseUrl(value: string): string {
  return value.trim().replace(/\/+$/, "");
}

export function applyStoredApiToken(config: RuntimeConfig, storage: Pick<Storage, "getItem"> | null = browserStorage()): RuntimeConfig {
  const token = readStoredApiToken(storage);
  return token === null ? config : { ...config, apiToken: token };
}

export function readStoredApiToken(storage: Pick<Storage, "getItem"> | null = browserStorage()): string | null {
  try {
    return storage?.getItem(apiTokenStorageKey) ?? null;
  } catch {
    return null;
  }
}

export function saveStoredApiToken(token: string, storage: Pick<Storage, "setItem"> | null = browserStorage()): void {
  try {
    storage?.setItem(apiTokenStorageKey, token.trim());
  } catch {
    // Ignore storage errors so the in-memory token can still be used.
  }
}

function browserStorage(): Storage | null {
  return typeof window === "undefined" ? null : window.localStorage;
}
