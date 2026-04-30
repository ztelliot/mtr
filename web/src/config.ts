import type { RuntimeConfig } from "./types";

export const apiTokenStorageKey = "mtr.api-token";
export const apiBaseUrlStorageKey = "mtr.api-base-url";

const fallbackConfig: RuntimeConfig = {
  apiBaseUrl: "",
  apiToken: ""
};

type RuntimeConfigFile = Partial<Omit<RuntimeConfig, "brand" | "brandUrl">> & {
  brand?: unknown;
  brandUrl?: unknown;
};

export async function loadConfig(fetcher: typeof fetch = fetch, storage: Pick<Storage, "getItem"> | null = browserStorage()): Promise<RuntimeConfig> {
  try {
    const response = await fetcher("/config.json", { cache: "no-store" });
    if (!response.ok) {
      return applyStoredRuntimeOverrides(fallbackConfig, storage);
    }
    const raw = (await response.json()) as RuntimeConfigFile;
    const next: RuntimeConfig = {
      apiBaseUrl: normalizeBaseUrl(raw.apiBaseUrl ?? ""),
      apiToken: raw.apiToken ?? ""
    };
    const brand = normalizeBrand(raw.brand);
    if (brand) {
      next.brand = brand;
    }
    const brandUrl = normalizeBrandUrl(raw.brandUrl);
    if (brandUrl !== undefined) {
      next.brandUrl = brandUrl;
    }
    return applyStoredRuntimeOverrides(next, storage);
  } catch {
    return applyStoredRuntimeOverrides(fallbackConfig, storage);
  }
}

export function normalizeBaseUrl(value: string): string {
  return value.trim().replace(/\/+$/, "");
}

export function normalizeBrand(value: unknown): string | undefined {
  const brand = typeof value === "string" ? value.trim() : "";
  return brand ? brand : undefined;
}

export function normalizeBrandUrl(value: unknown): string | null | undefined {
  if (value === undefined) {
    return undefined;
  }
  if (value === null || value === false) {
    return null;
  }
  if (typeof value !== "string") {
    return undefined;
  }
  const url = value.trim();
  return url ? url : null;
}

export function brandUrlTargetsCurrentApp(brandUrl: string, currentHref: string): boolean {
  try {
    const target = new URL(brandUrl, currentHref);
    const current = new URL(currentHref);
    if (target.origin !== current.origin) {
      return false;
    }
    const targetPath = normalizePathname(target.pathname);
    return targetPath === "/" || targetPath === normalizePathname(current.pathname);
  } catch {
    return false;
  }
}

export function applyStoredApiToken(config: RuntimeConfig, storage: Pick<Storage, "getItem"> | null = browserStorage()): RuntimeConfig {
  const token = readStoredApiToken(storage);
  return token === null ? config : { ...config, apiToken: token };
}

export function applyStoredRuntimeOverrides(config: RuntimeConfig, storage: Pick<Storage, "getItem"> | null = browserStorage()): RuntimeConfig {
  return applyStoredApiBaseUrl(applyStoredApiToken(config, storage), storage);
}

export function applyStoredApiBaseUrl(config: RuntimeConfig, storage: Pick<Storage, "getItem"> | null = browserStorage()): RuntimeConfig {
  const apiBaseUrl = readStoredApiBaseUrl(storage);
  return apiBaseUrl === null ? config : { ...config, apiBaseUrl };
}

export function readStoredApiToken(storage: Pick<Storage, "getItem"> | null = browserStorage()): string | null {
  try {
    return storage?.getItem(apiTokenStorageKey) ?? null;
  } catch {
    return null;
  }
}

export function readStoredApiBaseUrl(storage: Pick<Storage, "getItem"> | null = browserStorage()): string | null {
  try {
    const value = storage?.getItem(apiBaseUrlStorageKey) ?? null;
    return value === null ? null : normalizeBaseUrl(value);
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

export function saveStoredApiBaseUrl(apiBaseUrl: string, storage: Pick<Storage, "setItem"> | null = browserStorage()): void {
  try {
    storage?.setItem(apiBaseUrlStorageKey, normalizeBaseUrl(apiBaseUrl));
  } catch {
    // Ignore storage errors so the in-memory URL can still be used.
  }
}

function browserStorage(): Storage | null {
  return typeof window === "undefined" ? null : window.localStorage;
}

function normalizePathname(pathname: string): string {
  const path = pathname.replace(/\/+$/, "");
  return path || "/";
}
