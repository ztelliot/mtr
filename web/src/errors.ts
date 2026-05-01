import { ApiError } from "./api";
import i18n from "./i18n";

type ErrorPattern = {
  pattern: RegExp;
  key: string;
  params?: (match: RegExpMatchArray) => Record<string, string>;
};

const apiErrorKeys: Record<string, string> = {
  "invalid api token": "apiErrors.invalidApiToken",
  "rate limit exceeded": "apiErrors.rateLimitExceeded",
  "tool rate limit exceeded": "apiErrors.toolRateLimitExceeded",
  "invalid json": "apiErrors.invalidJson",
  "no online agents support this job": "apiErrors.noOnlineAgentsSupportJob",
  "target resolved to no addresses": "apiErrors.targetResolvedToNoAddresses",
  "schedule write access is not allowed": "apiErrors.scheduleWriteAccessDenied",
  "schedule read access is not allowed": "apiErrors.scheduleReadAccessDenied",
  "interval_seconds must be between 10 and 86400": "apiErrors.scheduleIntervalRange",
  "streaming unsupported": "apiErrors.streamingUnsupported",
  "agent not found": "apiErrors.agentNotFound",
  "agent is offline": "apiErrors.agentOffline",
  "invalid ip": "apiErrors.invalidIp",
  "invalid geoip response": "apiErrors.invalidGeoipResponse",
  "not found": "apiErrors.notFound",
  "target is required": "apiErrors.targetRequired",
  "http target must be an http or https URL": "apiErrors.httpTargetRequired",
  "target contains forbidden characters": "apiErrors.targetForbiddenChars",
  "target must be an IP address or hostname": "apiErrors.targetHostRequired"
};

const apiErrorPatterns: ErrorPattern[] = [
  { pattern: /^tool "([^"]+)" is not enabled$/, key: "apiErrors.toolNotEnabled", params: ([, tool]) => ({ tool }) },
  { pattern: /^(.+) requires agent_id$/, key: "apiErrors.agentRequired", params: ([, tool]) => ({ tool }) },
  { pattern: /^(.+) requires port$/, key: "apiErrors.portRequired", params: ([, tool]) => ({ tool }) },
  { pattern: /^argument "([^"]+)" is not allowed for (.+)$/, key: "apiErrors.argumentNotAllowed", params: ([, argument, tool]) => ({ argument, tool }) },
  { pattern: /^argument "([^"]+)" has invalid permission pattern$/, key: "apiErrors.argumentInvalidPermissionPattern", params: ([, argument]) => ({ argument }) },
  { pattern: /^argument "([^"]+)" is invalid$/, key: "apiErrors.argumentInvalid", params: ([, argument]) => ({ argument }) },
  { pattern: /^tool "([^"]+)" is not allowed$/, key: "apiErrors.toolNotAllowed", params: ([, tool]) => ({ tool }) },
  { pattern: /^ip_version (\d+) is not allowed for (.+)$/, key: "apiErrors.ipVersionNotAllowed", params: ([, version, tool]) => ({ version, tool }) },
  { pattern: /^resolve_on_agent=(true|false) is not allowed for (.+)$/, key: "apiErrors.resolveOnAgentNotAllowed", params: ([, value, tool]) => ({ value, tool }) },
  { pattern: /^agent "([^"]+)" is not allowed$/, key: "apiErrors.agentNotAllowed", params: ([, agent]) => ({ agent }) },
  { pattern: /^schedule target "([^"]+)" has no allowed online agents for (.+)$/, key: "apiErrors.noAvailableScheduleNodes", params: ([, label, tool]) => ({ label, tool }) },
  { pattern: /^invalid resolved ip "([^"]+)"$/, key: "apiErrors.invalidResolvedIp", params: ([, ip]) => ({ ip }) },
  { pattern: /^resolve target: (.+)$/, key: "apiErrors.resolveTargetFailed", params: ([, error]) => ({ error }) },
  { pattern: /^target address (.+) is not allowed$/, key: "apiErrors.targetAddressNotAllowed", params: ([, address]) => ({ address }) },
  { pattern: /^target address (.+) is not IPv4$/, key: "apiErrors.targetAddressNotIPv4", params: ([, address]) => ({ address }) },
  { pattern: /^target address (.+) is not IPv6$/, key: "apiErrors.targetAddressNotIPv6", params: ([, address]) => ({ address }) },
  { pattern: /^geoip upstream returned HTTP (\d+)$/, key: "apiErrors.geoipUpstreamHTTP", params: ([, status]) => ({ status }) }
];

export function errorMessage(err: unknown): string {
  if (err instanceof ApiError) {
    return apiErrorMessage(err.message) ?? `${err.status}: ${err.message}`;
  }
  if (err instanceof Error) {
    return err.message;
  }
  return translate("apiErrors.unexpected");
}

export function apiErrorMessage(message: string, language?: string): string | null {
  const exactKey = apiErrorKeys[message];
  if (exactKey) {
    return translate(exactKey, undefined, language);
  }

  for (const item of apiErrorPatterns) {
    const match = message.match(item.pattern);
    if (match) {
      return translate(item.key, item.params?.(match), language);
    }
  }

  return null;
}

function translate(key: string, options?: Record<string, string>, language?: string): string {
  const result = i18n.t(key, language ? { ...options, lng: language } : options);
  return typeof result === "string" ? result : String(result);
}
