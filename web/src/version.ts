import packageInfo from "../package.json";

const injectedVersion = import.meta.env.VITE_APP_VERSION?.trim();
const injectedCommit = import.meta.env.VITE_APP_COMMIT?.trim();

function shortCommit(value?: string): string {
  return value ? value.slice(0, 8) : "";
}

export const appVersion = injectedVersion || packageInfo.version;
export const appCommit = shortCommit(injectedCommit);
export const appVersionLabel = appCommit ? `${appVersion} ${appCommit}` : appVersion;
