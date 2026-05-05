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
  TextInput,
  Tooltip
} from "@mantine/core";
import { notifications } from "@mantine/notifications";
import { AlertTriangle, Pencil, Plus, RefreshCw, RotateCcw, Save, Trash2, X } from "lucide-react";
import { FormEvent, useEffect, useMemo, useState } from "react";
import type React from "react";
import { ApiClient } from "./api";
import { errorMessage } from "./errors";
import { formatDateTime } from "./formatters";
import { canReadManage, canWriteManage } from "./permissions";
import type { AgentConfig, APITokenPermission, IPVersion, LabelConfigSettings, ManagedAgent, ManagedAgentLabelUpdate, ManagedSettings, HTTPAgentConfig, Permissions, PolicySettings, RateLimitConfig, RuntimeSettings, SchedulerSettings, TokenToolScope, Tool } from "./types";

const tools: Tool[] = ["ping", "traceroute", "mtr", "http", "dns", "port"];
const protocolOptions = ["icmp", "tcp"];
const methodOptions = ["GET", "HEAD"];
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
  http_invoke_attempts: 3,
  http_max_health_interval_sec: 300
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

const emptyHTTPAgent: HTTPAgentConfig = {
  id: "",
  transport: "http",
  enabled: true,
  base_url: "",
  http_token: "",
  labels: [],
  tls: { enabled: false, ca_files: [], cert_file: "", key_file: "" },
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
  changedLabelAgents,
  customLabels,
  displayTokenToolScope,
  httpAgentRequiredFieldsMissing,
  labelSummaries,
  managedHTTPAgentFromConfig,
  normalizeIPVersions,
  normalizeToken,
  normalizeTokenToolScope,
  setPolicyEnabled,
  selectTokenTools,
  withTokenDefaults
};

export function ManagePage({ client, permissions, t }: { client: ApiClient | null; permissions: Permissions | null; t: (key: string, options?: Record<string, unknown>) => string }) {
  const [settings, setSettings] = useState<ManagedSettings>(emptySettings);
  const [agents, setAgents] = useState<ManagedAgent[]>([]);
  const [httpAgents, setHTTPAgents] = useState<ManagedAgent[]>([]);
  const [agentConfigForm, setAgentConfigForm] = useState<AgentConfig>(emptyAgentConfig);
  const [httpAgentForm, setHTTPAgentForm] = useState<HTTPAgentConfig>(emptyHTTPAgent);
  const [tokenForm, setTokenForm] = useState<APITokenPermission>(emptyAPIToken);
  const [labelForm, setLabelForm] = useState<LabelConfigSettings>(emptyLabelConfig);
  const [labelNodes, setLabelNodes] = useState<string[]>([]);
  const [selectionLabels, setSelectionLabels] = useState<string[]>([]);
  const [editingAgentId, setEditingAgentId] = useState("");
  const [editingHTTPAgentId, setEditingHTTPAgentId] = useState("");
  const [editingTokenIndex, setEditingTokenIndex] = useState<number | null>(null);
  const [editingLabel, setEditingLabel] = useState("");
  const [editingOriginalLabel, setEditingOriginalLabel] = useState("");
  const [agentModalOpen, setAgentModalOpen] = useState(false);
  const [httpAgentModalOpen, setHTTPAgentModalOpen] = useState(false);
  const [tokenModalOpen, setTokenModalOpen] = useState(false);
  const [labelModalOpen, setLabelModalOpen] = useState(false);
  const [savingRegisterTokenIndex, setSavingRegisterTokenIndex] = useState<number | null>(null);
  const [section, setSection] = useState("httpAgents");
  const [loading, setLoading] = useState(false);
  const [savingSettings, setSavingSettings] = useState(false);
  const [savingAgentConfig, setSavingAgentConfig] = useState(false);
  const [savingHTTPAgent, setSavingHTTPAgent] = useState(false);
  const [savingToken, setSavingToken] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const writable = canWriteManage(permissions);
  const enabledHTTPAgents = useMemo(() => httpAgents.filter((agent) => agent.http?.enabled).length, [httpAgents]);
  const enabledAgents = useMemo(() => agents.filter((agent) => !agent.config?.disabled).length, [agents]);
  const allManagedAgents = useMemo(() => [...agents, ...httpAgents].sort((a, b) => a.id.localeCompare(b.id)), [agents, httpAgents]);
  const labelRows = useMemo(() => labelSummaries(allManagedAgents, settings.label_configs ?? {}), [allManagedAgents, settings.label_configs]);
  const nodeOptions = useMemo(() => allManagedAgents.map((agent) => ({ value: agent.id, label: nodeOptionLabel(agent) })), [allManagedAgents]);
  const labelOptions = useMemo(() => labelRows.map((row) => ({ value: row.label, label: `${row.label} (${row.count})` })), [labelRows]);
  const httpAgentMissingFields = httpAgentRequiredFieldsMissing(httpAgentForm, editingHTTPAgentId, httpAgentModalOpen);
  const httpAgentSubmitDisabled = !writable || httpAgentMissingFields.id || httpAgentMissingFields.baseURL || httpAgentMissingFields.token;
  const accessOptions = useMemo(() => localizedAccessOptions(t), [t]);

  useEffect(() => {
    if (client) {
      void load();
    }
  }, [client, permissions, writable]);

  useEffect(() => {
    if (!writable && section === "tokens") {
      setSection("httpAgents");
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
      setHTTPAgents(nextAgents.filter((agent) => managedAgentType(agent) === "http"));
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

  async function saveHTTPAgent(event: FormEvent) {
    event.preventDefault();
    if (!client || !writable) {
      return;
    }
    const id = (editingHTTPAgentId || httpAgentForm.id).trim();
    const baseURL = httpAgentForm.base_url.trim();
    const httpToken = httpAgentForm.http_token?.trim() ?? "";
    if (!id || !baseURL || !httpToken) {
      return;
    }
    setSavingHTTPAgent(true);
    try {
      const payload = { ...httpAgentForm, id, transport: "http" as const, base_url: baseURL, http_token: httpToken };
      const saved = withHTTPAgentDefaults(editingHTTPAgentId ? await client.updateHTTPAgent(editingHTTPAgentId, payload) : await client.createHTTPAgent(payload));
      setHTTPAgents((current) => upsertManagedHTTPAgent(current, saved));
      closeHTTPAgentModal();
      notifications.show({ color: "green", message: t("manage.httpAgentSaved") });
      setError(null);
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setSavingHTTPAgent(false);
    }
  }

  async function deleteHTTPAgent(id: string) {
    if (!client || !writable) {
      return;
    }
    try {
      await client.deleteManagedAgent(id);
      setHTTPAgents((current) => current.filter((agent) => agent.id !== id));
      if (editingHTTPAgentId === id) {
        closeHTTPAgentModal();
      }
      notifications.show({ color: "green", message: t("manage.httpAgentDeleted") });
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
      const response = await client.createManagedRegisterToken({ name: "" });
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

  async function renameRegisterToken(index: number, name: string) {
    if (!client || !writable) {
      return;
    }
    const token = (settings.register_tokens ?? [])[index];
    if (!token?.id) {
      return;
    }
    setSavingRegisterTokenIndex(index);
    try {
      const response = await client.updateManagedRegisterToken(token.id, { name });
      const saved = response.token;
      setSettings((current) => withSettingsDefaults({ ...current, revision: response.revision, updated_at: response.updated_at, register_tokens: (current.register_tokens ?? []).map((item, itemIndex) => itemIndex === index ? saved : item) }));
      notifications.show({ color: "green", message: t("manage.registerTokenSaved") });
      setError(null);
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setSavingRegisterTokenIndex(null);
    }
  }

  async function deleteRegisterToken(index: number) {
    if (!client || !writable) {
      return;
    }
    const token = (settings.register_tokens ?? [])[index];
    if (!token?.id) {
      return;
    }
    setSavingSettings(true);
    try {
      const response = await client.deleteManagedRegisterToken(token.id);
      setSettings((current) => withSettingsDefaults({ ...current, revision: response.revision, updated_at: response.updated_at, register_tokens: response.tokens ?? (current.register_tokens ?? []).filter((_, itemIndex) => itemIndex !== index) }));
      notifications.show({ color: "green", message: t("manage.registerTokenDeleted") });
      setError(null);
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setSavingSettings(false);
    }
  }

  async function rotateRegisterToken(index: number) {
    if (!client || !writable) {
      return;
    }
    const token = (settings.register_tokens ?? [])[index];
    if (!token?.id) {
      return;
    }
    setSavingRegisterTokenIndex(index);
    try {
      const response = await client.rotateManagedRegisterToken(token.id);
      const saved = response.token;
      setSettings((current) => withSettingsDefaults({ ...current, revision: response.revision, updated_at: response.updated_at, register_tokens: (current.register_tokens ?? []).map((item, itemIndex) => itemIndex === index ? saved : item) }));
      notifications.show({ color: "green", message: t("manage.registerTokenRotated") });
      setError(null);
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setSavingRegisterTokenIndex(null);
    }
  }

  function openNewHTTPAgentModal() {
    setEditingHTTPAgentId("");
    setHTTPAgentForm(withHTTPAgentDefaults(emptyHTTPAgent));
    setHTTPAgentModalOpen(true);
  }

  function editHTTPAgent(node: HTTPAgentConfig) {
    setEditingHTTPAgentId(node.id);
    setHTTPAgentForm(withHTTPAgentDefaults(node));
    setHTTPAgentModalOpen(true);
  }

  function closeHTTPAgentModal() {
    setHTTPAgentModalOpen(false);
    setEditingHTTPAgentId("");
    setHTTPAgentForm(withHTTPAgentDefaults(emptyHTTPAgent));
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
    const nextHTTPAgents = applyLabelMembership(httpAgents, normalizedLabel, labelNodes);
    try {
      await persistLabelChanges(nextSettings, nextAgents, nextHTTPAgents, t("manage.labelSaved"));
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
    const nextHTTPAgents = applyLabelMembership(httpAgents, label, []);
    try {
      await persistLabelChanges({ ...settings, label_configs: labelConfigs }, nextAgents, nextHTTPAgents, t("manage.labelDeleted"));
      if (editingLabel === label) {
        closeLabelModal();
      }
    } catch {
      // persistLabelChanges already surfaced the error.
    }
  }

  async function persistLabelChanges(nextSettings: ManagedSettings, nextAgents: ManagedAgent[], nextHTTPAgents: ManagedAgent[], message: string) {
    if (!client || !writable) {
      return;
    }
    setSavingSettings(true);
    try {
      const changedAgents = changedLabelAgents(agents, nextAgents);
      const changedHTTPAgents = changedLabelHTTPAgents(httpAgents, nextHTTPAgents);
      const labelUpdates: ManagedAgentLabelUpdate[] = [
        ...changedAgents.map((agent) => ({ id: agent.id, labels: customLabels(agent.labels) })),
        ...changedHTTPAgents.map((agent) => ({ id: agent.id, labels: customLabels(agent.labels) }))
      ];
      const saved = await client.updateManagedLabelsAndAgents({
        revision: nextSettings.revision,
        label_configs: nextSettings.label_configs ?? {},
        agents: labelUpdates
      });
      const savedAgents = saved.agents.length > 0 ? saved.agents : [...nextAgents, ...nextHTTPAgents];
      setSettings((current) => withSettingsDefaults({ ...current, revision: saved.revision, updated_at: saved.updated_at, label_configs: saved.label_configs ?? {} }));
      setAgents(savedAgents.filter((agent) => managedAgentType(agent) === "grpc"));
      setHTTPAgents(savedAgents.filter((agent) => managedAgentType(agent) === "http"));
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
            <ResponsiveActionLabel full={t("manage.enabledHTTPAgents", { count: enabledHTTPAgents })} compact={t("manage.enabledHTTPAgentsCompact", { count: enabledHTTPAgents })} />
          </Badge>
        </Group>
        <Button
          aria-busy={loading}
          className="manage-refresh-button"
          disabled={loading}
          leftSection={<RotateCcw className={loading ? "manage-refresh-icon is-spinning" : "manage-refresh-icon"} size={16} />}
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
          { value: "httpAgents", label: t("manage.nodesTab") },
          { value: "labels", label: t("manage.labelsTab") },
          ...(writable ? [{ value: "tokens", label: t("manage.tokensTab") }] : []),
          { value: "settings", label: t("manage.rateLimitTab") }
        ]}
      />

      {section === "settings" && (
        <Stack gap="md">
          <Paper withBorder p="md">
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

      {section === "httpAgents" && (
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
              <SectionHeader title={t("manage.httpAgents")} action={<GuardedButton leftSection={<Plus size={16} />} disabled={!writable} disabledReason={t("manage.writeRequired")} onClick={openNewHTTPAgentModal}><ResponsiveActionLabel full={t("manage.addHTTPAgent")} compact={t("actions.add")} /></GuardedButton>} />
              <Table.ScrollContainer minWidth={780}>
                <Table className="manage-table" verticalSpacing="xs">
                  <Table.Thead>
                    <Table.Tr>
                      <Table.Th>{t("manage.id")}</Table.Th>
                      <Table.Th>{t("manage.url")}</Table.Th>
                      <Table.Th>{t("manage.labels")}</Table.Th>
                      <Table.Th>{t("manage.status")}</Table.Th>
                      <Table.Th>{t("manage.updated")}</Table.Th>
                      <Table.Th className="manage-table-actions">{t("schedule.actions")}</Table.Th>
                    </Table.Tr>
                  </Table.Thead>
                  <Table.Tbody>
                    {httpAgents.map((agent) => {
                      const node = withHTTPAgentDefaults(agent.http ?? { ...emptyHTTPAgent, id: agent.id });
                      return (
                        <Table.Tr key={agent.id}>
                          <Table.Td><Text fw={600}>{agent.id}</Text></Table.Td>
                          <Table.Td>{node.base_url}</Table.Td>
                          <Table.Td><LabelBadges labels={agent.labels ?? node.labels} /></Table.Td>
                          <Table.Td><HTTPAgentBadges agent={agent} node={node} t={t} /></Table.Td>
                          <Table.Td>{formatDateTime(node.updated_at || agent.last_seen_at)}</Table.Td>
                          <Table.Td className="manage-table-actions">
                            <Group gap="xs" justify="flex-end">
                              <Tooltip label={t("schedule.edit")}><ActionIcon variant="subtle" onClick={() => editHTTPAgent(node)}><Pencil size={16} /></ActionIcon></Tooltip>
                              <GuardedActionIcon label={t("schedule.delete")} disabled={!writable} disabledReason={t("manage.writeRequired")} color="red" variant="subtle" onClick={() => deleteHTTPAgent(agent.id)}><Trash2 size={16} /></GuardedActionIcon>
                            </Group>
                          </Table.Td>
                        </Table.Tr>
                      );
                    })}
                    {httpAgents.length === 0 && <EmptyRow colSpan={6} label={t("manage.noHTTPAgentConfigs")} />}
                  </Table.Tbody>
                </Table>
              </Table.ScrollContainer>
            </Stack>
          </Paper>
        </Stack>
      )}

      {writable && section === "tokens" && (
        <Stack gap="lg">
            <Paper withBorder p="md">
              <Stack gap="md">
                <SectionHeader title={t("manage.tokens")} action={<GuardedButton leftSection={<Plus size={16} />} disabled={!writable} disabledReason={t("manage.writeRequired")} onClick={openNewTokenModal}><ResponsiveActionLabel full={t("manage.addToken")} compact={t("actions.add")} /></GuardedButton>} />
                <Table.ScrollContainer minWidth={760}>
                  <Table className="manage-table" verticalSpacing="xs">
                    <Table.Thead>
                      <Table.Tr>
                        <Table.Th>{t("manage.tokenName")}</Table.Th>
                        <Table.Th>{t("manage.token")}</Table.Th>
                        <Table.Th>{t("manage.access")}</Table.Th>
                        <Table.Th>{t("manage.scope")}</Table.Th>
                        <Table.Th>{t("manage.tools")}</Table.Th>
                        <Table.Th className="manage-table-actions">{t("schedule.actions")}</Table.Th>
                      </Table.Tr>
                    </Table.Thead>
                    <Table.Tbody>
                      {(settings.api_tokens ?? []).map((token, index) => (
                        <Table.Tr key={`${token.id || token.secret}:${index}`}>
                          <Table.Td>{token.name || token.secret.slice(0, 8) || "-"}</Table.Td>
                          <Table.Td><Text className="manage-token-secret" fw={600}>{token.secret || "********"}</Text></Table.Td>
                          <Table.Td><TokenAccessBadges token={token} t={t} /></Table.Td>
                          <Table.Td>{tokenScopeLabel(token)}</Table.Td>
                          <Table.Td><TokenToolBadges token={token} /></Table.Td>
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

            <Paper withBorder p="md">
              <Stack gap="md">
                <SectionHeader title={t("manage.registerTokenList")} action={<GuardedButton leftSection={<Plus size={16} />} loading={savingSettings} disabled={!writable} disabledReason={t("manage.writeRequired")} onClick={createRegisterToken}><ResponsiveActionLabel full={t("manage.addRegisterToken")} compact={t("actions.add")} /></GuardedButton>} />
                <Table.ScrollContainer minWidth={680}>
                  <Table className="manage-table" verticalSpacing="xs">
                    <Table.Thead>
                      <Table.Tr>
                        <Table.Th>{t("manage.tokenName")}</Table.Th>
                        <Table.Th>{t("manage.token")}</Table.Th>
                        <Table.Th className="manage-table-actions">{t("schedule.actions")}</Table.Th>
                      </Table.Tr>
                    </Table.Thead>
                    <Table.Tbody>
                      {(settings.register_tokens ?? []).map((token, index) => (
                        <Table.Tr key={`${token.id || token.token}:${index}`}>
                          <Table.Td>
                            <RegisterTokenNameInput
                              disabled={!writable || savingRegisterTokenIndex === index}
                              loading={savingRegisterTokenIndex === index}
                              onSave={(name) => renameRegisterToken(index, name)}
                              placeholder={t("manage.tokenName")}
                              value={token.name ?? ""}
                            />
                          </Table.Td>
                          <Table.Td><Text className="manage-token-secret" fw={600}>{token.token || "********"}</Text></Table.Td>
                          <Table.Td className="manage-table-actions">
                            <Group gap="xs" justify="flex-end">
                              <GuardedActionIcon label={t("manage.rotateRegisterToken")} disabled={!writable || savingRegisterTokenIndex === index} disabledReason={t("manage.writeRequired")} variant="subtle" onClick={() => rotateRegisterToken(index)}><RefreshCw size={16} /></GuardedActionIcon>
                              <GuardedActionIcon label={t("schedule.delete")} disabled={!writable} disabledReason={t("manage.writeRequired")} color="red" variant="subtle" onClick={() => deleteRegisterToken(index)}><Trash2 size={16} /></GuardedActionIcon>
                            </Group>
                          </Table.Td>
                        </Table.Tr>
                      ))}
                      {(settings.register_tokens ?? []).length === 0 && <EmptyRow colSpan={3} label={t("manage.noRegisterTokens")} />}
                    </Table.Tbody>
                  </Table>
                </Table.ScrollContainer>
              </Stack>
            </Paper>
        </Stack>
      )}

      {section === "labels" && (
        <Paper withBorder p="md">
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

      <Modal centered size="xl" opened={httpAgentModalOpen} title={editingHTTPAgentId ? t("manage.editHTTPAgent") : t("manage.newHTTPAgent")} onClose={closeHTTPAgentModal}>
        <form className="manage-modal-scope" onSubmit={saveHTTPAgent}>
          <Stack gap="md">
            <Stack className="manage-node-fields" gap="md">
              <SimpleGrid cols={{ base: 1, md: 2 }}>
                <TextInput label={t("manage.id")} value={httpAgentForm.id} disabled={!writable || Boolean(editingHTTPAgentId)} error={httpAgentMissingFields.id} onChange={(event) => setHTTPAgentForm({ ...httpAgentForm, id: event.currentTarget.value })} />
                <TextInput label={t("manage.url")} value={httpAgentForm.base_url} disabled={!writable} error={httpAgentMissingFields.baseURL} onChange={(event) => setHTTPAgentForm({ ...httpAgentForm, base_url: event.currentTarget.value })} />
                <PasswordInput label={t("manage.token")} value={httpAgentForm.http_token ?? ""} disabled={!writable} error={httpAgentMissingFields.token} onChange={(event) => setHTTPAgentForm({ ...httpAgentForm, http_token: event.currentTarget.value })} />
                <Box className="manage-node-labels-field">
                  <TagsInput label={t("manage.labels")} value={httpAgentForm.labels ?? []} disabled={!writable} onChange={(labels) => setHTTPAgentForm({ ...httpAgentForm, labels })} />
                </Box>
              </SimpleGrid>
              <Group justify="space-between">
                <Switch label={t("manage.mtls")} checked={Boolean(httpAgentForm.tls?.enabled)} disabled={!writable} onChange={(event) => setHTTPAgentForm({ ...httpAgentForm, tls: { ...httpAgentForm.tls, enabled: event.currentTarget.checked } })} />
                <Switch className="manage-node-enable-switch" label={t("manage.enabled")} checked={httpAgentForm.enabled} disabled={!writable} onChange={(event) => setHTTPAgentForm({ ...httpAgentForm, enabled: event.currentTarget.checked })} />
              </Group>
              {httpAgentForm.tls?.enabled && (
                <SimpleGrid cols={{ base: 1, md: 3 }}>
                  <TagsInput label={t("manage.caFiles")} value={httpAgentForm.tls?.ca_files ?? []} disabled={!writable} onChange={(caFiles) => setHTTPAgentForm({ ...httpAgentForm, tls: { ...httpAgentForm.tls, ca_files: caFiles } })} />
                  <TextInput label={t("manage.certFile")} value={httpAgentForm.tls?.cert_file ?? ""} disabled={!writable} onChange={(event) => setHTTPAgentForm({ ...httpAgentForm, tls: { ...httpAgentForm.tls, cert_file: event.currentTarget.value } })} />
                  <TextInput label={t("manage.keyFile")} value={httpAgentForm.tls?.key_file ?? ""} disabled={!writable} onChange={(event) => setHTTPAgentForm({ ...httpAgentForm, tls: { ...httpAgentForm.tls, key_file: event.currentTarget.value } })} />
                </SimpleGrid>
              )}
            </Stack>
            <ModalActions loading={savingHTTPAgent} disabled={httpAgentSubmitDisabled} disabledReason={!writable ? t("manage.writeRequired") : t("manage.requiredFields")} onCancel={closeHTTPAgentModal} submitLabel={editingHTTPAgentId ? t("manage.saveHTTPAgent") : t("manage.addHTTPAgent")} t={t} />
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

function RegisterTokenNameInput({ disabled, loading, onSave, placeholder, value }: { disabled: boolean; loading: boolean; onSave: (value: string) => void | Promise<void>; placeholder: string; value: string }) {
  const [draft, setDraft] = useState(value);

  useEffect(() => {
    setDraft(value);
  }, [value]);

  const changed = draft.trim() !== value.trim();

  return (
    <Group className="manage-inline-name" gap="xs" wrap="nowrap">
      <TextInput
        aria-label={placeholder}
        disabled={disabled}
        value={draft}
        placeholder={placeholder}
        onChange={(event) => setDraft(event.currentTarget.value)}
        onKeyDown={(event) => {
          if (event.key === "Enter" && changed && !disabled && !loading) {
            event.preventDefault();
            void onSave(draft);
          }
        }}
      />
      <Button disabled={disabled || !changed} loading={loading} onClick={() => onSave(draft)} size="xs">
        <Save size={14} />
      </Button>
    </Group>
  );
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

function GuardedButton({ disabled, disabledReason: _disabledReason, children, variant = "default", ...props }: GuardedButtonProps) {
  return <Button {...props} disabled={disabled} variant={variant}>{children}</Button>;
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
      <NumberField label={t("manage.invokeAttempts")} value={value.http_invoke_attempts} disabled={disabled} onChange={(next) => onChange({ ...value, http_invoke_attempts: next })} />
      <NumberField label={t("manage.healthInterval")} value={value.http_max_health_interval_sec} disabled={disabled} onChange={(next) => onChange({ ...value, http_max_health_interval_sec: next })} />
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
  return <ToolArgsEditor tool={tool} args={value.allowed_args ?? {}} disabled={disabled || value.enabled === false} t={t} onChange={(allowedArgs) => onChange({ ...value, allowed_args: allowedArgs })} />;
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
              <ToolArgsEditor tool={tool} args={scope.allowed_args ?? {}} disabled={disabled} t={t} onChange={(allowedArgs) => onChange(tool, { ...rawScope, allowed_args: allowedArgs })} />
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
      <Badge color={agent.status === "online" ? "green" : "gray"}>{statusValueLabel(agent.status, t)}</Badge>
      {agent.config?.disabled && <Badge color="red">{t("manage.disabled")}</Badge>}
    </Group>
  );
}

function HTTPAgentBadges({ agent, node, t }: { agent: ManagedAgent; node: HTTPAgentConfig; t: (key: string) => string }) {
  return (
    <Group gap="xs">
      <Badge color={agent.status === "online" ? "green" : "gray"}>{statusValueLabel(agent.status, t)}</Badge>
      {!node.enabled && <Badge color="red">{t("manage.disabled")}</Badge>}
      {node.tls?.enabled && <Badge variant="light">{t("manage.mtls")}</Badge>}
    </Group>
  );
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

function upsertManagedHTTPAgent(httpAgents: ManagedAgent[], node: HTTPAgentConfig): ManagedAgent[] {
  const next = httpAgents.filter((item) => item.id !== node.id);
  next.push(managedHTTPAgentFromConfig(node, httpAgents.find((item) => item.id === node.id)));
  return next.sort((a, b) => a.id.localeCompare(b.id));
}

function httpAgentRequiredFieldsMissing(node: HTTPAgentConfig, editingID = "", active = true): { id: boolean; baseURL: boolean; token: boolean } {
  if (!active) {
    return { id: false, baseURL: false, token: false };
  }
  return {
    id: !editingID && !node.id.trim(),
    baseURL: !node.base_url.trim(),
    token: !node.http_token?.trim()
  };
}

function managedAgentType(agent: ManagedAgent): "grpc" | "http" {
  return agent.transport;
}

function managedHTTPAgentFromConfig(node: HTTPAgentConfig, previous?: ManagedAgent): ManagedAgent {
  return {
    ...previous,
    id: node.id,
    labels: normalizedManagedLabels(node.id, "http", node.labels),
    capabilities: previous?.capabilities ?? [],
    protocols: previous?.protocols ?? 0,
    status: node.enabled ? previous?.status ?? "offline" : "offline",
    last_seen_at: previous?.last_seen_at ?? node.updated_at ?? "",
    created_at: previous?.created_at ?? node.created_at ?? "",
    transport: "http",
    config: {
      id: node.id,
      disabled: !node.enabled,
      labels: node.labels,
      created_at: node.created_at,
      updated_at: node.updated_at
    },
    http: { ...node, transport: "http" }
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

function withHTTPAgentDefaults(node: HTTPAgentConfig): HTTPAgentConfig {
  return {
    ...emptyHTTPAgent,
    ...node,
    transport: "http",
    labels: node.labels ?? [],
    tls: { ...emptyHTTPAgent.tls, ...node.tls }
  };
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
  if (label === "agent:grpc" || label === "agent:http") {
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
    if (managedAgentType(agent) === "http") {
      const http = withHTTPAgentDefaults(agent.http ?? { ...emptyHTTPAgent, id: agent.id });
      http.labels = nextLabels;
      return { ...agent, labels: normalizedLabels, config: { ...(agent.config ?? { id: agent.id }), labels: nextLabels }, http };
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

function changedLabelHTTPAgents(before: ManagedAgent[], after: ManagedAgent[]): ManagedAgent[] {
  const beforeByID = new Map(before.map((agent) => [agent.id, customLabels(agent.labels).join("\n")]));
  return after.filter((agent) => beforeByID.get(agent.id) !== customLabels(agent.labels).join("\n"));
}

function customLabels(labels?: string[]): string[] {
  return uniqueStrings((labels ?? []).filter((label) => !isReservedLabel(label)));
}

function managedLabelsForAgent(agent: ManagedAgent, labels: string[]): string[] {
  return normalizedManagedLabels(agent.id, managedAgentType(agent), labels);
}

function normalizedManagedLabels(id: string, transport: "grpc" | "http", labels?: string[]): string[] {
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
  return trimmed === "agent" || trimmed === "agent:grpc" || trimmed === "agent:http" || trimmed.startsWith("id:");
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
    case "traceroute":
    case "mtr":
      return { protocol: "icmp,tcp", port: defaultPortRange };
    case "http":
      return { method: "GET,HEAD" };
    case "dns":
      return { type: "A,AAAA,CNAME,MX,TXT,NS" };
    case "port":
      return { port: defaultPortRange };
  }
}

function withPolicyDefaults(tool: Tool, policy?: PolicySettings): PolicySettings {
  return {
    enabled: policy?.enabled ?? true,
    allowed_args: { ...defaultAllowedArgs(tool), ...(policy?.allowed_args ?? {}) }
  };
}

function setPolicyEnabled(policy: PolicySettings | undefined, enabled: boolean): PolicySettings {
  return { ...(policy ?? {}), enabled };
}

function displayTokenToolScope(tool: Tool, scope?: TokenToolScope): TokenToolScope {
  return {
    ...scope,
    allowed_args: { ...defaultAllowedArgs(tool), ...(scope?.allowed_args ?? {}) }
  };
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

function ToolArgsEditor({ tool, args, disabled, onChange, t }: { tool: Tool; args: Record<string, string>; disabled: boolean; onChange: (value: Record<string, string>) => void; t: (key: string) => string }) {
  const singleField = tool === "dns" || tool === "port" || tool === "http";
  return (
    <SimpleGrid className={singleField ? "manage-tool-args-grid manage-tool-args-grid-single" : "manage-tool-args-grid"} cols={singleField ? 1 : { base: 1, md: 2 }}>
      {(tool === "ping" || tool === "traceroute" || tool === "mtr") && (
        <MultiSelect
          className="manage-nowrap-multiselect"
          label={t("form.protocol")}
          data={protocolOptions.map((protocol) => ({ value: protocol, label: protocol }))}
          value={valuesFromPattern(args.protocol, protocolOptions)}
          disabled={disabled}
          onChange={(selected) => onChange({ ...args, protocol: patternFromValues(selected, protocolOptions) })}
        />
      )}
      {tool === "http" && (
        <MultiSelect
          className="manage-nowrap-multiselect"
          label={t("form.method")}
          data={methodOptions.map((method) => ({ value: method, label: method }))}
          value={valuesFromPattern(args.method, methodOptions)}
          disabled={disabled}
          onChange={(selected) => onChange({ ...args, method: patternFromValues(selected, methodOptions) })}
        />
      )}
      {tool === "dns" && (
        <MultiSelect
          className="manage-nowrap-multiselect"
          label={t("form.recordType")}
          data={dnsTypeOptions.map((recordType) => ({ value: recordType, label: recordType }))}
          value={valuesFromPattern(args.type, dnsTypeOptions)}
          disabled={disabled}
          onChange={(selected) => onChange({ ...args, type: patternFromValues(selected, dnsTypeOptions) })}
        />
      )}
      {(tool === "ping" || tool === "traceroute" || tool === "mtr" || tool === "port") && (
        <TextInput
          label={t("manage.allowedPorts")}
          placeholder="1-32768,32769-65535"
          value={portsFromRule(args.port)}
          disabled={disabled}
          onChange={(event) => onChange({ ...args, port: normalizePortRule(event.currentTarget.value) })}
        />
      )}
    </SimpleGrid>
  );
}

function valuesFromPattern(pattern: string | undefined, options: string[]): string[] {
  if (!pattern) {
    return options;
  }
  const values = pattern.split(",").map((value) => value.trim()).filter((value) => options.includes(value));
  return values.length > 0 ? values : options;
}

function patternFromValues(values: string[], options: string[]): string {
  const selected = values.filter((value) => options.includes(value));
  if (selected.length === 0 || selected.length === options.length) {
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
