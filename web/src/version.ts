import packageInfo from "../package.json";

const injectedVersion = import.meta.env.VITE_APP_VERSION?.trim();

export const appVersion = injectedVersion || packageInfo.version;
