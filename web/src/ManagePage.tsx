import {
  ActionIcon,
  Alert,
  Badge,
  Box,
  Button,
  Group,
  Modal,
  MultiSelect,
  NumberInput,
  Paper,
  PasswordInput,
  Select,
  SimpleGrid,
  Stack,
  Switch,
  Table,
  Tabs,
  TagsInput,
  Text,
  Textarea,
  TextInput,
  Tooltip
} from "@mantine/core";
import { notifications } from "@mantine/notifications";
import { AlertTriangle, Pencil, Plus, RefreshCw, RotateCcw, Save, Trash2, X } from "lucide-react";
import { FormEvent, useEffect, useMemo, useState } from "react";
import type React from "react";
import { ApiClient } from "./api";
import { errorMessage } from "./errors";
import { formatDateTime, formatHTTPVersion } from "./formatters";
import { canReadManage, canWriteManage } from "./permissions";
import type { AgentConfig, APITokenPermission, IPVersion, LabelConfigSettings, ManagedAgent, ManagedAgentLabelUpdate, ManagedSettings, OutboundAgentAuth, OutboundAgentConfig, Permissions, PolicySettings, RateLimitConfig, RegisterToken, RuntimeSettings, SchedulerSettings, TokenToolScope, Tool } from "./types";

const tools: Tool[] = ["ping", "traceroute", "mtr", "http", "dns", "port"];
const pingProtocolOptions = ["icmp", "udp", "tcp"];
const routeProtocolOptions = ["icmp", "udp", "tcp"];
const methodOptions = ["GET", "HEAD"];
const httpVersionOptions = ["3", "2", "1.1"];
const dnsTypeOptions = ["A", "AAAA", "CNAME", "MX", "TXT", "NS"];
const ipVersionOptions: IPVersion[] = [0, 4, 6];
const defaultPortRange = "1-65535";

const emptyRuntime: RuntimeSettings = {
  count: 10,
  max_hops: 30,
  probe_step_timeout_sec: 1,
  max_tool_timeout_sec: 300,
  http_timeout_sec: 5,
  dns_timeout_sec: 5,
  resolve_timeout_sec: 3,
  outbound_invoke_attempts: 3,
  outbound_max_health_interval_sec: 300
};

const emptyScheduler: SchedulerSettings = {
  agent_offline_after_sec: 90,
  max_inflight_per_agent: 4,
  poll_interval_sec: 2
};

const emptyRateLimit: RateLimitConfig = {
  global: { requests_per_minute: 600, burst: 200 },
  ip: { requests_per_minute: 60, burst: 20 },
  cidr: { requests_per_minute: 300, burst: 100, ipv4_prefix: 32, ipv6_prefix: 128 },
  geoip: { requests_per_minute: 120, burst: 60 },
  tools: {},
  exempt_cidrs: []
};

const emptySettings: ManagedSettings = {
  rate_limit: emptyRateLimit,
  label_configs: {},
  api_tokens: [],
  register_tokens: []
};

const emptyOutboundAgent: OutboundAgentConfig = {
  id: "",
  transport: "http",
  enabled: true,
  url: "",
  auth: { type: "bearer", secret: "" },
  headers: {},
  event_response: "direct",
  labels: [],
  tls: { enabled: false, ca_files: [], cert_file: "", key_file: "", server_name: "" },
};

const emptyAPIToken: APITokenPermission = {
  name: "",
  secret: "",
  all: false,
  schedule_access: "none",
  manage_access: "none",
  agents: ["*"],
  denied_agents: [],
  agent_tags: [],
  denied_tags: [],
  tools: {}
};

const emptyAgentConfig: AgentConfig = {
  id: "",
  transport: "grpc",
  disabled: false,
  labels: []
};

const emptyLabelConfig: LabelConfigSettings = {
  runtime: null,
  scheduler: null,
  tool_policies: {}
};

export const __managePageTest = {
  outboundAgentAuthForSave,
  outboundCredentialScopeEqual,
  changedLabelAgents,
  customLabels,
  displayTokenToolScope,
  effectiveAllowedArgs,
  effectiveAgentStatus,
  outboundAgentRequiredFieldsMissing,
  labelSummaries,
  managedOutboundAgentFromConfig,
  normalizeIPVersions,
  parseOutboundHeaders,
  normalizeRegisterAgentIDs,
  registerTokenDraftChanged,
  normalizeToken,
  normalizeTokenToolScope,
  setPolicyEnabled,
  selectTokenTools,
  updateAllowedArg,
  withPolicyDefaults,
  withTokenDefaults
};

export function ManagePage({ client, permissions, t }: { client: ApiClient | null; permissions: Permissions | null; t: (key: string, options?: Record<string, unknown>) => string }) {
  const [settings, setSettings] = useState<ManagedSettings>(emptySettings);
  const [agents, setAgents] = useState<ManagedAgent[]>([]);
  const [outboundAgents, setOutboundAgents] = useState<ManagedAgent[]>([]);
  const [agentConfigForm, setAgentConfigForm] = useState<AgentConfig>(emptyAgentConfig);
  const [outboundAgentForm, setOutboundAgentForm] = useState<OutboundAgentConfig>(emptyOutboundAgent);
  const [outboundHeadersJSON, setOutboundHeadersJSON] = useState("{}");
  const [tokenForm, setTokenForm] = useState<APITokenPermission>(emptyAPIToken);
  const [labelForm, setLabelForm] = useState<LabelConfigSettings>(emptyLabelConfig);
  const [labelNodes, setLabelNodes] = useState<string[]>([]);
  const [selectionLabels, setSelectionLabels] = useState<string[]>([]);
  const [editingAgentId, setEditingAgentId] = useState("");
  const [editingOutboundAgentId, setEditingOutboundAgentId] = useState("");
  const [editingOutboundAgentOriginal, setEditingOutboundAgentOriginal] = useState<OutboundAgentConfig | null>(null);
  const [editingTokenIndex, setEditingTokenIndex] = useState<number | null>(null);
  const [editingLabel, setEditingLabel] = useState("");
  const [editingOriginalLabel, setEditingOriginalLabel] = useState("");
  const [agentModalOpen, setAgentModalOpen] = useState(false);
  const [outboundAgentModalOpen, setOutboundAgentModalOpen] = useState(false);
  const [tokenModalOpen, setTokenModalOpen] = useState(false);
  const [labelModalOpen, setLabelModalOpen] = useState(false);
  const [savingRegisterTokenID, setSavingRegisterTokenID] = useState<string | null>(null);
  const [section, setSection] = useState("outboundAgents");
  const [loading, setLoading] = useState(false);
  const [savingSettings, setSavingSettings] = useState(false);
  const [savingAgentConfig, setSavingAgentConfig] = useState(false);
  const [savingOutboundAgent, setSavingOutboundAgent] = useState(false);
  const [savingToken, setSavingToken] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const writable = canWriteManage(permissions);
  const enabledOutboundAgents = useMemo(() => outboundAgents.filter((agent) => agent.outbound?.enabled).length, [outboundAgents]);
  const enabledAgents = useMemo(() => agents.filter((agent) => !agent.config?.disabled).length, [agents]);
  const allManagedAgents = useMemo(() => [...agents, ...outboundAgents].sort((a, b) => a.id.localeCompare(b.id)), [agents, outboundAgents]);
  const labelRows = useMemo(() => labelSummaries(allManagedAgents, settings.label_configs ?? {}), [allManagedAgents, settings.label_configs]);
  const nodeOptions = useMemo(() => allManagedAgents.map((agent) => ({ value: agent.id, label: nodeOptionLabel(agent) })), [allManagedAgents]);
  const labelOptions = useMemo(() => labelRows.map((row) => ({ value: row.label, label: `${row.label} (${row.count})` })), [labelRows]);
  const outboundHeaders = parseOutboundHeaders(outboundHeadersJSON);
  const outboundAgentCandidate = outboundAgentForSave(outboundAgentForm, outboundHeaders ?? outboundAgentForm.headers ?? {});
  const outboundAgentMissingFields = outboundAgentRequiredFieldsMissing(outboundAgentCandidate, editingOutboundAgentId, outboundAgentModalOpen, editingOutboundAgentOriginal);
  const outboundAgentSubmitDisabled = !writable || outboundHeaders === null || Object.values(outboundAgentMissingFields).some(Boolean);
  const accessOptions = useMemo(() => localizedAccessOptions(t), [t]);

  useEffect(() => {
    if (client) {
      void load();
    }
  }, [client, permissions, writable]);

  useEffect(() => {
    if (!writable && section === "tokens") {
      setSection("outboundAgents");
    }
  }, [section, writable]);

  async function load() {
    if (!client) {
      return;
    }
    setLoading(true);
    try {
      const readable = canReadManage(permissions);
      const [rateLimit, labels, apiTokens, registerTokens, nextAgents] = await Promise.all([
        readable ? client.getManagedRateLimit() : Promise.resolve({ revision: undefined, updated_at: undefined, rate_limit: emptyRateLimit }),
        readable ? client.getManagedLabels() : Promise.resolve({ revision: undefined, updated_at: undefined, label_configs: {} }),
        writable ? client.listManagedTokens() : Promise.resolve({ revision: undefined, updated_at: undefined, tokens: [] }),
        writable ? client.listManagedRegisterTokens() : Promise.resolve({ revision: undefined, updated_at: undefined, tokens: [] }),
        readable ? client.listManagedAgents() : Promise.resolve([])
      ]);
      const revision = apiTokens.revision ?? registerTokens.revision ?? rateLimit.revision ?? labels.revision;
      const updatedAt = apiTokens.updated_at ?? registerTokens.updated_at ?? rateLimit.updated_at ?? labels.updated_at;
      setSettings(withSettingsDefaults({
        revision,
        updated_at: updatedAt,
        rate_limit: rateLimit.rate_limit,
        label_configs: labels.label_configs ?? {},
        api_tokens: apiTokens.tokens ?? [],
        register_tokens: registerTokens.tokens ?? []
      }));
      setAgents(nextAgents.filter((agent) => managedAgentType(agent) === "grpc"));
      setOutboundAgents(nextAgents.filter((agent) => managedAgentType(agent) !== "grpc"));
      setError(null);
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setLoading(false);
    }
  }

  async function persistRateLimit(rateLimit: RateLimitConfig, message = t("manage.saved")) {
    if (!client || !writable) {
      return;
    }
    setSavingSettings(true);
    try {
      const saved = await client.updateManagedRateLimit({ revision: settings.revision, rate_limit: rateLimit });
      setSettings((current) => withSettingsDefaults({ ...current, revision: saved.revision, updated_at: saved.updated_at, rate_limit: saved.rate_limit }));
      notifications.show({ color: "green", message });
      setError(null);
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setSavingSettings(false);
    }
  }

  async function saveOutboundAgent(event: FormEvent) {
    event.preventDefault();
    if (!client || !writable) {
      return;
    }
    const headers = parseOutboundHeaders(outboundHeadersJSON);
    if (headers === null) {
      return;
    }
    const payload = outboundAgentForSave({
      ...outboundAgentForm,
      id: editingOutboundAgentId || outboundAgentForm.id
    }, headers);
    if (Object.values(outboundAgentRequiredFieldsMissing(payload, editingOutboundAgentId, true, editingOutboundAgentOriginal)).some(Boolean)) {
      return;
    }
    setSavingOutboundAgent(true);
    try {
      const saved = withOutboundAgentDefaults(editingOutboundAgentId ? await client.updateOutboundAgent(editingOutboundAgentId, payload) : await client.createOutboundAgent(payload));
      setOutboundAgents((current) => upsertManagedOutboundAgent(current, saved));
      closeOutboundAgentModal();
      notifications.show({ color: "green", message: t("manage.outboundAgentSaved") });
      setError(null);
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setSavingOutboundAgent(false);
    }
  }

  async function deleteOutboundAgent(id: string) {
    if (!client || !writable) {
      return;
    }
    try {
      await client.deleteManagedAgent(id);
      setOutboundAgents((current) => current.filter((agent) => agent.id !== id));
      if (editingOutboundAgentId === id) {
        closeOutboundAgentModal();
      }
      notifications.show({ color: "green", message: t("manage.outboundAgentDeleted") });
    } catch (err) {
      setError(errorMessage(err));
    }
  }

  async function saveAgentConfig(event: FormEvent) {
    event.preventDefault();
    if (!client || !writable || !editingAgentId) {
      return;
    }
    setSavingAgentConfig(true);
    try {
      const saved = await client.updateAgentConfig(editingAgentId, { ...agentConfigForm, id: editingAgentId, transport: "grpc" });
      setAgents((current) => current.map((agent) => agent.id === editingAgentId ? { ...agent, config: saved } : agent));
      closeAgentModal();
      notifications.show({ color: "green", message: t("manage.agentSaved") });
      setError(null);
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setSavingAgentConfig(false);
    }
  }

  async function deleteAgent(id: string) {
    if (!client || !writable) {
      return;
    }
    try {
      await client.deleteManagedAgent(id);
      setAgents((current) => current.filter((agent) => agent.id !== id));
      if (editingAgentId === id) {
        closeAgentModal();
      }
      notifications.show({ color: "green", message: t("manage.agentDeleted") });
    } catch (err) {
      setError(errorMessage(err));
    }
  }

  async function saveToken(event: FormEvent) {
    event.preventDefault();
    if (!client || !writable) {
      return;
    }
    const token = normalizeToken(tokenForm);
    setSavingToken(true);
    try {
      const payload = managedTokenRequest(token);
      const response = editingTokenIndex === null
        ? await client.createManagedToken(payload)
        : await client.updateManagedToken((settings.api_tokens ?? [])[editingTokenIndex]?.id ?? "", payload);
      const saved = response.token;
      setSettings((current) => withSettingsDefaults({
        ...current,
        revision: response.revision,
        updated_at: response.updated_at,
        api_tokens: editingTokenIndex === null
          ? [...(current.api_tokens ?? []), saved]
          : (current.api_tokens ?? []).map((item, itemIndex) => itemIndex === editingTokenIndex ? saved : item)
      }));
      notifications.show({ color: "green", message: t("manage.tokenSaved") });
      setError(null);
      closeTokenModal();
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setSavingToken(false);
    }
  }

  async function deleteToken(index: number) {
    if (!client || !writable) {
      return;
    }
    const token = (settings.api_tokens ?? [])[index];
    if (!token?.id) {
      return;
    }
    setSavingSettings(true);
    try {
      const response = await client.deleteManagedToken(token.id);
      setSettings((current) => withSettingsDefaults({ ...current, revision: response.revision, updated_at: response.updated_at, api_tokens: response.tokens ?? (current.api_tokens ?? []).filter((_, itemIndex) => itemIndex !== index) }));
      notifications.show({ color: "green", message: t("manage.tokenDeleted") });
      setError(null);
      if (editingTokenIndex === index) {
        closeTokenModal();
      }
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setSavingSettings(false);
    }
  }

  async function rotateToken(index: number) {
    if (!client || !writable) {
      return;
    }
    const token = (settings.api_tokens ?? [])[index];
    if (!token?.id) {
      return;
    }
    setSavingSettings(true);
    try {
      const response = await client.rotateManagedToken(token.id);
      const saved = response.token;
      setSettings((current) => withSettingsDefaults({ ...current, revision: response.revision, updated_at: response.updated_at, api_tokens: (current.api_tokens ?? []).map((item, itemIndex) => itemIndex === index ? saved : item) }));
      notifications.show({ color: "green", message: t("manage.tokenRotated") });
      setError(null);
      if (editingTokenIndex === index) {
        closeTokenModal();
      }
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setSavingSettings(false);
    }
  }

  async function createRegisterToken() {
    if (!client || !writable) {
      return;
    }
    setSavingSettings(true);
    try {
      const response = await client.createManagedRegisterToken({ name: "", remaining_uses: 1, allowed_agent_ids: [] });
      const saved = response.token;
      setSettings((current) => withSettingsDefaults({ ...current, revision: response.revision, updated_at: response.updated_at, register_tokens: [...(current.register_tokens ?? []), saved] }));
      notifications.show({ color: "green", message: t("manage.registerTokenSaved") });
      setError(null);
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setSavingSettings(false);
    }
  }

  async function saveRegisterToken(tokenID: string, draft: RegisterTokenDraft) {
    if (!client || !writable) {
      return;
    }
    if (!tokenID) {
      return;
    }
    setSavingRegisterTokenID(tokenID);
    try {
      const response = await client.updateManagedRegisterToken(tokenID, {
        name: draft.name.trim(),
        remaining_uses: Math.max(0, Math.floor(draft.remainingUses)),
        allowed_agent_ids: normalizeRegisterAgentIDs(draft.allowedAgentIDs)
      });
      const saved = response.token;
      setSettings((current) => withSettingsDefaults({ ...current, revision: response.revision, updated_at: response.updated_at, register_tokens: (current.register_tokens ?? []).map((item) => item.id === tokenID ? saved : item) }));
      notifications.show({ color: "green", message: t("manage.registerTokenSaved") });
      setError(null);
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setSavingRegisterTokenID(null);
    }
  }

  async function deleteRegisterToken(tokenID: string) {
    if (!client || !writable) {
      return;
    }
    if (!tokenID) {
      return;
    }
    setSavingSettings(true);
    try {
      const response = await client.deleteManagedRegisterToken(tokenID);
      setSettings((current) => withSettingsDefaults({ ...current, revision: response.revision, updated_at: response.updated_at, register_tokens: response.tokens ?? (current.register_tokens ?? []).filter((item) => item.id !== tokenID) }));
      notifications.show({ color: "green", message: t("manage.registerTokenDeleted") });
      setError(null);
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setSavingSettings(false);
    }
  }

  async function rotateRegisterToken(tokenID: string) {
    if (!client || !writable) {
      return;
    }
    if (!tokenID) {
      return;
    }
    setSavingRegisterTokenID(tokenID);
    try {
      const response = await client.rotateManagedRegisterToken(tokenID);
      const saved = response.token;
      setSettings((current) => withSettingsDefaults({ ...current, revision: response.revision, updated_at: response.updated_at, register_tokens: (current.register_tokens ?? []).map((item) => item.id === tokenID ? saved : item) }));
      notifications.show({ color: "green", message: t("manage.registerTokenRotated") });
      setError(null);
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setSavingRegisterTokenID(null);
    }
  }

  function openNewOutboundAgentModal() {
    setEditingOutboundAgentId("");
    setEditingOutboundAgentOriginal(null);
    setOutboundAgentForm(withOutboundAgentDefaults(emptyOutboundAgent));
    setOutboundHeadersJSON(formatOutboundHeaders({}));
    setOutboundAgentModalOpen(true);
  }

  function editOutboundAgent(node: OutboundAgentConfig) {
    const original = withOutboundAgentDefaults(node);
    setEditingOutboundAgentId(node.id);
    setEditingOutboundAgentOriginal(original);
    setOutboundAgentForm(withOutboundAgentDefaults(node));
    setOutboundHeadersJSON(formatOutboundHeaders(node.headers));
    setOutboundAgentModalOpen(true);
  }

  function closeOutboundAgentModal() {
    setOutboundAgentModalOpen(false);
    setEditingOutboundAgentId("");
    setEditingOutboundAgentOriginal(null);
    setOutboundAgentForm(withOutboundAgentDefaults(emptyOutboundAgent));
    setOutboundHeadersJSON(formatOutboundHeaders({}));
  }

  function editAgent(agent: ManagedAgent) {
    setEditingAgentId(agent.id);
    setAgentConfigForm(withAgentConfigDefaults(agent.id, agent.config));
    setAgentModalOpen(true);
  }

  function closeAgentModal() {
    setAgentModalOpen(false);
    setEditingAgentId("");
    setAgentConfigForm(withAgentConfigDefaults("", undefined));
  }

  function openNewTokenModal() {
    setEditingTokenIndex(null);
    setTokenForm(withTokenDefaults(emptyAPIToken));
    setTokenModalOpen(true);
  }

  function editToken(token: APITokenPermission, index: number) {
    setEditingTokenIndex(index);
    setTokenForm(withTokenDefaults(token));
    setTokenModalOpen(true);
  }

  function closeTokenModal() {
    setTokenModalOpen(false);
    setEditingTokenIndex(null);
    setTokenForm(withTokenDefaults(emptyAPIToken));
  }

  function openNewLabelModal() {
    const label = firstAvailableLabelName(settings.label_configs ?? {}, allManagedAgents);
    setEditingOriginalLabel("");
    setEditingLabel(label);
    setLabelForm(emptyLabelConfig);
    setLabelNodes([]);
    setSelectionLabels([]);
    setLabelModalOpen(true);
  }

  function openLabelModal(label: string) {
    setEditingOriginalLabel(label);
    setEditingLabel(label);
    setLabelForm(withLabelConfigDefaults((settings.label_configs ?? {})[label]));
    setLabelNodes(agentIDsForLabel(allManagedAgents, label));
    setSelectionLabels([]);
    setLabelModalOpen(true);
  }

  function closeLabelModal() {
    setLabelModalOpen(false);
    setEditingLabel("");
    setEditingOriginalLabel("");
    setLabelForm(emptyLabelConfig);
    setLabelNodes([]);
    setSelectionLabels([]);
  }

  function applySelectionFromLabels(labels: string[]) {
    setSelectionLabels(labels);
    if (labels.length === 0) {
      return;
    }
    setLabelNodes(agentIDsForLabels(allManagedAgents, labels));
  }

  async function saveLabelConfig(event: FormEvent) {
    event.preventDefault();
    if (!writable || !editingLabel) {
      return;
    }
    const normalizedLabel = editingLabel.trim();
    if (!normalizedLabel) {
      return;
    }
    const nextSettings = {
      ...settings,
      label_configs: {
        ...(settings.label_configs ?? {}),
        [normalizedLabel]: normalizeLabelConfig(normalizedLabel, labelForm)
      }
    };
    const nextAgents = applyLabelMembership(agents, normalizedLabel, labelNodes);
    const nextOutboundAgents = applyLabelMembership(outboundAgents, normalizedLabel, labelNodes);
    try {
      await persistLabelChanges(nextSettings, nextAgents, nextOutboundAgents, t("manage.labelSaved"));
      closeLabelModal();
    } catch {
      // persistLabelChanges already surfaced the error.
    }
  }

  async function deleteLabel(label: string) {
    if (!writable) {
      return;
    }
    const { [label]: _removed, ...labelConfigs } = settings.label_configs ?? {};
    const nextAgents = applyLabelMembership(agents, label, []);
    const nextOutboundAgents = applyLabelMembership(outboundAgents, label, []);
    try {
      await persistLabelChanges({ ...settings, label_configs: labelConfigs }, nextAgents, nextOutboundAgents, t("manage.labelDeleted"));
      if (editingLabel === label) {
        closeLabelModal();
      }
    } catch {
      // persistLabelChanges already surfaced the error.
    }
  }

  async function persistLabelChanges(nextSettings: ManagedSettings, nextAgents: ManagedAgent[], nextOutboundAgents: ManagedAgent[], message: string) {
    if (!client || !writable) {
      return;
    }
    setSavingSettings(true);
    try {
      const changedAgents = changedLabelAgents(agents, nextAgents);
      const changedOutboundAgents = changedLabelOutboundAgents(outboundAgents, nextOutboundAgents);
      const labelUpdates: ManagedAgentLabelUpdate[] = [
        ...changedAgents.map((agent) => ({ id: agent.id, labels: customLabels(agent.labels) })),
        ...changedOutboundAgents.map((agent) => ({ id: agent.id, labels: customLabels(agent.labels) }))
      ];
      const saved = await client.updateManagedLabelsAndAgents({
        revision: nextSettings.revision,
        label_configs: nextSettings.label_configs ?? {},
        agents: labelUpdates
      });
      const savedAgents = saved.agents.length > 0 ? saved.agents : [...nextAgents, ...nextOutboundAgents];
      setSettings((current) => withSettingsDefaults({ ...current, revision: saved.revision, updated_at: saved.updated_at, label_configs: saved.label_configs ?? {} }));
      setAgents(savedAgents.filter((agent) => managedAgentType(agent) === "grpc"));
      setOutboundAgents(savedAgents.filter((agent) => managedAgentType(agent) !== "grpc"));
      notifications.show({ color: "green", message });
      setError(null);
    } catch (err) {
      setError(errorMessage(err));
      throw err;
    } finally {
      setSavingSettings(false);
    }
  }

  return (
    <Stack className="manage-page" gap="lg">
      {error && (
        <Alert color="red" icon={<AlertTriangle size={18} />} title={t("errors.requestFailed")}>
          {error}
        </Alert>
      )}

      <Group gap="sm" justify="space-between">
        <Group gap="sm">
          <Badge color="indigo" variant="light">
            <ResponsiveActionLabel full={t("manage.enabledAgents", { count: enabledAgents })} compact={t("manage.enabledAgentsCompact", { count: enabledAgents })} />
          </Badge>
          <Badge color="teal" variant="light">
            <ResponsiveActionLabel full={t("manage.enabledOutboundAgents", { count: enabledOutboundAgents })} compact={t("manage.enabledOutboundAgentsCompact", { count: enabledOutboundAgents })} />
          </Badge>
        </Group>
        <Button
          aria-busy={loading}
          className="refresh-button"
          disabled={loading}
          leftSection={<RotateCcw className={loading ? "button-progress-icon is-spinning" : "button-progress-icon"} size={16} />}
          onClick={load}
          variant="default"
        >
          {t("schedule.refresh")}
        </Button>
      </Group>

      <ManageTabs
        value={section}
        onChange={setSection}
        data={[
          { value: "outboundAgents", label: t("manage.nodesTab") },
          { value: "labels", label: t("manage.labelsTab") },
          ...(writable ? [{ value: "tokens", label: t("manage.tokensTab") }] : []),
          { value: "settings", label: t("manage.rateLimitTab") }
        ]}
      />

      {section === "settings" && (
        <Stack gap="md">
          <Paper className="manage-agent-box" withBorder p="md">
            <Stack gap="lg">
              <SectionHeader title={t("manage.rateLimitTab")} />
              <RateLimitEditor value={settings.rate_limit} disabled={!writable} t={t} onChange={(rateLimit) => setSettings({ ...settings, rate_limit: rateLimit })} />
              <Group justify="flex-end">
                <GuardedButton leftSection={<Save size={16} />} loading={savingSettings} disabled={!writable} disabledReason={t("manage.writeRequired")} onClick={() => persistRateLimit(settings.rate_limit)}>
                  {t("manage.saveSettings")}
                </GuardedButton>
              </Group>
            </Stack>
          </Paper>
        </Stack>
      )}

      {section === "outboundAgents" && (
        <Stack gap="lg">
          <Paper className="manage-agent-box" withBorder p="md">
            <Stack gap="md">
              <SectionHeader title={t("manage.agents")} />
              <Table.ScrollContainer minWidth={760}>
                <Table className="manage-table" verticalSpacing="xs">
                  <Table.Thead>
                    <Table.Tr>
                      <Table.Th>{t("manage.id")}</Table.Th>
                      <Table.Th>{t("manage.labels")}</Table.Th>
                      <Table.Th>{t("manage.status")}</Table.Th>
                      <Table.Th>{t("manage.lastSeen")}</Table.Th>
                      <Table.Th className="manage-table-actions">{t("schedule.actions")}</Table.Th>
                    </Table.Tr>
                  </Table.Thead>
                  <Table.Tbody>
                    {agents.map((agent) => (
                      <Table.Tr key={agent.id}>
                        <Table.Td><Text fw={600}>{agent.id}</Text></Table.Td>
                        <Table.Td><LabelBadges labels={agent.labels} /></Table.Td>
                        <Table.Td><AgentBadges agent={agent} t={t} /></Table.Td>
                        <Table.Td>{formatDateTime(agent.last_seen_at)}</Table.Td>
                        <Table.Td className="manage-table-actions">
                          <Group gap="xs" justify="flex-end">
                            <Tooltip label={t("schedule.edit")}><ActionIcon variant="subtle" onClick={() => editAgent(agent)}><Pencil size={16} /></ActionIcon></Tooltip>
                            <GuardedActionIcon label={t("schedule.delete")} disabled={!writable} disabledReason={t("manage.writeRequired")} color="red" variant="subtle" onClick={() => deleteAgent(agent.id)}><Trash2 size={16} /></GuardedActionIcon>
                          </Group>
                        </Table.Td>
                      </Table.Tr>
                    ))}
                    {agents.length === 0 && <EmptyRow colSpan={5} label={t("manage.noAgents")} />}
                  </Table.Tbody>
                </Table>
              </Table.ScrollContainer>
            </Stack>
          </Paper>
          <Paper className="manage-agent-box" withBorder p="md">
            <Stack gap="md">
              <SectionHeader title={t("manage.outboundAgents")} action={<GuardedButton leftSection={<Plus size={16} />} disabled={!writable} disabledReason={t("manage.writeRequired")} onClick={openNewOutboundAgentModal}><ResponsiveActionLabel full={t("manage.addOutboundAgent")} compact={t("actions.add")} /></GuardedButton>} />
              <Table.ScrollContainer minWidth={920}>
                <Table className="manage-table" verticalSpacing="xs">
                  <Table.Thead>
                    <Table.Tr>
                      <Table.Th>{t("manage.id")}</Table.Th>
                      <Table.Th>{t("manage.transport")}</Table.Th>
                      <Table.Th>{t("manage.url")}</Table.Th>
                      <Table.Th>{t("manage.labels")}</Table.Th>
                      <Table.Th className="manage-http-status-column">{t("manage.status")}</Table.Th>
                      <Table.Th className="manage-date-column">{t("manage.updated")}</Table.Th>
                      <Table.Th className="manage-table-actions">{t("schedule.actions")}</Table.Th>
                    </Table.Tr>
                  </Table.Thead>
                  <Table.Tbody>
                    {outboundAgents.map((agent) => {
                      const node = withOutboundAgentDefaults(agent.outbound ?? { ...emptyOutboundAgent, id: agent.id, transport: agent.transport === "event" ? "event" : "http" });
                      return (
                        <Table.Tr key={agent.id}>
                          <Table.Td><Text fw={600}>{agent.id}</Text></Table.Td>
                          <Table.Td><Badge variant="light">{node.transport}</Badge></Table.Td>
                          <Table.Td>{node.url}</Table.Td>
                          <Table.Td><LabelBadges labels={agent.labels ?? node.labels} /></Table.Td>
                          <Table.Td className="manage-http-status-column"><OutboundAgentBadges agent={agent} node={node} t={t} /></Table.Td>
                          <Table.Td className="manage-date-column">{formatDateTime(node.updated_at || agent.last_seen_at)}</Table.Td>
                          <Table.Td className="manage-table-actions">
                            <Group gap="xs" justify="flex-end">
                              <Tooltip label={t("schedule.edit")}><ActionIcon variant="subtle" onClick={() => editOutboundAgent(node)}><Pencil size={16} /></ActionIcon></Tooltip>
                              <GuardedActionIcon label={t("schedule.delete")} disabled={!writable} disabledReason={t("manage.writeRequired")} color="red" variant="subtle" onClick={() => deleteOutboundAgent(agent.id)}><Trash2 size={16} /></GuardedActionIcon>
                            </Group>
                          </Table.Td>
                        </Table.Tr>
                      );
                    })}
                    {outboundAgents.length === 0 && <EmptyRow colSpan={7} label={t("manage.noOutboundAgentConfigs")} />}
                  </Table.Tbody>
                </Table>
              </Table.ScrollContainer>
            </Stack>
          </Paper>
        </Stack>
      )}

      {writable && section === "tokens" && (
        <Stack gap="lg">
            <Paper className="manage-agent-box" withBorder p="md">
              <Stack gap="md">
                <SectionHeader title={t("manage.tokens")} action={<GuardedButton leftSection={<Plus size={16} />} disabled={!writable} disabledReason={t("manage.writeRequired")} onClick={openNewTokenModal}><ResponsiveActionLabel full={t("manage.addToken")} compact={t("actions.add")} /></GuardedButton>} />
                <Table.ScrollContainer minWidth={1080}>
                  <Table className="manage-table manage-token-table manage-api-token-table" verticalSpacing="xs">
                    <Table.Thead>
                      <Table.Tr>
                        <Table.Th className="manage-token-name-column">{t("manage.tokenName")}</Table.Th>
                        <Table.Th className="manage-token-secret-column">{t("manage.token")}</Table.Th>
                        <Table.Th className="manage-api-token-access-column">{t("manage.access")}</Table.Th>
                        <Table.Th className="manage-api-token-scope-column">{t("manage.scope")}</Table.Th>
                        <Table.Th className="manage-api-token-tools-column">{t("manage.tools")}</Table.Th>
                        <Table.Th className="manage-table-actions">{t("schedule.actions")}</Table.Th>
                      </Table.Tr>
                    </Table.Thead>
                    <Table.Tbody>
                      {(settings.api_tokens ?? []).map((token, index) => (
                        <Table.Tr key={`${token.id || token.secret}:${index}`}>
                          <Table.Td className="manage-token-name-column">{token.name || token.secret.slice(0, 8) || "-"}</Table.Td>
                          <Table.Td className="manage-token-secret-column"><Text className="manage-token-secret" fw={600}>{token.secret || "********"}</Text></Table.Td>
                          <Table.Td className="manage-api-token-access-column"><TokenAccessBadges token={token} t={t} /></Table.Td>
                          <Table.Td className="manage-api-token-scope-column">{tokenScopeLabel(token)}</Table.Td>
                          <Table.Td className="manage-api-token-tools-column"><TokenToolBadges token={token} /></Table.Td>
                          <Table.Td className="manage-table-actions">
                            <Group gap="xs" justify="flex-end">
                              <Tooltip label={t("schedule.edit")}><ActionIcon variant="subtle" onClick={() => editToken(token, index)}><Pencil size={16} /></ActionIcon></Tooltip>
                              <GuardedActionIcon label={t("manage.rotateToken")} disabled={!writable || savingSettings} disabledReason={t("manage.writeRequired")} variant="subtle" onClick={() => rotateToken(index)}><RefreshCw size={16} /></GuardedActionIcon>
                              <GuardedActionIcon label={t("schedule.delete")} disabled={!writable} disabledReason={t("manage.writeRequired")} color="red" variant="subtle" onClick={() => deleteToken(index)}><Trash2 size={16} /></GuardedActionIcon>
                            </Group>
                          </Table.Td>
                        </Table.Tr>
                      ))}
                      {(settings.api_tokens ?? []).length === 0 && <EmptyRow colSpan={6} label={t("manage.noTokens")} />}
                    </Table.Tbody>
                  </Table>
                </Table.ScrollContainer>
              </Stack>
            </Paper>

            <Paper className="manage-agent-box" withBorder p="md">
              <Stack gap="md">
                <SectionHeader title={t("manage.registerTokenList")} action={<GuardedButton leftSection={<Plus size={16} />} loading={savingSettings} disabled={!writable || savingRegisterTokenID !== null} disabledReason={t("manage.writeRequired")} onClick={createRegisterToken}><ResponsiveActionLabel full={t("manage.addRegisterToken")} compact={t("actions.add")} /></GuardedButton>} />
                <Table.ScrollContainer minWidth={1080}>
                  <Table className="manage-table manage-token-table manage-register-token-table" verticalSpacing="xs">
                    <Table.Thead>
                      <Table.Tr>
                        <Table.Th className="manage-token-name-column">{t("manage.tokenName")}</Table.Th>
                        <Table.Th className="manage-token-secret-column">{t("manage.token")}</Table.Th>
                        <Table.Th className="manage-register-token-uses-column">{t("manage.remainingUses")}</Table.Th>
                        <Table.Th className="manage-register-token-agent-ids-column">{t("manage.allowedAgentIDs")}</Table.Th>
                        <Table.Th className="manage-table-actions">{t("schedule.actions")}</Table.Th>
                      </Table.Tr>
                    </Table.Thead>
                    <Table.Tbody>
                      {(settings.register_tokens ?? []).map((token, index) => {
                        const tokenID = token.id ?? "";
                        const tokenBusy = savingSettings || savingRegisterTokenID !== null;
                        const tokenLoading = savingRegisterTokenID === tokenID;
                        return (
                          <RegisterTokenRow
                            key={`${tokenID || token.token}:${index}`}
                            disabled={!writable || tokenBusy}
                            loading={tokenLoading}
                            onDelete={() => deleteRegisterToken(tokenID)}
                            onRotate={() => rotateRegisterToken(tokenID)}
                            onSave={(draft) => saveRegisterToken(tokenID, draft)}
                            t={t}
                            token={token}
                          />
                        );
                      })}
                      {(settings.register_tokens ?? []).length === 0 && <EmptyRow colSpan={5} label={t("manage.noRegisterTokens")} />}
                    </Table.Tbody>
                  </Table>
                </Table.ScrollContainer>
              </Stack>
            </Paper>
        </Stack>
      )}

      {section === "labels" && (
        <Paper className="manage-agent-box" withBorder p="md">
          <Stack gap="md">
            <SectionHeader
              title={t("manage.labelManagement")}
              action={<GuardedButton leftSection={<Plus size={16} />} disabled={!writable} disabledReason={t("manage.writeRequired")} onClick={openNewLabelModal}><ResponsiveActionLabel full={t("manage.addLabel")} compact={t("actions.add")} /></GuardedButton>}
            />
            <Table.ScrollContainer minWidth={820}>
              <Table className="manage-table" verticalSpacing="xs">
                <Table.Thead>
                  <Table.Tr>
                    <Table.Th>{t("manage.labels")}</Table.Th>
                    <Table.Th>{t("manage.nodes")}</Table.Th>
                    <Table.Th>{t("manage.parameters")}</Table.Th>
                    <Table.Th>{t("manage.policies")}</Table.Th>
                    <Table.Th className="manage-table-actions">{t("schedule.actions")}</Table.Th>
                  </Table.Tr>
                </Table.Thead>
                <Table.Tbody>
                  {labelRows.map((row) => (
                    <Table.Tr key={row.label}>
                      <Table.Td><Text fw={600}>{row.label}</Text></Table.Td>
                      <Table.Td>{row.count}</Table.Td>
                      <Table.Td><ConfigBadges config={row.config} t={t} /></Table.Td>
                      <Table.Td><ToolPolicyBadges policies={row.config.tool_policies} /></Table.Td>
                      <Table.Td className="manage-table-actions">
                        <Group gap="xs" justify="flex-end">
                          <Tooltip label={t("schedule.edit")}><ActionIcon variant="subtle" onClick={() => openLabelModal(row.label)}><Pencil size={16} /></ActionIcon></Tooltip>
                          <GuardedActionIcon label={t("schedule.delete")} disabled={!writable || isReservedLabel(row.label)} disabledReason={isReservedLabel(row.label) ? t("manage.reservedLabel") : t("manage.writeRequired")} color="red" variant="subtle" onClick={() => deleteLabel(row.label)}><Trash2 size={16} /></GuardedActionIcon>
                        </Group>
                      </Table.Td>
                    </Table.Tr>
                  ))}
                  {labelRows.length === 0 && <EmptyRow colSpan={5} label={t("manage.noLabels")} />}
                </Table.Tbody>
              </Table>
            </Table.ScrollContainer>
          </Stack>
        </Paper>
      )}

      <Modal centered size="xl" opened={agentModalOpen} title={t("manage.editAgent")} onClose={closeAgentModal}>
        <form className="manage-modal-scope" onSubmit={saveAgentConfig}>
          <Stack gap="md">
            <Box className="manage-node-fields">
              <SimpleGrid cols={{ base: 1, md: 2 }}>
                <TextInput label={t("manage.id")} value={editingAgentId} readOnly />
                <TagsInput label={t("manage.labels")} value={agentConfigForm.labels ?? []} disabled={!writable} onChange={(labels) => setAgentConfigForm({ ...agentConfigForm, labels })} />
              </SimpleGrid>
              <Group mt="md">
                <Switch label={t("manage.disabled")} checked={Boolean(agentConfigForm.disabled)} disabled={!writable} onChange={(event) => setAgentConfigForm({ ...agentConfigForm, disabled: event.currentTarget.checked })} />
              </Group>
            </Box>
            <ModalActions loading={savingAgentConfig} disabled={!writable} disabledReason={t("manage.writeRequired")} onCancel={closeAgentModal} submitLabel={t("manage.saveAgent")} t={t} />
          </Stack>
        </form>
      </Modal>

      <Modal centered size="xl" opened={outboundAgentModalOpen} title={editingOutboundAgentId ? t("manage.editOutboundAgent") : t("manage.newOutboundAgent")} onClose={closeOutboundAgentModal}>
        <form className="manage-modal-scope" onSubmit={saveOutboundAgent}>
          <Stack gap="md">
            <Stack className="manage-node-fields" gap="md">
              <SimpleGrid cols={{ base: 1, md: 2 }}>
                <TextInput label={t("manage.id")} value={outboundAgentForm.id} disabled={!writable || Boolean(editingOutboundAgentId)} error={outboundAgentMissingFields.id} onChange={(event) => setOutboundAgentForm({ ...outboundAgentForm, id: event.currentTarget.value })} />
                <Select
                  label={t("manage.transport")}
                  data={[
                    { value: "http", label: t("manage.transportHTTP") },
                    { value: "event", label: t("manage.transportEvent") }
                  ]}
                  value={outboundAgentForm.transport}
                  disabled={!writable}
                  allowDeselect={false}
                  onChange={(value) => setOutboundAgentForm({ ...outboundAgentForm, transport: value === "event" ? "event" : "http" })}
                />
                <TextInput label={t("manage.url")} value={outboundAgentForm.url} disabled={!writable} error={outboundAgentMissingFields.url} onChange={(event) => setOutboundAgentForm({ ...outboundAgentForm, url: event.currentTarget.value })} />
                <Select
                  label={t("manage.authType")}
                  data={[
                    { value: "none", label: t("manage.authNone") },
                    { value: "bearer", label: t("manage.authBearer") },
                    { value: "header", label: t("manage.authHeaderType") },
                    { value: "huaweicloud_aksk", label: t("manage.authHuaweiCloudAKSK") }
                  ]}
                  value={outboundAgentForm.auth.type}
                  disabled={!writable}
                  allowDeselect={false}
                  onChange={(value) => setOutboundAgentForm({ ...outboundAgentForm, auth: outboundAgentAuthForType(value, outboundAgentForm.auth) })}
                />
                {outboundAgentForm.auth.type === "header" && (
                  <TextInput label={t("manage.authHeader")} value={outboundAgentForm.auth.header ?? ""} disabled={!writable} error={outboundAgentMissingFields.header} onChange={(event) => setOutboundAgentForm({ ...outboundAgentForm, auth: { ...outboundAgentForm.auth, header: event.currentTarget.value } })} />
                )}
                {(outboundAgentForm.auth.type === "bearer" || outboundAgentForm.auth.type === "header") && (
                  <PasswordInput
                    label={t("manage.authSecret")}
                    value={outboundAgentForm.auth.secret ?? ""}
                    disabled={!writable}
                    error={outboundAgentMissingFields.secret}
                    onChange={(event) => setOutboundAgentForm({ ...outboundAgentForm, auth: { ...outboundAgentForm.auth, secret: event.currentTarget.value } })}
                  />
                )}
                {outboundAgentForm.auth.type === "huaweicloud_aksk" && (
                  <>
                    <TextInput label={t("manage.accessKey")} value={outboundAgentForm.auth.access_key ?? ""} disabled={!writable} error={outboundAgentMissingFields.accessKey} onChange={(event) => setOutboundAgentForm({ ...outboundAgentForm, auth: { ...outboundAgentForm.auth, access_key: event.currentTarget.value } })} />
                    <TextInput label={t("manage.projectID")} value={outboundAgentForm.auth.project_id ?? ""} disabled={!writable} error={outboundAgentMissingFields.projectID} onChange={(event) => setOutboundAgentForm({ ...outboundAgentForm, auth: { ...outboundAgentForm.auth, project_id: event.currentTarget.value } })} />
                    <PasswordInput
                      label={t("manage.secretKey")}
                      value={outboundAgentForm.auth.secret ?? ""}
                      disabled={!writable}
                      error={outboundAgentMissingFields.secret}
                      onChange={(event) => setOutboundAgentForm({ ...outboundAgentForm, auth: { ...outboundAgentForm.auth, secret: event.currentTarget.value } })}
                    />
                  </>
                )}
                <Textarea
                  autosize
                  minRows={1}
                  label={t("manage.requestHeaders")}
                  value={outboundHeadersJSON}
                  disabled={!writable}
                  error={outboundHeaders === null ? t("manage.invalidRequestHeaders") : undefined}
                  onChange={(event) => setOutboundHeadersJSON(event.currentTarget.value)}
                />
                {outboundAgentForm.transport === "event" && (
                  <Select
                    label={t("manage.eventResponse")}
                    data={[
                      { value: "direct", label: t("manage.eventResponseDirect") },
                      { value: "result_field", label: t("manage.eventResponseResult") }
                    ]}
                    value={outboundAgentForm.event_response ?? "direct"}
                    disabled={!writable}
                    error={outboundAgentMissingFields.eventResponse}
                    allowDeselect={false}
                    onChange={(value) => setOutboundAgentForm({ ...outboundAgentForm, event_response: value === "result_field" ? "result_field" : "direct" })}
                  />
                )}
                <Box className="manage-node-labels-field">
                  <TagsInput label={t("manage.labels")} value={outboundAgentForm.labels ?? []} disabled={!writable} onChange={(labels) => setOutboundAgentForm({ ...outboundAgentForm, labels })} />
                </Box>
              </SimpleGrid>
              <Group justify="space-between">
                <Switch label={t("manage.mtls")} checked={Boolean(outboundAgentForm.tls?.enabled)} disabled={!writable} onChange={(event) => setOutboundAgentForm({ ...outboundAgentForm, tls: { ...outboundAgentForm.tls, enabled: event.currentTarget.checked } })} />
                <Switch className="manage-node-enable-switch" label={t("manage.enabled")} checked={outboundAgentForm.enabled} disabled={!writable} onChange={(event) => setOutboundAgentForm({ ...outboundAgentForm, enabled: event.currentTarget.checked })} />
              </Group>
              {outboundAgentForm.tls?.enabled && (
                <SimpleGrid cols={{ base: 1, md: 2 }}>
                  <TagsInput label={t("manage.caFiles")} value={outboundAgentForm.tls?.ca_files ?? []} disabled={!writable} onChange={(caFiles) => setOutboundAgentForm({ ...outboundAgentForm, tls: { ...outboundAgentForm.tls, ca_files: caFiles } })} />
                  <TextInput label={t("manage.certFile")} value={outboundAgentForm.tls?.cert_file ?? ""} disabled={!writable} onChange={(event) => setOutboundAgentForm({ ...outboundAgentForm, tls: { ...outboundAgentForm.tls, cert_file: event.currentTarget.value } })} />
                  <TextInput label={t("manage.keyFile")} value={outboundAgentForm.tls?.key_file ?? ""} disabled={!writable} onChange={(event) => setOutboundAgentForm({ ...outboundAgentForm, tls: { ...outboundAgentForm.tls, key_file: event.currentTarget.value } })} />
                  <TextInput label={t("manage.serverName")} value={outboundAgentForm.tls?.server_name ?? ""} disabled={!writable} onChange={(event) => setOutboundAgentForm({ ...outboundAgentForm, tls: { ...outboundAgentForm.tls, server_name: event.currentTarget.value } })} />
                </SimpleGrid>
              )}
            </Stack>
            <ModalActions loading={savingOutboundAgent} disabled={outboundAgentSubmitDisabled} disabledReason={!writable ? t("manage.writeRequired") : t("manage.requiredFields")} onCancel={closeOutboundAgentModal} submitLabel={editingOutboundAgentId ? t("manage.saveOutboundAgent") : t("manage.addOutboundAgent")} t={t} />
          </Stack>
        </form>
      </Modal>

      <Modal centered size="xl" opened={labelModalOpen} title={editingLabel ? t("manage.editLabel") : t("manage.newLabel")} onClose={closeLabelModal}>
        <form className="manage-modal-scope" onSubmit={saveLabelConfig}>
          <Stack gap="md">
            <ManageSection title={t("manage.basic")}>
              <SimpleGrid cols={{ base: 1, md: 2 }}>
                <TextInput label={t("manage.labels")} value={editingLabel} disabled={!writable || Boolean(editingOriginalLabel)} onChange={(event) => setEditingLabel(event.currentTarget.value)} />
                <MultiSelect
                  className="manage-nowrap-multiselect"
                  label={t("manage.selectByLabel")}
                  data={labelOptions.filter((item) => item.value !== editingLabel)}
                  value={selectionLabels}
                  disabled={!writable || isReservedLabel(editingLabel)}
                  clearable
                  searchable
                  onChange={applySelectionFromLabels}
                />
              </SimpleGrid>
              <MultiSelect mt="md" label={t("manage.nodes")} data={nodeOptions} value={labelNodes} disabled={!writable || isReservedLabel(editingLabel)} searchable onChange={setLabelNodes} />
            </ManageSection>
            <ManageSection
              title={t("manage.runtime")}
              action={<OverrideSwitch label={t("manage.overrideRuntime")} checked={isAgentLabel(editingLabel) || Boolean(labelForm.runtime)} disabled={!writable || isAgentLabel(editingLabel)} onChange={(checked) => setLabelForm({ ...labelForm, runtime: checked ? cloneRuntime(labelForm.runtime) : null })} />}
            >
              {(labelForm.runtime || isAgentLabel(editingLabel)) && <RuntimeEditor value={cloneRuntime(labelForm.runtime)} disabled={!writable} t={t} onChange={(runtime) => setLabelForm({ ...labelForm, runtime })} />}
            </ManageSection>
            <ManageSection
              title={t("manage.scheduler")}
              action={<OverrideSwitch label={t("manage.overrideScheduler")} checked={isAgentLabel(editingLabel) || Boolean(labelForm.scheduler)} disabled={!writable || isAgentLabel(editingLabel)} onChange={(checked) => setLabelForm({ ...labelForm, scheduler: checked ? cloneScheduler(labelForm.scheduler) : null })} />}
            >
              {(labelForm.scheduler || isAgentLabel(editingLabel)) && <SchedulerEditor value={cloneScheduler(labelForm.scheduler)} disabled={!writable} t={t} onChange={(scheduler) => setLabelForm({ ...labelForm, scheduler })} />}
            </ManageSection>
            <ManageSection title={t("manage.policies")}>
              <ToolPolicySections value={labelForm.tool_policies ?? {}} disabled={!writable} t={t} onChange={(tool, policy) => setLabelForm({ ...labelForm, tool_policies: { ...(labelForm.tool_policies ?? {}), [tool]: policy } })} />
            </ManageSection>
            <ModalActions loading={savingSettings} disabled={!writable || !editingLabel.trim()} disabledReason={!writable ? t("manage.writeRequired") : t("manage.requiredFields")} onCancel={closeLabelModal} submitLabel={t("manage.saveLabel")} t={t} />
          </Stack>
        </form>
      </Modal>

      <Modal centered size="lg" opened={tokenModalOpen} title={editingTokenIndex === null ? t("manage.newToken") : t("manage.editToken")} onClose={closeTokenModal}>
        <form className="manage-modal-scope" onSubmit={saveToken}>
          <Stack gap="md">
            <ManageSection title={t("manage.basic")}>
              <Stack gap="md">
                <SimpleGrid cols={{ base: 1, md: 2 }}>
                  <TextInput label={t("manage.tokenName")} value={tokenForm.name ?? ""} disabled={!writable} onChange={(event) => setTokenForm({ ...tokenForm, name: event.currentTarget.value })} />
                  <Select label={t("manage.scheduleAccess")} data={accessOptions} value={tokenForm.schedule_access ?? "none"} disabled={!writable || tokenForm.all} onChange={(value) => setTokenForm({ ...tokenForm, schedule_access: accessValue(value) })} />
                  <Select label={t("manage.manageAccess")} data={accessOptions} value={tokenForm.manage_access ?? "none"} disabled={!writable || tokenForm.all} onChange={(value) => setTokenForm({ ...tokenForm, manage_access: accessValue(value) })} />
                </SimpleGrid>
                <Switch label={t("manage.fullAccess")} checked={Boolean(tokenForm.all)} disabled={!writable} onChange={(event) => setTokenForm({ ...tokenForm, all: event.currentTarget.checked })} />
              </Stack>
            </ManageSection>
            <ManageSection title={t("manage.scope")}>
              <Stack gap="md">
                <SimpleGrid cols={{ base: 1, md: 2 }}>
                  <TagsInput label={t("manage.allowAgents")} value={tokenForm.agents ?? []} disabled={!writable || tokenForm.all} onChange={(agentsValue) => setTokenForm({ ...tokenForm, agents: agentsValue })} />
                  <TagsInput label={t("manage.denyAgents")} value={tokenForm.denied_agents ?? []} disabled={!writable || tokenForm.all} onChange={(agentsValue) => setTokenForm({ ...tokenForm, denied_agents: agentsValue })} />
                  <TagsInput label={t("manage.allowTags")} value={tokenForm.agent_tags ?? []} disabled={!writable || tokenForm.all} onChange={(tags) => setTokenForm({ ...tokenForm, agent_tags: tags })} />
                  <TagsInput label={t("manage.denyTags")} value={tokenForm.denied_tags ?? []} disabled={!writable || tokenForm.all} onChange={(tags) => setTokenForm({ ...tokenForm, denied_tags: tags })} />
                </SimpleGrid>
              </Stack>
            </ManageSection>
            <ManageSection title={t("manage.policies")}>
              <Stack gap="md">
                <MultiSelect className="manage-nowrap-multiselect" label={t("manage.tools")} data={tools.map((tool) => ({ value: tool, label: tool }))} value={Object.keys(tokenForm.tools ?? {})} disabled={!writable || tokenForm.all} onChange={(selected) => setTokenForm({ ...tokenForm, tools: selectTokenTools(tokenForm.tools ?? {}, selected as Tool[]) })} />
                <TokenToolPolicySections value={tokenForm.tools ?? {}} disabled={!writable || Boolean(tokenForm.all)} t={t} onChange={(tool, scope) => setTokenForm({ ...tokenForm, tools: { ...(tokenForm.tools ?? {}), [tool]: scope } })} />
              </Stack>
            </ManageSection>
            <ModalActions loading={savingToken || savingSettings} disabled={!writable} disabledReason={t("manage.writeRequired")} onCancel={closeTokenModal} submitLabel={editingTokenIndex === null ? t("manage.addToken") : t("manage.saveToken")} t={t} />
          </Stack>
        </form>
      </Modal>
    </Stack>
  );
}

function SectionHeader({ title, action }: { title: string; action?: React.ReactNode }) {
  return <Group justify="space-between" align="center"><Text fw={700}>{title}</Text>{action}</Group>;
}

function ResponsiveActionLabel({ full, compact }: { full: string; compact: string }) {
  return (
    <>
      <span className="responsive-label-full">{full}</span>
      <span className="responsive-label-compact">{compact}</span>
    </>
  );
}

function ManageSection({ title, action, children, marker = true }: { title: string; action?: React.ReactNode; children: React.ReactNode; marker?: boolean }) {
  return (
    <section className="manage-section-block">
      <Group className="manage-section-header" align="center" gap="xs" wrap="nowrap">
        {marker && <span aria-hidden="true" className="manage-section-marker" />}
        <Text className="manage-section-title" fw={700}>{title}</Text>
        <span aria-hidden="true" className="manage-section-rule" />
        {action && <div className="manage-section-action">{action}</div>}
      </Group>
      <div className="manage-section-body">{children}</div>
    </section>
  );
}

function EmptyRow({ colSpan, label }: { colSpan: number; label: string }) {
  return <Table.Tr><Table.Td colSpan={colSpan}><Text c="dimmed" ta="center">{label}</Text></Table.Td></Table.Tr>;
}

type RegisterTokenDraft = {
  name: string;
  remainingUses: number;
  allowedAgentIDs: string[];
};

function RegisterTokenRow({ disabled, loading, onDelete, onRotate, onSave, t, token }: {
  disabled: boolean;
  loading: boolean;
  onDelete: () => void | Promise<void>;
  onRotate: () => void | Promise<void>;
  onSave: (draft: RegisterTokenDraft) => void | Promise<void>;
  t: (key: string) => string;
  token: RegisterToken;
}) {
  const [draft, setDraft] = useState<RegisterTokenDraft>(() => registerTokenDraft(token));

  useEffect(() => {
    setDraft(registerTokenDraft(token));
  }, [token]);

  const changed = registerTokenDraftChanged(token, draft);

  return (
    <Table.Tr>
      <Table.Td className="manage-token-name-column">
        <TextInput
          aria-label={t("manage.tokenName")}
          className="manage-register-token-input manage-register-token-name-input"
          disabled={disabled}
          onChange={(event) => {
            const name = event.currentTarget.value;
            setDraft((current) => ({ ...current, name }));
          }}
          placeholder={t("manage.tokenName")}
          value={draft.name}
        />
      </Table.Td>
      <Table.Td className="manage-token-secret-column"><Text className="manage-token-secret" fw={600}>{token.token || "********"}</Text></Table.Td>
      <Table.Td className="manage-register-token-uses-column">
        <NumberInput
          aria-label={t("manage.remainingUses")}
          allowDecimal={false}
          allowNegative={false}
          className="manage-register-token-input manage-register-token-uses-input"
          disabled={disabled}
          min={0}
          onChange={(next) => setDraft((current) => ({ ...current, remainingUses: Math.max(0, Math.floor(Number(next) || 0)) }))}
          value={draft.remainingUses}
        />
      </Table.Td>
      <Table.Td className="manage-register-token-agent-ids-column">
        <TagsInput
          aria-label={t("manage.allowedAgentIDsPlaceholder")}
          className="manage-register-token-input manage-register-token-agent-ids-input"
          clearable
          disabled={disabled}
          onChange={(next) => setDraft((current) => ({ ...current, allowedAgentIDs: normalizeRegisterAgentIDs(next) }))}
          placeholder={draft.allowedAgentIDs.length === 0 ? t("manage.allowedAgentIDsPlaceholder") : ""}
          value={draft.allowedAgentIDs}
        />
      </Table.Td>
      <Table.Td className="manage-table-actions">
        <Group gap="xs" justify="flex-end">
          <GuardedActionIcon label={t("actions.save")} disabled={disabled || !changed} variant="subtle" onClick={() => onSave(draft)}>
            {loading ? <RotateCcw className="button-progress-icon is-spinning" size={16} /> : <Save size={16} />}
          </GuardedActionIcon>
          <GuardedActionIcon label={t("manage.rotateRegisterToken")} disabled={disabled} variant="subtle" onClick={onRotate}><RefreshCw size={16} /></GuardedActionIcon>
          <GuardedActionIcon label={t("schedule.delete")} disabled={disabled} color="red" variant="subtle" onClick={onDelete}><Trash2 size={16} /></GuardedActionIcon>
        </Group>
      </Table.Td>
    </Table.Tr>
  );
}

function registerTokenDraft(token: RegisterToken): RegisterTokenDraft {
  return {
    name: token.name ?? "",
    remainingUses: token.remaining_uses ?? 0,
    allowedAgentIDs: normalizeRegisterAgentIDs(token.allowed_agent_ids ?? [])
  };
}

function registerTokenDraftChanged(token: RegisterToken, draft: RegisterTokenDraft): boolean {
  const saved = registerTokenDraft(token);
  return draft.name.trim() !== saved.name.trim()
    || draft.remainingUses !== saved.remainingUses
    || JSON.stringify(normalizeRegisterAgentIDs(draft.allowedAgentIDs)) !== JSON.stringify(saved.allowedAgentIDs);
}

function normalizeRegisterAgentIDs(values: string[]): string[] {
  return Array.from(new Set(values.map((value) => value.trim()).filter(Boolean)));
}

function ManageTabs({ value, onChange, data }: { value: string; onChange: (value: string) => void; data: { value: string; label: string }[] }) {
  return (
    <Tabs className="manage-tabs-root" value={value} onChange={(next) => next && onChange(next)}>
      <Tabs.List className="manage-tabs">
        {data.map((item) => (
          <Tabs.Tab key={item.value} value={item.value}>
            {item.label}
          </Tabs.Tab>
        ))}
      </Tabs.List>
    </Tabs>
  );
}

function ModalActions({ loading, disabled, onCancel, submitLabel, t }: { loading: boolean; disabled: boolean; disabledReason?: string; onCancel: () => void; submitLabel: string; t: (key: string) => string }) {
  return (
    <Group justify="flex-end" gap="sm">
      <Button variant="default" leftSection={<X size={16} />} onClick={onCancel}>{t("actions.cancel")}</Button>
      <GuardedButton type="submit" leftSection={<Save size={16} />} loading={loading} disabled={disabled}>{submitLabel}</GuardedButton>
    </Group>
  );
}

type GuardedButtonProps = {
  children: React.ReactNode;
  disabled?: boolean;
  disabledReason?: string;
  leftSection?: React.ReactNode;
  loading?: boolean;
  onClick?: () => void | Promise<void>;
  type?: "button" | "submit" | "reset";
  variant?: string;
};

function GuardedButton({ disabled, disabledReason: _disabledReason, children, leftSection, loading = false, variant = "default", ...props }: GuardedButtonProps) {
  return (
    <Button
      {...props}
      aria-busy={loading}
      disabled={disabled || loading}
      leftSection={loading ? <RotateCcw className="button-progress-icon is-spinning" size={16} /> : leftSection}
      variant={variant}
    >
      {children}
    </Button>
  );
}

type GuardedActionIconProps = {
  children: React.ReactNode;
  color?: string;
  disabled?: boolean;
  disabledReason?: string;
  label: string;
  onClick?: () => void | Promise<void>;
  variant?: string;
};

function GuardedActionIcon({ label, disabled, disabledReason, children, ...props }: GuardedActionIconProps) {
  const action = <ActionIcon {...props} disabled={disabled}>{children}</ActionIcon>;
  return (
    <Tooltip label={disabled && disabledReason ? disabledReason : label}>
      <span>{action}</span>
    </Tooltip>
  );
}

function OverrideSwitch({ label, checked, disabled, onChange }: { label: string; checked: boolean; disabled: boolean; onChange: (checked: boolean) => void }) {
  return <Switch className="manage-section-override-switch" label={label} checked={checked} disabled={disabled} onChange={(event) => onChange(event.currentTarget.checked)} />;
}

function NumberField({ label, value, disabled, onChange }: { label: string; value: number; disabled: boolean; onChange: (value: number) => void }) {
  return <NumberInput label={label} value={value} disabled={disabled} min={0} onChange={(value) => onChange(Number(value) || 0)} />;
}

function RuntimeEditor({ value, disabled, onChange, t }: { value: RuntimeSettings; disabled: boolean; onChange: (value: RuntimeSettings) => void; t: (key: string) => string }) {
  return (
    <SimpleGrid className="manage-form-grid manage-form-grid-runtime" cols={{ base: 2, md: 3, lg: 4 }}>
      <NumberField label={t("manage.count")} value={value.count} disabled={disabled} onChange={(next) => onChange({ ...value, count: next })} />
      <NumberField label={t("manage.maxHops")} value={value.max_hops} disabled={disabled} onChange={(next) => onChange({ ...value, max_hops: next })} />
      <NumberField label={t("manage.probeTimeout")} value={value.probe_step_timeout_sec} disabled={disabled} onChange={(next) => onChange({ ...value, probe_step_timeout_sec: next })} />
      <NumberField label={t("manage.maxToolTimeout")} value={value.max_tool_timeout_sec} disabled={disabled} onChange={(next) => onChange({ ...value, max_tool_timeout_sec: next })} />
      <NumberField label={t("manage.httpTimeout")} value={value.http_timeout_sec} disabled={disabled} onChange={(next) => onChange({ ...value, http_timeout_sec: next })} />
      <NumberField label={t("manage.dnsTimeout")} value={value.dns_timeout_sec} disabled={disabled} onChange={(next) => onChange({ ...value, dns_timeout_sec: next })} />
      <NumberField label={t("manage.resolveTimeout")} value={value.resolve_timeout_sec} disabled={disabled} onChange={(next) => onChange({ ...value, resolve_timeout_sec: next })} />
      <NumberField label={t("manage.invokeAttempts")} value={value.outbound_invoke_attempts} disabled={disabled} onChange={(next) => onChange({ ...value, outbound_invoke_attempts: next })} />
      <NumberField label={t("manage.healthInterval")} value={value.outbound_max_health_interval_sec} disabled={disabled} onChange={(next) => onChange({ ...value, outbound_max_health_interval_sec: next })} />
    </SimpleGrid>
  );
}

function SchedulerEditor({ value, disabled, onChange, t }: { value: SchedulerSettings; disabled: boolean; onChange: (value: SchedulerSettings) => void; t: (key: string) => string }) {
  return (
    <SimpleGrid className="manage-form-grid manage-form-grid-scheduler" cols={{ base: 2, md: 3, lg: 4 }}>
      <NumberField label={t("manage.maxInflight")} value={value.max_inflight_per_agent} disabled={disabled} onChange={(next) => onChange({ ...value, max_inflight_per_agent: next })} />
      <NumberField label={t("manage.pollInterval")} value={value.poll_interval_sec} disabled={disabled} onChange={(next) => onChange({ ...value, poll_interval_sec: next })} />
      <NumberField label={t("manage.offlineAfter")} value={value.agent_offline_after_sec} disabled={disabled} onChange={(next) => onChange({ ...value, agent_offline_after_sec: next })} />
    </SimpleGrid>
  );
}

function RateLimitEditor({ value, disabled, onChange, t }: { value: RateLimitConfig; disabled: boolean; onChange: (value: RateLimitConfig) => void; t: (key: string) => string }) {
  return (
    <Stack gap="md">
      <div className="manage-rate-grid">
        <LimitInputs prefix={t("manage.globalLimit")} limit={value.global} disabled={disabled} t={t} onChange={(limit) => onChange({ ...value, global: limit })} />
        <LimitInputs prefix={t("manage.ipLimit")} limit={value.ip} disabled={disabled} t={t} onChange={(limit) => onChange({ ...value, ip: limit })} />
        <CIDRInputs
          prefix={t("manage.cidrLimit")}
          exemptCidrs={value.exempt_cidrs ?? []}
          limit={value.cidr}
          disabled={disabled}
          t={t}
          onChange={(limit) => onChange({ ...value, cidr: limit })}
          onExemptChange={(exempt) => onChange({ ...value, exempt_cidrs: exempt })}
        />
        <LimitInputs prefix={t("manage.geoipLimit")} limit={value.geoip} disabled={disabled} t={t} onChange={(limit) => onChange({ ...value, geoip: limit })} />
      </div>
    </Stack>
  );
}

function LimitInputs({ prefix, limit, disabled, onChange, t }: { prefix: string; limit: { requests_per_minute: number; burst: number }; disabled: boolean; onChange: (limit: { requests_per_minute: number; burst: number }) => void; t: (key: string) => string }) {
  return (
    <div className="manage-tool-policy-card manage-limit-group">
      <Group className="manage-tool-policy-card-header manage-limit-card-header" wrap="nowrap">
        <Text className="manage-tool-policy-name manage-limit-title" fw={700}>{prefix}</Text>
      </Group>
      <div className="manage-tool-policy-card-body">
        <SimpleGrid className="manage-limit-fields" cols={2}>
          <NumberField label={t("manage.requestsPerMinute")} value={limit.requests_per_minute} disabled={disabled} onChange={(next) => onChange({ ...limit, requests_per_minute: next })} />
          <NumberField label={t("manage.burst")} value={limit.burst} disabled={disabled} onChange={(next) => onChange({ ...limit, burst: next })} />
        </SimpleGrid>
      </div>
    </div>
  );
}

function CIDRInputs({ prefix, limit, exemptCidrs, disabled, onChange, onExemptChange, t }: { prefix: string; limit: RateLimitConfig["cidr"]; exemptCidrs: string[]; disabled: boolean; onChange: (limit: RateLimitConfig["cidr"]) => void; onExemptChange: (exempt: string[]) => void; t: (key: string) => string }) {
  return (
    <div className="manage-tool-policy-card manage-limit-group manage-limit-group-cidr">
      <Group className="manage-tool-policy-card-header manage-limit-card-header" wrap="nowrap">
        <Text className="manage-tool-policy-name manage-limit-title" fw={700}>{prefix}</Text>
      </Group>
      <div className="manage-tool-policy-card-body">
        <SimpleGrid className="manage-limit-fields manage-limit-fields-cidr" cols={{ base: 2, md: 5 }}>
          <NumberField label={t("manage.requestsPerMinute")} value={limit.requests_per_minute} disabled={disabled} onChange={(next) => onChange({ ...limit, requests_per_minute: next })} />
          <NumberField label={t("manage.burst")} value={limit.burst} disabled={disabled} onChange={(next) => onChange({ ...limit, burst: next })} />
          <NumberField label="IPv4" value={limit.ipv4_prefix} disabled={disabled} onChange={(next) => onChange({ ...limit, ipv4_prefix: next })} />
          <NumberField label="IPv6" value={limit.ipv6_prefix} disabled={disabled} onChange={(next) => onChange({ ...limit, ipv6_prefix: next })} />
          <TagsInput className="manage-exempt-cidrs" label={t("manage.exemptCidrs")} value={exemptCidrs} disabled={disabled} onChange={onExemptChange} />
        </SimpleGrid>
      </div>
    </div>
  );
}

function ToolPolicySections({ value, disabled, onChange, t }: { value: Partial<Record<Tool, PolicySettings>>; disabled: boolean; onChange: (tool: Tool, value: PolicySettings) => void; t: (key: string) => string }) {
  return (
    <div className="manage-tool-policy-grid">
      {tools.map((tool) => {
        const rawPolicy = value[tool];
        const policy = withPolicyDefaults(tool, rawPolicy);
        return (
          <div className="manage-tool-policy-card" key={tool}>
            <Group className="manage-tool-policy-card-header" justify="space-between" wrap="nowrap">
              <Text className="manage-tool-policy-name" fw={700}>{tool}</Text>
              <Switch className="manage-tool-enable-switch" label={t("manage.enabled")} checked={policy.enabled !== false} disabled={disabled} onChange={(event) => onChange(tool, setPolicyEnabled(rawPolicy, event.currentTarget.checked))} />
            </Group>
            <div className="manage-tool-policy-card-body">
              <ToolPolicyEditor tool={tool} value={policy} disabled={disabled} t={t} onChange={(nextPolicy) => onChange(tool, nextPolicy)} />
            </div>
          </div>
        );
      })}
    </div>
  );
}

function ToolPolicyEditor({ tool, value, disabled, onChange, t }: { tool: Tool; value: PolicySettings; disabled: boolean; onChange: (value: PolicySettings) => void; t: (key: string) => string }) {
  return (
    <ToolArgsEditor
      tool={tool}
      args={effectiveAllowedArgs(tool, value.allowed_args)}
      disabled={disabled || value.enabled === false}
      t={t}
      onChange={(key, pattern) => onChange({ ...value, allowed_args: updateAllowedArg(tool, value.allowed_args, key, pattern) })}
    />
  );
}

function TokenToolPolicySections({ value, disabled, onChange, t }: { value: Partial<Record<Tool, TokenToolScope>>; disabled: boolean; onChange: (tool: Tool, value: TokenToolScope) => void; t: (key: string) => string }) {
  const selectedTools = tools.filter((tool) => value[tool]);
  if (selectedTools.length === 0) {
    return <Text c="dimmed">{t("manage.noTokenTools")}</Text>;
  }
  return (
    <Stack className="manage-tool-sections" gap="lg">
      {selectedTools.map((tool) => {
        const rawScope = value[tool] ?? {};
        const scope = displayTokenToolScope(tool, rawScope);
        return (
          <ManageSection key={tool} title={tool} marker={false}>
            <Stack gap="md">
              <ToolArgsEditor
                tool={tool}
                args={scope.allowed_args ?? {}}
                disabled={disabled}
                t={t}
                onChange={(key, pattern) => onChange(tool, { ...rawScope, allowed_args: updateAllowedArg(tool, rawScope.allowed_args, key, pattern) })}
              />
              <SimpleGrid cols={{ base: 1, md: tool === "dns" ? 1 : 2 }}>
                {tool !== "dns" && (
                  <MultiSelect
                    className="manage-nowrap-multiselect"
                    label={t("form.ipVersion")}
                    data={ipVersionOptions.map((version) => ({ value: String(version), label: version === 0 ? t("form.automatic") : `IPv${version}` }))}
                    value={(scope.ip_versions ?? ipVersionOptions).map(String)}
                    disabled={disabled}
                    clearable={false}
                    onChange={(versions) => onChange(tool, { ...rawScope, ip_versions: normalizeIPVersions(versions) })}
                  />
                )}
                <Select
                  label={t("manage.resolveOnAgent")}
                  data={[
                    { value: "any", label: t("manage.any") },
                    { value: "true", label: t("manage.yes") },
                    { value: "false", label: t("manage.no") }
                  ]}
                  value={scope.resolve_on_agent === undefined ? "any" : String(scope.resolve_on_agent)}
                  disabled={disabled}
                  onChange={(next) => onChange(tool, { ...rawScope, resolve_on_agent: resolveOnAgentValue(next) })}
                />
              </SimpleGrid>
            </Stack>
          </ManageSection>
        );
      })}
    </Stack>
  );
}

function AgentBadges({ agent, t }: { agent: ManagedAgent; t: (key: string) => string }) {
  return (
    <Group gap="xs">
      <AgentStatusBadge status={agent.status} disabled={Boolean(agent.config?.disabled)} t={t} />
    </Group>
  );
}

function OutboundAgentBadges({ agent, node, t }: { agent: ManagedAgent; node: OutboundAgentConfig; t: (key: string) => string }) {
  return (
    <Group gap="xs">
      <AgentStatusBadge status={agent.status} disabled={!node.enabled} t={t} />
      {node.tls?.enabled && <Badge variant="light">{t("manage.mtls")}</Badge>}
    </Group>
  );
}

function AgentStatusBadge({ status, disabled, t }: { status: ManagedAgent["status"]; disabled: boolean; t: (key: string) => string }) {
  const effectiveStatus = effectiveAgentStatus(status, disabled);
  const color = effectiveStatus === "disabled" ? "red" : effectiveStatus === "online" ? "green" : "gray";
  return <Badge color={color}>{statusValueLabel(effectiveStatus, t)}</Badge>;
}

function effectiveAgentStatus(status: ManagedAgent["status"], disabled: boolean): ManagedAgent["status"] | "disabled" {
  return disabled ? "disabled" : status;
}

function LabelBadges({ labels }: { labels?: string[] }) {
  const normalized = uniqueStrings(labels ?? []);
  if (normalized.length === 0) {
    return <Text c="dimmed">-</Text>;
  }
  return (
    <Group gap={6}>
      {normalized.map((label) => (
        <Badge key={label} color={isReservedLabel(label) ? "gray" : "blue"} radius="sm" variant="light">
          {label}
        </Badge>
      ))}
    </Group>
  );
}

function ConfigBadges({ config, t }: { config: LabelConfigSettings; t: (key: string) => string }) {
  const items = labelConfigLabels(config, t);
  if (items.length === 0) {
    return <Text c="dimmed">-</Text>;
  }
  return (
    <Group gap={6}>
      {items.map((label) => (
        <Badge key={label} color="violet" radius="sm" variant="light">
          {label}
        </Badge>
      ))}
    </Group>
  );
}

function TokenToolBadges({ token }: { token: APITokenPermission }) {
  if (token.all) {
    return (
      <Group className="manage-tool-badges" gap={6}>
        <Badge color="green" radius="sm" variant="light">*</Badge>
      </Group>
    );
  }
  return <ToolBadges selected={selectedTools(token.tools)} />;
}

function ToolPolicyBadges({ policies }: { policies?: Partial<Record<Tool, PolicySettings>> }) {
  const selected = selectedTools(policies);
  if (selected.length === 0) {
    return <Text c="dimmed">-</Text>;
  }
  return (
    <Group className="manage-tool-badges" gap={6}>
      {selected.map((tool) => (
        <Badge key={tool} color={policies?.[tool]?.enabled === false ? "red" : "blue"} radius="sm" variant="light">
          {tool}
        </Badge>
      ))}
    </Group>
  );
}

function ToolBadges({ selected }: { selected: Tool[] }) {
  if (selected.length === 0) {
    return <Text c="dimmed">-</Text>;
  }
  return (
    <Group className="manage-tool-badges" gap={6}>
      {selected.map((tool) => (
        <Badge key={tool} color="blue" radius="sm" variant="light">
          {tool}
        </Badge>
      ))}
    </Group>
  );
}

function upsertManagedOutboundAgent(outboundAgents: ManagedAgent[], node: OutboundAgentConfig): ManagedAgent[] {
  const next = outboundAgents.filter((item) => item.id !== node.id);
  next.push(managedOutboundAgentFromConfig(node, outboundAgents.find((item) => item.id === node.id)));
  return next.sort((a, b) => a.id.localeCompare(b.id));
}

type OutboundAgentMissingFields = {
  id: boolean;
  url: boolean;
  header: boolean;
  accessKey: boolean;
  projectID: boolean;
  secret: boolean;
  eventResponse: boolean;
};

function outboundAgentAuthForType(value: string | null, current: OutboundAgentAuth): OutboundAgentAuth {
  switch (value) {
    case "none":
      return { type: "none" };
    case "header":
      return { type: "header", header: current.type === "header" ? current.header : undefined, secret: current.type === "header" ? current.secret : undefined };
    case "huaweicloud_aksk":
      return {
        type: "huaweicloud_aksk",
        access_key: current.type === "huaweicloud_aksk" ? current.access_key : undefined,
        project_id: current.type === "huaweicloud_aksk" ? current.project_id : undefined,
        secret: current.type === "huaweicloud_aksk" ? current.secret : undefined
      };
    default:
      return { type: "bearer", secret: current.type === "bearer" ? current.secret : undefined };
  }
}

function outboundAgentAuthForSave(auth: OutboundAgentAuth): OutboundAgentAuth {
  const secret = auth.secret?.trim() || undefined;
  switch (auth.type) {
    case "none":
      return { type: "none" };
    case "header":
      return { type: "header", header: auth.header?.trim() || undefined, secret };
    case "huaweicloud_aksk":
      return {
        type: "huaweicloud_aksk",
        access_key: auth.access_key?.trim() || undefined,
        project_id: auth.project_id?.trim() || undefined,
        secret
      };
    default:
      return { type: "bearer", secret };
  }
}

function outboundAgentForSave(node: OutboundAgentConfig, headers: Record<string, string>): OutboundAgentConfig {
  return {
    ...node,
    id: node.id.trim(),
    url: node.url.trim(),
    auth: outboundAgentAuthForSave(node.auth),
    headers,
    ...(node.transport === "event" ? { event_response: node.event_response ?? "direct" } : { event_response: undefined })
  };
}

function outboundCredentialScopeEqual(current: OutboundAgentConfig, next: OutboundAgentConfig): boolean {
  const currentAuth = outboundAgentAuthForSave(current.auth);
  const nextAuth = outboundAgentAuthForSave(next.auth);
  const currentTLS = withOutboundAgentDefaults(current).tls;
  const nextTLS = withOutboundAgentDefaults(next).tls;
  return current.transport === next.transport
    && current.url === next.url
    && currentAuth.type === nextAuth.type
    && (currentAuth.header ?? "") === (nextAuth.header ?? "")
    && (currentAuth.access_key ?? "") === (nextAuth.access_key ?? "")
    && (currentAuth.project_id ?? "") === (nextAuth.project_id ?? "")
    && stringRecordEqual(current.headers, next.headers)
    && Boolean(currentTLS?.enabled) === Boolean(nextTLS?.enabled)
    && stringArraysEqual(currentTLS?.ca_files, nextTLS?.ca_files)
    && (currentTLS?.cert_file ?? "") === (nextTLS?.cert_file ?? "")
    && (currentTLS?.key_file ?? "") === (nextTLS?.key_file ?? "")
    && (currentTLS?.server_name ?? "") === (nextTLS?.server_name ?? "");
}

function stringRecordEqual(current?: Record<string, string>, next?: Record<string, string>): boolean {
  const currentEntries = Object.entries(current ?? {});
  const nextRecord = next ?? {};
  return currentEntries.length === Object.keys(nextRecord).length
    && currentEntries.every(([key, value]) => nextRecord[key] === value);
}

function stringArraysEqual(current?: string[], next?: string[]): boolean {
  const currentValues = current ?? [];
  const nextValues = next ?? [];
  return currentValues.length === nextValues.length
    && currentValues.every((value, index) => value === nextValues[index]);
}

function outboundAgentRequiredFieldsMissing(node: OutboundAgentConfig, editingID = "", active = true, existing: OutboundAgentConfig | null = null): OutboundAgentMissingFields {
  if (!active) {
    return { id: false, url: false, header: false, accessKey: false, projectID: false, secret: false, eventResponse: false };
  }
  const canRetainSecret = Boolean(editingID && existing && outboundCredentialScopeEqual(existing, node));
  return {
    id: !editingID && !node.id.trim(),
    url: !node.url.trim(),
    header: node.auth.type === "header" && !node.auth.header?.trim(),
    accessKey: node.auth.type === "huaweicloud_aksk" && !node.auth.access_key?.trim(),
    projectID: node.auth.type === "huaweicloud_aksk" && !node.auth.project_id?.trim(),
    secret: node.auth.type !== "none" && !canRetainSecret && !node.auth.secret?.trim(),
    eventResponse: node.transport === "event" && node.event_response !== "direct" && node.event_response !== "result_field"
  };
}

function managedAgentType(agent: ManagedAgent): ManagedAgent["transport"] {
  return agent.transport;
}

function managedOutboundAgentFromConfig(node: OutboundAgentConfig, previous?: ManagedAgent): ManagedAgent {
  return {
    ...previous,
    id: node.id,
    labels: normalizedManagedLabels(node.id, node.transport, node.labels),
    capabilities: previous?.capabilities ?? [],
    protocols: previous?.protocols ?? 0,
    status: node.enabled ? previous?.status ?? "offline" : "offline",
    last_seen_at: previous?.last_seen_at ?? node.updated_at ?? "",
    created_at: previous?.created_at ?? node.created_at ?? "",
    transport: node.transport,
    config: {
      id: node.id,
      disabled: !node.enabled,
      labels: node.labels,
      created_at: node.created_at,
      updated_at: node.updated_at
    },
    outbound: { ...node }
  };
}

function withSettingsDefaults(settings: ManagedSettings): ManagedSettings {
  const labelConfigs: Record<string, LabelConfigSettings> = Object.fromEntries(Object.entries(settings.label_configs ?? {}).map(([label, config]) => [label, withLabelConfigDefaults(config)]));
  if (!labelConfigs.agent) {
    labelConfigs.agent = {
      runtime: cloneRuntime(),
      scheduler: cloneScheduler(),
      tool_policies: {}
    };
  }
  return {
    ...emptySettings,
    ...settings,
    rate_limit: {
      ...emptyRateLimit,
      ...settings.rate_limit,
      global: { ...emptyRateLimit.global, ...settings.rate_limit?.global },
      ip: { ...emptyRateLimit.ip, ...settings.rate_limit?.ip },
      cidr: { ...emptyRateLimit.cidr, ...settings.rate_limit?.cidr },
      geoip: { ...emptyRateLimit.geoip, ...settings.rate_limit?.geoip },
      exempt_cidrs: settings.rate_limit?.exempt_cidrs ?? []
    },
    api_tokens: settings.api_tokens ?? [],
    register_tokens: settings.register_tokens ?? [],
    label_configs: labelConfigs
  };
}

function withOutboundAgentDefaults(node: OutboundAgentConfig): OutboundAgentConfig {
  return {
    ...emptyOutboundAgent,
    ...node,
    auth: { ...emptyOutboundAgent.auth, ...node.auth },
    headers: node.headers ?? {},
    labels: node.labels ?? [],
    tls: { ...emptyOutboundAgent.tls, ...node.tls }
  };
}

function parseOutboundHeaders(raw: string): Record<string, string> | null {
  try {
    const parsed: unknown = JSON.parse(raw || "{}");
    if (!parsed || Array.isArray(parsed) || typeof parsed !== "object") {
      return null;
    }
    const entries = Object.entries(parsed);
    if (!entries.every(([name, value]) => name.trim() !== "" && typeof value === "string")) {
      return null;
    }
    return Object.fromEntries(entries);
  } catch {
    return null;
  }
}

function formatOutboundHeaders(headers?: Record<string, string>): string {
  return JSON.stringify(headers ?? {}, null, 2);
}

function withAgentConfigDefaults(id: string, cfg?: AgentConfig): AgentConfig {
  return {
    ...emptyAgentConfig,
    ...cfg,
    id,
    transport: "grpc",
    labels: cfg?.labels ?? []
  };
}

function withLabelConfigDefaults(config?: LabelConfigSettings): LabelConfigSettings {
  return {
    ...emptyLabelConfig,
    ...config,
    runtime: config?.runtime ? cloneRuntime(config.runtime) : null,
    scheduler: config?.scheduler ? cloneScheduler(config.scheduler) : null,
    tool_policies: config?.tool_policies ?? {}
  };
}

type LabelSummary = {
  label: string;
  count: number;
  config: LabelConfigSettings;
};

function labelSummaries(agents: ManagedAgent[], configs: Record<string, LabelConfigSettings>): LabelSummary[] {
  const labels = new Set<string>(Object.keys(configs));
  for (const agent of agents) {
    for (const label of agent.labels ?? []) {
      labels.add(label);
    }
  }
  return [...labels]
    .sort((a, b) => labelSortKey(a).localeCompare(labelSortKey(b)))
    .map((label) => ({
      label,
      count: agentIDsForLabel(agents, label).length,
      config: withLabelConfigDefaults(configs[label])
    }));
}

function labelSortKey(label: string): string {
  if (label === "agent") {
    return `0:${label}`;
  }
  if (label === "agent:grpc" || label === "agent:http" || label === "agent:event") {
    return `1:${label}`;
  }
  if (!label.startsWith("id:")) {
    return `2:${label}`;
  }
  return `3:${label}`;
}

function agentIDsForLabel(agents: ManagedAgent[], label: string): string[] {
  return agents.filter((agent) => (agent.labels ?? []).includes(label)).map((agent) => agent.id).sort();
}

function agentIDsForLabels(agents: ManagedAgent[], labels: string[]): string[] {
  const selected = new Set(labels);
  return agents
    .filter((agent) => (agent.labels ?? []).some((label) => selected.has(label)))
    .map((agent) => agent.id)
    .sort();
}

function applyLabelMembership(agents: ManagedAgent[], label: string, selectedIDs: string[]): ManagedAgent[] {
  const selected = new Set(selectedIDs);
  return agents.map((agent) => {
    const labels = customLabels(agent.labels);
    const hasLabel = labels.includes(label);
    let nextLabels = labels;
    if (selected.has(agent.id) && !hasLabel && !isReservedLabel(label)) {
      nextLabels = [...labels, label];
    } else if (!selected.has(agent.id) && hasLabel && !isReservedLabel(label)) {
      nextLabels = labels.filter((item) => item !== label);
    }
    const normalizedLabels = managedLabelsForAgent(agent, nextLabels);
    if (managedAgentType(agent) !== "grpc") {
      const transport = agent.transport === "event" ? "event" : "http";
      const outbound = withOutboundAgentDefaults(agent.outbound ?? { ...emptyOutboundAgent, id: agent.id, transport });
      outbound.labels = nextLabels;
      return { ...agent, labels: normalizedLabels, config: { ...(agent.config ?? { id: agent.id }), labels: nextLabels }, outbound };
    }
    const config = withAgentConfigDefaults(agent.id, agent.config);
    config.labels = nextLabels;
    return { ...agent, labels: normalizedLabels, config };
  });
}

function changedLabelAgents(before: ManagedAgent[], after: ManagedAgent[]): ManagedAgent[] {
  const beforeByID = new Map(before.map((agent) => [agent.id, customLabels(agent.labels).join("\n")]));
  return after.filter((agent) => beforeByID.get(agent.id) !== customLabels(agent.labels).join("\n"));
}

function changedLabelOutboundAgents(before: ManagedAgent[], after: ManagedAgent[]): ManagedAgent[] {
  const beforeByID = new Map(before.map((agent) => [agent.id, customLabels(agent.labels).join("\n")]));
  return after.filter((agent) => beforeByID.get(agent.id) !== customLabels(agent.labels).join("\n"));
}

function customLabels(labels?: string[]): string[] {
  return uniqueStrings((labels ?? []).filter((label) => !isReservedLabel(label)));
}

function managedLabelsForAgent(agent: ManagedAgent, labels: string[]): string[] {
  return normalizedManagedLabels(agent.id, managedAgentType(agent), labels);
}

function normalizedManagedLabels(id: string, transport: ManagedAgent["transport"], labels?: string[]): string[] {
  return uniqueStrings(["agent", `agent:${transport}`, `id:${id}`, ...(labels ?? [])]);
}

function uniqueStrings(values: string[]): string[] {
  const seen = new Set<string>();
  const out: string[] = [];
  for (const value of values) {
    const trimmed = value.trim();
    if (!trimmed || seen.has(trimmed)) {
      continue;
    }
    seen.add(trimmed);
    out.push(trimmed);
  }
  return out;
}

function isReservedLabel(label: string): boolean {
  const trimmed = label.trim();
  return trimmed === "agent" || trimmed === "agent:grpc" || trimmed === "agent:http" || trimmed === "agent:event" || trimmed.startsWith("id:");
}

function isAgentLabel(label: string): boolean {
  return label.trim() === "agent";
}

function firstAvailableLabelName(configs: Record<string, LabelConfigSettings>, agents: ManagedAgent[]): string {
  const existing = new Set<string>([...Object.keys(configs), ...agents.flatMap((agent) => agent.labels ?? [])]);
  if (!existing.has("agent")) {
    return "agent";
  }
  let index = 1;
  while (existing.has(`label-${index}`)) {
    index += 1;
  }
  return `label-${index}`;
}

function normalizeLabelConfig(label: string, config: LabelConfigSettings): LabelConfigSettings {
  return {
    runtime: config.runtime || isAgentLabel(label) ? cloneRuntime(config.runtime) : undefined,
    scheduler: config.scheduler || isAgentLabel(label) ? cloneScheduler(config.scheduler) : undefined,
    tool_policies: config.tool_policies ?? {}
  };
}

function nodeOptionLabel(agent: ManagedAgent): string {
  return `${agent.id} · ${managedAgentType(agent)}`;
}

function labelConfigLabels(config: LabelConfigSettings, t: (key: string) => string): string[] {
  return [
    config.runtime ? t("manage.runtime") : "",
    config.scheduler ? t("manage.scheduler") : ""
  ].filter(Boolean);
}

function withTokenDefaults(token: APITokenPermission): APITokenPermission {
  return {
    ...emptyAPIToken,
    ...token,
    agents: token.agents ?? [],
    denied_agents: token.denied_agents ?? [],
    agent_tags: token.agent_tags ?? [],
    denied_tags: token.denied_tags ?? [],
    tools: token.tools ?? {}
  };
}

function cloneRuntime(runtime?: RuntimeSettings | null): RuntimeSettings {
  return { ...emptyRuntime, ...runtime };
}

function cloneScheduler(scheduler?: SchedulerSettings | null): SchedulerSettings {
  return { ...emptyScheduler, ...scheduler };
}

function normalizeToken(token: APITokenPermission): APITokenPermission {
  const secret = token.secret.trim();
  const base = secret ? { ...token, secret, rotate: token.rotate || undefined } : { ...token, secret: "", rotate: token.rotate || undefined };
  return base.all ? { ...base, schedule_access: "write", manage_access: "write", agents: ["*"], denied_agents: [], agent_tags: [], denied_tags: [], tools: {} } : { ...base, tools: normalizeTokenTools(base.tools ?? {}) };
}

function managedTokenRequest(token: APITokenPermission) {
  return {
    name: token.name,
    all: token.all,
    schedule_access: token.schedule_access,
    manage_access: token.manage_access,
    agents: token.agents,
    denied_agents: token.denied_agents,
    agent_tags: token.agent_tags,
    denied_tags: token.denied_tags,
    tools: token.tools
  };
}

function accessValue(value: string | null): "none" | "read" | "write" {
  return value === "read" || value === "write" ? value : "none";
}

function localizedAccessOptions(t: (key: string) => string) {
  return [
    { value: "none", label: t("manage.accessNone") },
    { value: "read", label: t("manage.accessRead") },
    { value: "write", label: t("manage.accessWrite") }
  ];
}

function accessLabel(value: APITokenPermission["schedule_access"] | APITokenPermission["manage_access"], t: (key: string) => string): string {
  switch (value) {
    case "read":
      return t("manage.accessRead");
    case "write":
      return t("manage.accessWrite");
    default:
      return t("manage.accessNone");
  }
}

function TokenAccessBadges({ token, t }: { token: APITokenPermission; t: (key: string) => string }) {
  if (token.all) {
    return (
      <Group className="manage-token-access" gap={6}>
        <Badge color="green" variant="light">{t("manage.fullAccess")}</Badge>
      </Group>
    );
  }
  return (
    <Group className="manage-token-access" gap={6}>
      <TokenAccessBadge label={t("manage.scheduleAccess")} value={token.schedule_access} t={t} />
      <TokenAccessBadge label={t("manage.manageAccess")} value={token.manage_access} t={t} />
    </Group>
  );
}

function TokenAccessBadge({ label, value, t }: { label: string; value: APITokenPermission["schedule_access"] | APITokenPermission["manage_access"]; t: (key: string) => string }) {
  const normalized = accessValue(value ?? null);
  const color = normalized === "write" ? "green" : normalized === "read" ? "blue" : "gray";
  return <Badge color={color} variant="light">{`${label}: ${accessLabel(normalized, t)}`}</Badge>;
}

function tokenScopeLabel(token: APITokenPermission): string {
  if (token.all) {
    return "*";
  }
  const parts = [
    ...(token.agents ?? []),
    ...(token.denied_agents ?? []).map((agent) => `!${agent}`),
    ...(token.agent_tags ?? []).map((tag) => `tag:${tag}`),
    ...(token.denied_tags ?? []).map((tag) => `!tag:${tag}`)
  ];
  return parts.join(", ") || "-";
}

function statusValueLabel(value: string, t: (key: string) => string): string {
  const key = `statusValues.${value}`;
  const translated = t(key);
  return translated === key ? value : translated;
}

function selectedTools(record?: Partial<Record<Tool, unknown>>): Tool[] {
  return tools.filter((tool) => record?.[tool] !== undefined);
}

function defaultAllowedArgs(tool: Tool): Record<string, string> {
  switch (tool) {
    case "ping":
      return { protocol: "icmp,udp,tcp", port: defaultPortRange };
    case "traceroute":
    case "mtr":
      return { protocol: "icmp,udp,tcp", port: defaultPortRange };
    case "http":
      return { method: "GET,HEAD", http_version: "3,2,1.1" };
    case "dns":
      return { type: "A,AAAA,CNAME,MX,TXT,NS" };
    case "port":
      return { port: defaultPortRange };
  }
}

function withPolicyDefaults(_tool: Tool, policy?: PolicySettings): PolicySettings {
  return {
    ...policy,
    enabled: policy?.enabled ?? true,
    ...(policy?.allowed_args === undefined ? {} : { allowed_args: { ...policy.allowed_args } })
  };
}

function setPolicyEnabled(policy: PolicySettings | undefined, enabled: boolean): PolicySettings {
  return { ...(policy ?? {}), enabled };
}

function displayTokenToolScope(tool: Tool, scope?: TokenToolScope): TokenToolScope {
  return {
    ...scope,
    allowed_args: effectiveAllowedArgs(tool, scope?.allowed_args)
  };
}

function effectiveAllowedArgs(tool: Tool, args?: Record<string, string>): Record<string, string> {
  if (args === undefined) {
    return defaultAllowedArgs(tool);
  }
  return { ...args };
}

function updateAllowedArg(tool: Tool, args: Record<string, string> | undefined, key: string, pattern: string): Record<string, string> {
  const next = args === undefined ? defaultAllowedArgs(tool) : { ...args };
  next[key] = pattern;
  return next;
}

function selectTokenTools(current: Partial<Record<Tool, TokenToolScope>>, selected: Tool[]): Partial<Record<Tool, TokenToolScope>> {
  return Object.fromEntries(selected.map((tool) => [tool, current[tool] ?? {}])) as Partial<Record<Tool, TokenToolScope>>;
}

function normalizeTokenTools(current: Partial<Record<Tool, TokenToolScope>>): Partial<Record<Tool, TokenToolScope>> {
  return Object.fromEntries(Object.entries(current).map(([tool, scope]) => [tool, normalizeTokenToolScope(tool as Tool, scope)])) as Partial<Record<Tool, TokenToolScope>>;
}

function normalizeTokenToolScope(tool: Tool, scope?: TokenToolScope): TokenToolScope {
  const next = scope ?? {};
  return {
    ...next,
    ip_versions: tool === "dns" || sameIPVersions(next.ip_versions, ipVersionOptions) ? undefined : next.ip_versions
  };
}

function sameIPVersions(left?: IPVersion[], right?: IPVersion[]): boolean {
  return JSON.stringify([...(left ?? [])].sort()) === JSON.stringify([...(right ?? [])].sort());
}

function normalizeIPVersions(values: string[]): IPVersion[] | undefined {
  const versions = values.map((value) => Number(value)).filter((value): value is IPVersion => value === 0 || value === 4 || value === 6);
  return versions.length === 0 || sameIPVersions(versions, ipVersionOptions) ? undefined : versions;
}

function resolveOnAgentValue(value: string | null): boolean | undefined {
  if (value === "true") {
    return true;
  }
  if (value === "false") {
    return false;
  }
  return undefined;
}

function ToolArgsEditor({ tool, args, disabled, onChange, t }: { tool: Tool; args: Record<string, string>; disabled: boolean; onChange: (key: string, pattern: string) => void; t: (key: string) => string }) {
  const singleField = tool === "dns" || tool === "port";
  const availableProtocols = tool === "ping" ? pingProtocolOptions : routeProtocolOptions;
  return (
    <SimpleGrid className={singleField ? "manage-tool-args-grid manage-tool-args-grid-single" : "manage-tool-args-grid"} cols={singleField ? 1 : { base: 1, md: 2 }}>
      {(tool === "ping" || tool === "traceroute" || tool === "mtr") && (
        <MultiSelect
          className="manage-nowrap-multiselect"
          label={t("form.protocol")}
          data={availableProtocols.map((protocol) => ({ value: protocol, label: protocol }))}
          value={valuesFromPattern(args.protocol, availableProtocols)}
          disabled={disabled}
          onChange={(selected) => onChange("protocol", patternFromValues(selected, availableProtocols))}
        />
      )}
      {tool === "http" && (
        <MultiSelect
          className="manage-nowrap-multiselect"
          label={t("form.method")}
          data={methodOptions.map((method) => ({ value: method, label: method }))}
          value={valuesFromPattern(args.method, methodOptions)}
          disabled={disabled}
          onChange={(selected) => onChange("method", patternFromValues(selected, methodOptions))}
        />
      )}
      {tool === "http" && (
        <MultiSelect
          className="manage-nowrap-multiselect"
          label={t("form.httpVersion")}
          data={httpVersionOptions.map((version) => ({ value: version, label: formatHTTPVersion(version) }))}
          value={valuesFromPattern(args.http_version, httpVersionOptions)}
          disabled={disabled}
          onChange={(selected) => onChange("http_version", patternFromValues(selected, httpVersionOptions))}
        />
      )}
      {tool === "dns" && (
        <MultiSelect
          className="manage-nowrap-multiselect"
          label={t("form.recordType")}
          data={dnsTypeOptions.map((recordType) => ({ value: recordType, label: recordType }))}
          value={valuesFromPattern(args.type, dnsTypeOptions)}
          disabled={disabled}
          onChange={(selected) => onChange("type", patternFromValues(selected, dnsTypeOptions))}
        />
      )}
      {(tool === "ping" || tool === "traceroute" || tool === "mtr" || tool === "port") && (
        <TextInput
          label={t("manage.allowedPorts")}
          placeholder="1-32768,32769-65535"
          value={portsFromRule(args.port)}
          disabled={disabled}
          onChange={(event) => onChange("port", normalizePortRule(event.currentTarget.value))}
        />
      )}
    </SimpleGrid>
  );
}

function valuesFromPattern(pattern: string | undefined, options: string[]): string[] {
  if (pattern === undefined) {
    return [];
  }
  const values = pattern.split(",").map((value) => value.trim()).filter((value) => options.includes(value));
  return values;
}

function patternFromValues(values: string[], options: string[]): string {
  const selected = values.filter((value) => options.includes(value));
  if (selected.length === options.length) {
    return options.join(",");
  }
  return selected.join(",");
}

function portsFromRule(rule: string | undefined): string {
  if (!rule || rule === defaultPortRange) {
    return "";
  }
  return rule;
}

function normalizePortRule(rule: string): string {
  return rule.trim() || defaultPortRange;
}
