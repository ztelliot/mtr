export function formatVersionLabel(version: string | undefined, commit?: string): string {
  const cleanVersion = version?.trim() ?? "";
  const cleanCommit = commit?.trim() ?? "";
  if (!cleanVersion) {
    return "";
  }
  if (isReleaseVersion(cleanVersion) || !cleanCommit) {
    return cleanVersion;
  }
  return shortCommit(cleanCommit);
}

function isReleaseVersion(version: string): boolean {
  return /^v\d/.test(version);
}

function shortCommit(commit: string): string {
  return commit.slice(0, 8);
}
