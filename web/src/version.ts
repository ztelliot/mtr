import packageInfo from "../package.json";
import { formatVersionLabel } from "./versionLabel";

const injectedVersion = import.meta.env.VITE_APP_VERSION?.trim();
const injectedCommit = import.meta.env.VITE_APP_COMMIT?.trim();

export const appVersion = injectedVersion || packageInfo.version;
export const appCommit = injectedCommit || "";
export const appVersionLabel = formatVersionLabel(appVersion, appCommit);
