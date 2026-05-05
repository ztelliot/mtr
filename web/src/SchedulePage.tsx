import {
  ActionIcon,
  Alert,
  Button,
  Group,
  Input,
  Loader,
  MultiSelect,
  NumberInput,
  Paper,
  ScrollArea,
  Select,
  Stack,
  Switch,
  Table,
  Text,
  TextInput,
  Tooltip
} from "@mantine/core";
import { notifications } from "@mantine/notifications";
import { AlertTriangle, Pencil, Play, Trash2, X } from "lucide-react";
import { FormEvent, useEffect, useMemo, useRef, useState } from "react";
import { agentLocationProviderLabel, agentRegionLabel, agentSelectLabel, ispProtocolLabel, ispProtocolProviderLabel, ProviderCell, RegionCell, StatusBadge } from "./agentDisplay";
import { ApiClient } from "./api";
import { DynamicFields, targetPlaceholder } from "./dynamicFields";
import { errorMessage } from "./errors";
import { formatBytes, formatDateTime, formatHistoryDateTime, formatInterval, formatMS, formatPercent, formatShortDateTime, formatSpeed } from "./formatters";
import { fetchGeoIPQueued, ipFromDNSRecord, uniqueIPAddresses } from "./geoip";
import { buildCreateJobRequest, defaultFormState, navTools, normalizeTargetForTool, parseHostPort } from "./jobForm";
import { canReadSchedules, canWriteSchedules, formWithPermissionDefaults, ipVersionOptions, localizedFormError, permissionFormError, requiresAgentForTool, resolveOnAgentValue, toolAllowed } from "./permissions";
import { buildMtrRows, buildNodeRows, capableAgents, isFanoutTool } from "./pingRows";
import { collectGeoIPTargets, DNSRecordsCell, MtrResultTable, type GeoIPLookup } from "./resultTables";
import { targetResolvedIP } from "./streamEvents";
import type { Agent, CreateScheduledJobRequest, GeoIPInfo, IPVersion, Job, JobEvent, JobFormState, Permissions, ScheduledJob, ScheduleTarget, ScheduleTargetRequest, Tool } from "./types";

type ScheduleLabelOption = { value: string; label: string };
type Translate = (key: string, options?: Record<string, unknown>) => string;

type ScheduleTimeRange = "1h" | "6h" | "24h" | "7d" | "30d";
type ScheduleMetricKind =
  | "pingDuration"
  | "loss"
  | "portConnect"
  | "httpTotal"
  | "httpDNS"
  | "httpConnect"
  | "httpTLS"
  | "httpFirstByte"
  | "httpDownload"
  | "httpSpeed"
  | "httpBytes";
type ScheduleStatColumn = "last" | "min" | "max" | "mean" | "stdev";

export const __schedulePageTest = {
  agentsForScheduleLabel,
  agentsForScheduleLabels,
  scheduleIPVersionOptions,
  schedulePermissionFormError
};

export function SchedulePage({
  client,
  permissions,
  agents,
  t
}: {
  client: ApiClient | null;
  permissions: Permissions | null;
  agents: Agent[];
  t: (key: string, options?: Record<string, unknown>) => string;
}) {
  const [form, setForm] = useState<JobFormState>(defaultFormState);
  const [name, setName] = useState("");
  const [intervalSeconds, setIntervalSeconds] = useState<number | string>(300);
  const [scheduleLabels, setScheduleLabels] = useState<string[]>(["agent"]);
  const [scheduleTargetIntervals, setScheduleTargetIntervals] = useState<Record<string, number | string>>({});
  const [enabled, setEnabled] = useState(true);
  const [editingScheduleId, setEditingScheduleId] = useState("");
  const [timeRange, setTimeRange] = useState<ScheduleTimeRange>("24h");
  const [compact, setCompact] = useState(false);
  const [schedules, setSchedules] = useState<ScheduledJob[]>([]);
  const [selectedScheduleId, setSelectedScheduleId] = useState("");
  const [history, setHistory] = useState<Job[]>([]);
  const [historyVisibleCount, setHistoryVisibleCount] = useState(5);
  const [selectedJobId, setSelectedJobId] = useState("");
  const [events, setEvents] = useState<JobEvent[]>([]);
  const [historyEventsByJobId, setHistoryEventsByJobId] = useState<Record<string, JobEvent[]>>({});
  const [loadingSchedules, setLoadingSchedules] = useState(false);
  const [loadingHistory, setLoadingHistory] = useState(false);
  const [loadingEvents, setLoadingEvents] = useState(false);
  const [loadingTrend, setLoadingTrend] = useState(false);
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [attemptedSubmit, setAttemptedSubmit] = useState(false);
  const [geoIPByIP, setGeoIPByIP] = useState<Record<string, GeoIPInfo | null>>({});
  const pendingGeoIPRef = useRef<Set<string>>(new Set());
  const historyRequestRef = useRef(0);
  const eventsRequestRef = useRef(0);
  const allowedTools = useMemo(() => navTools.filter((tool) => toolAllowed(permissions, tool)), [permissions]);
  const scheduleWriteAllowed = canWriteSchedules(permissions);
  const routeAgents = useMemo(() => capableAgents(agents, form.tool), [agents, form.tool]);
  const scheduleLabelOptions = useMemo(() => scheduleLabelsForAgents(routeAgents, t), [routeAgents, t]);
  const scheduleIPOptions = useMemo(() => scheduleIPVersionOptions(permissions, form, routeAgents, scheduleLabels, t), [form, permissions, routeAgents, scheduleLabels, t]);
  const selectedSchedule = schedules.find((schedule) => schedule.id === selectedScheduleId);
  const editingSchedule = schedules.find((schedule) => schedule.id === editingScheduleId);
  const isEditingSchedule = Boolean(editingSchedule);
  const selectedJob = history.find((job) => job.id === selectedJobId);
  const durationSeries = useMemo(
    () => buildScheduleMetricSeries(selectedSchedule, history, historyEventsByJobId, agents, "pingDuration", t),
    [agents, history, historyEventsByJobId, selectedSchedule, t]
  );
  const lossSeries = useMemo(
    () => buildScheduleMetricSeries(selectedSchedule, history, historyEventsByJobId, agents, "loss", t),
    [agents, history, historyEventsByJobId, selectedSchedule, t]
  );
  const summarySeries = useMemo(
    () => buildScheduleMetricSeries(selectedSchedule, history, historyEventsByJobId, agents, schedulePrimaryMetricKind(selectedSchedule?.tool), t),
    [agents, history, historyEventsByJobId, selectedSchedule, t]
  );
  const httpMetricCards = useMemo(
    () => httpScheduleMetricCards(t).map((card) => ({
      ...card,
      series: buildScheduleMetricSeries(selectedSchedule, history, historyEventsByJobId, agents, card.kind, t)
    })),
    [agents, history, historyEventsByJobId, selectedSchedule, t]
  );
  const scheduleDNSRows = useMemo(
    () => selectedSchedule?.tool === "dns" ? buildScheduleDNSRows(selectedSchedule, history, historyEventsByJobId, agents) : [],
    [agents, history, historyEventsByJobId, selectedSchedule]
  );
  const scheduleMtrRows = selectedJob && !isFanoutTool(selectedJob.tool) ? buildMtrRows(agents, selectedJob.agent_id, events) : [];
  const scheduleTargetIP = targetResolvedIP(events) || selectedJob?.resolved_target || "";
  const scheduleAgent = agents.find((agent) => agent.id === selectedJob?.agent_id);
  const scheduleGeoIPTargets = useMemo(
    () => {
      if (selectedSchedule?.tool === "dns") {
        return uniqueIPAddresses(scheduleDNSRows.flatMap((row) => (row.records ?? []).map(ipFromDNSRecord)));
      }
      return selectedJob ? collectGeoIPTargets(false, [], scheduleMtrRows, scheduleTargetIP) : [];
    },
    [scheduleDNSRows, scheduleMtrRows, scheduleTargetIP, selectedJob, selectedSchedule?.tool]
  );
  const scheduleTargetDrafts = useMemo(
    () => buildScheduleTargetDrafts(scheduleLabels, intervalSeconds, scheduleTargetIntervals),
    [intervalSeconds, scheduleLabels, scheduleTargetIntervals]
  );
  const intervalError = scheduleTargetDrafts.every((target) => intervalIsValid(target.interval_seconds)) ? null : t("errors.intervalRequired");
  const scheduleTargetError = scheduleTargetFormError(scheduleLabels, scheduleLabelOptions, t);
  const schedulePermissionForm = { ...form, agentId: "" };
  const validationForm = requiresAgentForTool(permissions, form.tool) ? { ...schedulePermissionForm, agentId: schedulePermissionForm.agentId || "schedule-target" } : schedulePermissionForm;
  const formError = localizedFormError(validationForm, permissions, t) || schedulePermissionFormError(form, permissions, routeAgents, scheduleLabels, t);
  const showFormError = attemptedSubmit && (formError || scheduleTargetError || intervalError);

  useEffect(() => {
    if (!allowedTools.length || allowedTools.includes(form.tool)) {
      return;
    }
    setForm((current) => ({ ...current, tool: allowedTools[0], target: normalizeTargetForTool(current.target, allowedTools[0]) }));
  }, [allowedTools, form.tool]);

  useEffect(() => {
    const validValues = new Set(flatScheduleLabelOptions(scheduleLabelOptions).map((option) => option.value));
    setScheduleLabels((current) => {
      const next = current.filter((label) => validValues.has(label));
      if (next.length > 0) {
        return arraysEqual(next, current) ? current : next;
      }
      return validValues.has("agent") ? ["agent"] : [];
    });
  }, [scheduleLabelOptions]);

  useEffect(() => {
    if (form.tool === "dns" || scheduleIPOptions.length === 0 || scheduleIPOptions.some((option) => option.value === String(form.ipVersion))) {
      return;
    }
    updateForm("ipVersion", Number(scheduleIPOptions[0].value) as IPVersion);
  }, [form.ipVersion, form.tool, scheduleIPOptions]);

  useEffect(() => {
    if (!client || !canReadSchedules(permissions)) {
      return;
    }
    void loadSchedules();
  }, [client, permissions?.schedule_access]);

  useEffect(() => {
    if (!selectedScheduleId || !client || !canReadSchedules(permissions)) {
      historyRequestRef.current += 1;
      eventsRequestRef.current += 1;
      setHistory([]);
      setSelectedJobId("");
      setEvents([]);
      setHistoryEventsByJobId({});
      setLoadingHistory(false);
      setLoadingTrend(false);
      setLoadingEvents(false);
      return;
    }
    void loadHistory(selectedScheduleId);
  }, [client, permissions?.schedule_access, selectedScheduleId, timeRange]);

  useEffect(() => {
    setHistoryVisibleCount(5);
  }, [selectedScheduleId, timeRange]);

  useEffect(() => {
    if (!selectedJobId || !client || !selectedJob) {
      setEvents([]);
      return;
    }
    if (selectedSchedule && isRouteTool(selectedSchedule.tool) && loadingHistory) {
      return;
    }
    const cached = historyEventsByJobId[selectedJobId];
    if (cached) {
      setEvents(cached);
      return;
    }
    void loadEvents(selectedJobId);
  }, [client, historyEventsByJobId, loadingHistory, selectedJob, selectedJobId, selectedSchedule?.tool]);

  useEffect(() => {
    if (!selectedSchedule || !isRouteTool(selectedSchedule.tool)) {
      return;
    }
    setSelectedJobId((current) => current && history.some((job) => job.id === current) ? current : history[0]?.id || "");
  }, [history, selectedSchedule?.id, selectedSchedule?.tool]);

  useEffect(() => {
    if (!client || scheduleGeoIPTargets.length === 0) {
      return;
    }
    const missing = scheduleGeoIPTargets.filter((ip) => !(ip in geoIPByIP) && !pendingGeoIPRef.current.has(ip));
    if (!missing.length) {
      return;
    }
    missing.forEach((ip) => pendingGeoIPRef.current.add(ip));
    void fetchGeoIPQueued(missing, (ip) => client.getGeoIP(ip)).then((entries) => {
      setGeoIPByIP((current) => {
        const next = { ...current };
        for (const [ip, info] of entries) {
          next[ip] = info;
        }
        return next;
      });
    }).finally(() => {
      missing.forEach((ip) => pendingGeoIPRef.current.delete(ip));
    });
  }, [client, geoIPByIP, scheduleGeoIPTargets]);

  async function loadSchedules() {
    if (!client) {
      return;
    }
    setLoadingSchedules(true);
    try {
      const next = await client.listSchedules();
      const nextSelectedID = selectedScheduleId && next.some((schedule) => schedule.id === selectedScheduleId) ? selectedScheduleId : "";
      setSchedules(next);
      setSelectedScheduleId(nextSelectedID);
      if (scheduleWriteAllowed) {
        const schedule = next.find((item) => item.id === nextSelectedID);
        if (schedule) {
          fillScheduleEditor(schedule);
        } else if (editingScheduleId && !next.some((item) => item.id === editingScheduleId)) {
          resetScheduleEditor();
        }
      }
      setError(null);
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setLoadingSchedules(false);
    }
  }

  async function loadHistory(scheduleID: string, scheduleOverride?: ScheduledJob) {
    if (!client) {
      return;
    }
    const requestID = ++historyRequestRef.current;
    eventsRequestRef.current += 1;
    const schedule = scheduleOverride ?? schedules.find((item) => item.id === scheduleID);
    setHistory([]);
    setSelectedJobId("");
    setEvents([]);
    setHistoryEventsByJobId({});
    setLoadingEvents(false);
    if (!schedule) {
      setLoadingHistory(false);
      setLoadingTrend(false);
      return;
    }
    setLoadingHistory(true);
    setLoadingTrend(!isRouteTool(schedule.tool));
    try {
      let jobs: Job[];
      let nextEventsByJobId: Record<string, JobEvent[]> = {};
      if (isRouteTool(schedule.tool)) {
        jobs = await client.listScheduleHistory(scheduleID, timeRange);
      } else {
        const summary = await client.listScheduleHistorySummary(scheduleID, timeRange);
        jobs = summary.map((event) => jobFromScheduleSummary(schedule, event));
        nextEventsByJobId = Object.fromEntries(summary.map((event) => [event.job_id, [event]]));
      }
      if (requestID !== historyRequestRef.current) {
        return;
      }
      setHistory(jobs);
      setHistoryEventsByJobId(nextEventsByJobId);
      setSelectedJobId(jobs[0]?.id || "");
      setError(null);
    } catch (err) {
      if (requestID !== historyRequestRef.current) {
        return;
      }
      setError(errorMessage(err));
    } finally {
      if (requestID === historyRequestRef.current) {
        setLoadingHistory(false);
        setLoadingTrend(false);
      }
    }
  }

  async function loadEvents(jobID: string) {
    if (!client) {
      return;
    }
    const requestID = ++eventsRequestRef.current;
    setLoadingEvents(true);
    try {
      const next = await client.listJobEvents(jobID);
      if (requestID !== eventsRequestRef.current) {
        return;
      }
      setEvents(next);
      setHistoryEventsByJobId((current) => ({ ...current, [jobID]: next }));
      setError(null);
    } catch (err) {
      if (requestID !== eventsRequestRef.current) {
        return;
      }
      setError(errorMessage(err));
    } finally {
      if (requestID === eventsRequestRef.current) {
        setLoadingEvents(false);
      }
    }
  }

  function updateForm<Key extends keyof JobFormState>(key: Key, value: JobFormState[Key]) {
    setAttemptedSubmit(false);
    setForm((current) => ({ ...current, [key]: value }));
  }

  function changeScheduleTool(tool: Tool) {
    setAttemptedSubmit(false);
    setForm((current) => ({
      ...current,
      tool,
      agentId: "",
      target: normalizeTargetForTool(current.target, tool)
    }));
  }

  function schedulePayload(): CreateScheduledJobRequest {
    const scheduleTargets = scheduleTargetDrafts.map((target) => ({
      ...target,
      interval_seconds: Number(target.interval_seconds)
    }));
    const requestForm = formWithPermissionDefaults({ ...form, agentId: "" }, permissions);
    const jobRequest = buildCreateJobRequest(requestForm);
    return {
      ...jobRequest,
      name: name.trim() || undefined,
      enabled,
      schedule_targets: scheduleTargets
    };
  }

  function resetScheduleEditor() {
    const nextTool = allowedTools[0] ?? defaultFormState.tool;
    setEditingScheduleId("");
    setName("");
    setIntervalSeconds(300);
    setScheduleLabels(["agent"]);
    setScheduleTargetIntervals({});
    setEnabled(true);
    setAttemptedSubmit(false);
    setForm({ ...defaultFormState, tool: nextTool, target: "" });
  }

  function fillScheduleEditor(schedule: ScheduledJob) {
    setEditingScheduleId(schedule.id);
    setAttemptedSubmit(false);
    setError(null);
    setName(schedule.name ?? "");
    setIntervalSeconds(schedule.interval_seconds);
    fillScheduleTargets(schedule);
    setEnabled(schedule.enabled);
    setForm(scheduleFormState(schedule));
  }

  function fillScheduleTargets(schedule: ScheduledJob) {
    const targets = effectiveScheduleTargets(schedule);
    setScheduleLabels(targets.map((target) => target.label));
    setScheduleTargetIntervals(Object.fromEntries(targets.map((target) => [scheduleTargetKey(target.label), target.interval_seconds])));
  }

  function startEditSchedule(schedule: ScheduledJob) {
    if (!toolAllowed(permissions, schedule.tool)) {
      const message = t("errors.toolNotAllowed", { tool: schedule.tool });
      setError(message);
      notifications.show({ color: "red", title: t("statusValues.error"), message });
      return;
    }
    setSelectedScheduleId(schedule.id);
    fillScheduleEditor(schedule);
  }

  async function deleteSchedule(schedule: ScheduledJob) {
    if (!client || !canWriteSchedules(permissions) || submitting) {
      return;
    }
    const label = schedule.name || scheduleTargetLabel(schedule) || schedule.id;
    if (typeof window !== "undefined" && !window.confirm(t("schedule.deleteConfirm", { name: label }))) {
      return;
    }
    setSubmitting(true);
    try {
      await client.deleteSchedule(schedule.id);
      const nextSchedules = schedules.filter((item) => item.id !== schedule.id);
      const nextSelected = nextSchedules[0];
      setSchedules(nextSchedules);
      if (selectedScheduleId === schedule.id) {
        setSelectedScheduleId(nextSelected?.id || "");
        setHistory([]);
        setSelectedJobId("");
        setEvents([]);
      }
      if (editingScheduleId === schedule.id) {
        if (nextSelected && scheduleWriteAllowed) {
          fillScheduleEditor(nextSelected);
        } else {
          resetScheduleEditor();
        }
      }
      setError(null);
      notifications.show({ color: "green", title: t("schedule.deletedTitle"), message: t("schedule.deletedMessage") });
    } catch (err) {
      const message = errorMessage(err);
      setError(message);
      notifications.show({ color: "red", title: t("statusValues.error"), message });
    } finally {
      setSubmitting(false);
    }
  }

  async function saveSchedule(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    setAttemptedSubmit(true);
    if (!client || !canWriteSchedules(permissions) || formError || scheduleTargetError || intervalError || submitting) {
      return;
    }
    setSubmitting(true);
    try {
      const payload = schedulePayload();
      if (editingScheduleId) {
        const updated = await client.updateSchedule(editingScheduleId, payload);
        setSchedules((current) => current.map((schedule) => schedule.id === updated.id ? updated : schedule));
        setSelectedScheduleId(updated.id);
        setEditingScheduleId(updated.id);
        void loadHistory(updated.id, updated);
        notifications.show({ color: "green", title: t("schedule.updatedTitle"), message: t("schedule.updatedMessage") });
      } else {
        const created = await client.createSchedule(payload);
        setSchedules((current) => [created, ...current.filter((schedule) => schedule.id !== created.id)]);
        setSelectedScheduleId(created.id);
        resetScheduleEditor();
        notifications.show({ color: "green", title: t("schedule.createdTitle"), message: t("schedule.createdMessage") });
      }
      setError(null);
    } catch (err) {
      const message = errorMessage(err);
      setError(message);
      notifications.show({ color: "red", title: t("statusValues.error"), message });
    } finally {
      setSubmitting(false);
    }
  }

  if (permissions && !canReadSchedules(permissions)) {
    return (
      <Alert color="yellow" radius="md">
        {t("errors.scheduleNotAllowed")}
      </Alert>
    );
  }

  return (
    <Stack gap="lg" className="schedule-page">
      {scheduleWriteAllowed && (
        <Paper className="schedule-panel schedule-editor-panel" withBorder>
          <form onSubmit={saveSchedule}>
            <div className={`schedule-form-grid tool-${form.tool}`}>
              <TextInput
                className="schedule-name-field"
                disabled={submitting}
                label={t("schedule.name")}
                value={name}
                onChange={(event) => setName(event.currentTarget.value)}
                placeholder={t("schedule.namePlaceholder")}
              />
              <Select
                className="schedule-tool-field"
                checkIconPosition="left"
                disabled={submitting}
                label={t("schedule.tool")}
                data={allowedTools.map((tool) => ({ value: tool, label: t(`nav.${tool}`) }))}
                value={form.tool}
                onChange={(value) => value && changeScheduleTool(value as Tool)}
              />
              <TextInput
                className="schedule-target-field"
                disabled={submitting}
                label={t("form.target")}
                value={form.target}
                onChange={(event) => updateForm("target", event.currentTarget.value)}
                placeholder={targetPlaceholder(form.tool, t)}
              />
              <NumberInput
                className="schedule-interval-field"
                disabled={submitting}
                label={t("schedule.defaultInterval")}
                min={10}
                max={86400}
                value={intervalSeconds}
                onChange={setIntervalSeconds}
              />
              {form.tool !== "dns" && scheduleIPOptions.length > 1 && (
                <Select
                  className="schedule-ip-field"
                  checkIconPosition="left"
                  disabled={submitting}
                  label={t("form.ipVersion")}
                  data={scheduleIPOptions}
                  value={String(form.ipVersion)}
                  onChange={(value) => updateForm("ipVersion", Number(value ?? 0) as IPVersion)}
                />
              )}
              <DynamicFields form={schedulePermissionForm} permissions={permissions} agents={routeAgents} updateForm={updateForm} disabled={submitting} t={t} />
              <MultiSelect
                className="schedule-label-field"
                checkIconPosition="left"
                clearable
                data={scheduleLabelOptions}
                disabled={submitting}
                label={t("schedule.tags")}
                leftSection={<span className="schedule-label-summary">{scheduleLabels.length > 0 ? t("schedule.selectedCount", { count: scheduleLabels.length }) : t("schedule.tagsPlaceholder")}</span>}
                leftSectionPointerEvents="none"
                leftSectionWidth="calc(100% - 76px)"
                placeholder=""
                renderPill={() => null}
                value={scheduleLabels}
                onChange={setScheduleLabels}
              />
              <div className={`schedule-controls-group ${form.tool === "dns" ? "dns-controls" : ""}`}>
                <div className="schedule-switch-stack">
                  {form.tool !== "dns" && (
                    <Input.Wrapper className="schedule-remote-dns-field schedule-switch-field" label={t("form.remoteDns")}>
                      <Switch
                        aria-label={t("form.remoteDns")}
                        checked={resolveOnAgentValue(permissions, schedulePermissionForm)}
                        className="remote-dns-switch"
                        disabled={submitting || permissions?.tools?.[form.tool]?.resolve_on_agent !== undefined}
                        onChange={(event) => updateForm("resolveOnAgent", event.currentTarget.checked)}
                      />
                    </Input.Wrapper>
                  )}
                  <Input.Wrapper className="schedule-enabled-field schedule-switch-field" label={t("schedule.enabled")}>
                    <Switch checked={enabled} className="schedule-enabled-switch" disabled={submitting} onChange={(event) => setEnabled(event.currentTarget.checked)} />
                  </Input.Wrapper>
                </div>
                <div className="schedule-action-group">
                  {isEditingSchedule && (
                    <>
                      <ActionIcon aria-label={t("schedule.delete")} className="schedule-delete" color="red" disabled={submitting} onClick={() => editingSchedule && void deleteSchedule(editingSchedule)} size={42} title={t("schedule.delete")} type="button" variant="default">
                        <Trash2 size={18} />
                      </ActionIcon>
                      <ActionIcon aria-label={t("actions.cancel")} className="schedule-cancel" disabled={submitting} onClick={resetScheduleEditor} size={42} title={t("actions.cancel")} type="button" variant="default">
                        <X size={18} />
                      </ActionIcon>
                    </>
                  )}
                  <ActionIcon aria-label={submitting ? t("form.running") : isEditingSchedule ? t("schedule.update") : t("schedule.create")} className="schedule-submit" disabled={!client || submitting} size={42} title={submitting ? t("form.running") : isEditingSchedule ? t("schedule.update") : t("schedule.create")} type="submit" variant="default">
                    {submitting ? <Loader size={16} /> : isEditingSchedule ? <Pencil size={18} /> : <Play size={18} />}
                  </ActionIcon>
                </div>
              </div>
            </div>
            {scheduleLabels.length > 1 && (
              <div className="schedule-label-interval-panel">
                <Text className="schedule-label-interval-title">{t("schedule.labelIntervals")}</Text>
                <div className="schedule-group-intervals">
                  {scheduleLabels.map((label) => (
                    <NumberInput
                      key={label}
                      disabled={submitting}
                      label={scheduleLabelDisplay(label, routeAgents, t)}
                      min={10}
                      max={86400}
                      value={scheduleTargetIntervals[scheduleTargetKey(label)] ?? intervalSeconds}
                      onChange={(value) => setScheduleTargetIntervals((current) => ({ ...current, [scheduleTargetKey(label)]: value }))}
                    />
                  ))}
                </div>
              </div>
            )}
          </form>
        </Paper>
      )}

      {error && (
        <Alert color="red" icon={<AlertTriangle size={18} />} radius="md">
          {error}
        </Alert>
      )}
      {showFormError && (
        <Alert color="yellow" radius="md">
          {formError || scheduleTargetError || intervalError}
        </Alert>
      )}

      <Paper className="schedule-panel schedule-list-panel" withBorder>
        <Group justify="space-between" mb="sm">
          <Text className="schedule-section-title">{t("schedule.list")}</Text>
          <Button size="xs" variant="default" onClick={() => void loadSchedules()} disabled={!client || loadingSchedules}>
            {loadingSchedules ? t("status.streamLive") : t("schedule.refresh")}
          </Button>
        </Group>
        <ScheduleList
          agents={agents}
          schedules={schedules}
          selectedID={selectedScheduleId}
          onSelect={(schedule) => scheduleWriteAllowed ? startEditSchedule(schedule) : setSelectedScheduleId(schedule.id)}
          t={t}
        />
      </Paper>

      {selectedSchedule?.tool === "ping" && (
        <SchedulePingDashboard
          durationSeries={durationSeries}
          history={history}
          loading={loadingHistory || loadingTrend}
          lossSeries={lossSeries}
          schedule={selectedSchedule}
          timeRange={timeRange}
          compact={compact}
          onCompactChange={setCompact}
          onTimeRangeChange={setTimeRange}
          t={t}
        />
      )}

      {selectedSchedule?.tool === "http" && (
        <ScheduleHTTPDashboard
          history={history}
          loading={loadingHistory || loadingTrend}
          metrics={httpMetricCards}
          schedule={selectedSchedule}
          timeRange={timeRange}
          compact={compact}
          onCompactChange={setCompact}
          onTimeRangeChange={setTimeRange}
          t={t}
        />
      )}

      {selectedSchedule?.tool === "dns" && (
        <ScheduleDNSHistoryPanel
          geoIPByIP={geoIPByIP}
          loading={loadingHistory || loadingTrend}
          rows={scheduleDNSRows}
          schedule={selectedSchedule}
          timeRange={timeRange}
          compact={compact}
          onCompactChange={setCompact}
          onTimeRangeChange={setTimeRange}
          t={t}
        />
      )}

      {selectedSchedule?.tool === "port" && (
        <ScheduleMetricSummaryPanel
          loading={loadingHistory || loadingTrend}
          schedule={selectedSchedule}
          series={summarySeries}
          timeRange={timeRange}
          compact={compact}
          onCompactChange={setCompact}
          onTimeRangeChange={setTimeRange}
          t={t}
        />
      )}

      {selectedSchedule && isRouteTool(selectedSchedule.tool) && (
        <div className="schedule-dashboard schedule-route-dashboard">
          <ScheduleResultHeader
            agent={agents.find((agent) => agent.id === singleAgentIDFromSchedule(selectedSchedule))}
            runCount={history.length}
            schedule={selectedSchedule}
            showAgentDetails={false}
            timeRange={timeRange}
            compact={compact}
            onCompactChange={setCompact}
            onTimeRangeChange={setTimeRange}
            t={t}
          />
          <div className="schedule-route-layout">
            <Paper className="schedule-panel schedule-history-panel schedule-route-history-panel" withBorder>
              <ScheduleHistory
                agents={agents}
                compact={compact}
                jobs={history}
                loading={loadingHistory}
                selectedID={selectedJobId}
                visibleCount={historyVisibleCount}
                onLoadMore={() => setHistoryVisibleCount((current) => Math.min(current + 20, history.length))}
                onSelect={setSelectedJobId}
                t={t}
              />
            </Paper>
            <Paper withBorder className="result-table schedule-result-panel schedule-route-result-panel">
              {loadingEvents ? (
                <Text c="dimmed" ta="center" py="xl">{t("schedule.loadingResult")}</Text>
              ) : selectedJob ? (
                <MtrResultTable tool={selectedJob.tool} agent={scheduleAgent} targetIP={scheduleTargetIP} rows={scheduleMtrRows} compact={compact} geoIPByIP={geoIPByIP} t={t} />
              ) : (
                <Text c="dimmed" ta="center" py="xl">{t("schedule.noSelectedResult")}</Text>
              )}
            </Paper>
          </div>
        </div>
      )}
    </Stack>
  );
}

interface ScheduleMetricPoint {
  agentId: string;
  agentLabel: string;
  agentTooltipLabel: string;
  jobId: string;
  status: string;
  targetIP?: string;
  timestamp: number;
  value: number;
}

interface ScheduleMetricSeries {
  agentId: string;
  color: string;
  label: string;
  points: ScheduleMetricPoint[];
}

type ScheduleDNSHistoryRow = ReturnType<typeof buildNodeRows>[number] & { jobId: string; runAt?: string; sortKey: string };

const chartColors = ["#60a5fa", "#34d399", "#f59e0b", "#f472b6", "#a78bfa", "#22d3ee", "#f87171", "#c4b5fd"];

function ScheduleResultHeader({
  agent,
  compact = false,
  runCount,
  schedule,
  showAgentDetails = true,
  timeRange,
  onCompactChange,
  onTimeRangeChange,
  t
}: {
  agent?: Agent;
  compact?: boolean;
  runCount: number;
  schedule: ScheduledJob;
  showAgentDetails?: boolean;
  timeRange: ScheduleTimeRange;
  onCompactChange?: (value: boolean) => void;
  onTimeRangeChange: (value: ScheduleTimeRange) => void;
  t: (key: string, options?: Record<string, unknown>) => string;
}) {
  return (
    <Group className="schedule-dashboard-header" justify="space-between" align="flex-end">
      <div>
        <Text className="schedule-section-title">{scheduleHeaderTitle(schedule, t, showAgentDetails ? agent : undefined, showAgentDetails)}</Text>
        <Text c="dimmed" size="sm">
          {`${t("schedule.samples", { count: runCount })} · ${t("schedule.nextRun", { time: formatDateTime(schedule.next_run_at) })}`}
        </Text>
      </div>
      <Group className="schedule-dashboard-controls" gap="md" align="center">
        {onCompactChange && (
          <Switch className="compact-switch" checked={compact} label={t("results.compact")} labelPosition="left" onChange={(event) => onCompactChange(event.currentTarget.checked)} />
        )}
        <Select
          className="schedule-range-select"
          checkIconPosition="left"
          data={scheduleTimeRangeOptions(t)}
          value={timeRange}
          onChange={(value) => onTimeRangeChange((value as ScheduleTimeRange | null) ?? "24h")}
        />
      </Group>
    </Group>
  );
}

function scheduleHeaderTitle(schedule: ScheduledJob, t: Translate, agent?: Agent, showAgentDetails = true): string {
  const label = [schedule.name, scheduleTargetLabel(schedule)].map((part) => part?.trim()).filter(Boolean);
  const base = [...new Set(label)].join(" · ") || schedule.tool;
  const details = scheduleHeaderParameters(schedule, t, agent, showAgentDetails);
  return [base, ...details].filter(Boolean).join(" · ");
}

function scheduleHeaderParameters(schedule: ScheduledJob, t: Translate, agent?: Agent, showAgentDetails = true): string[] {
  const args = schedule.args ?? {};
  const details: string[] = [];
  const protocol = args.protocol?.toUpperCase();
  if (protocol) {
    details.push(protocol);
  }
  if (schedule.tool !== "dns" && schedule.ip_version !== undefined && schedule.ip_version !== 0) {
    details.push(`IPv${schedule.ip_version}`);
  }
  if (schedule.tool === "http" && args.method) {
    details.push(args.method.toUpperCase());
  }
  if (schedule.tool === "dns" && args.type) {
    details.push(args.type.toUpperCase());
  }
  const pinnedAgentID = singleAgentIDFromSchedule(schedule);
  if (showAgentDetails && pinnedAgentID) {
    details.push(agent ? agentLocationProviderLabel(agent, t) : pinnedAgentID);
  }
  return details.filter(Boolean);
}

function singleAgentIDFromSchedule(schedule: ScheduledJob): string {
  const targets = effectiveScheduleTargets(schedule);
  if (targets.length !== 1) {
    return "";
  }
  const label = targets[0].label;
  return label.startsWith("id:") ? label.slice(3) : "";
}

type ScheduleLabelOptionGroup = { group: string; items: ScheduleLabelOption[] };
type ScheduleLabelOptionData = ScheduleLabelOption | ScheduleLabelOptionGroup;

function scheduleLabelsForAgents(agents: Agent[], t: (key: string, options?: Record<string, unknown>) => string): ScheduleLabelOptionData[] {
  const groups = new Set<string>();
  agents.forEach((agent) => {
    (agent.labels ?? []).forEach((label) => {
      const trimmed = label.trim();
      if (trimmed && !isReservedScheduleLabel(trimmed)) {
        groups.add(trimmed);
      }
    });
  });
  const options: ScheduleLabelOptionData[] = [];
  if (agents.length > 0) {
    options.push({ value: "agent", label: scheduleLabelDisplay("agent", agents, t) });
  }
  const groupItems = [...groups].sort().map((label) => ({ value: label, label: scheduleLabelDisplay(label, agents, t) }));
  if (groupItems.length > 0) {
    options.push({ group: t("schedule.groups"), items: groupItems });
  }
  const nodeItems = agents
    .map((agent) => ({ value: `id:${agent.id}`, label: agentSelectLabel(agent, t) }))
    .sort((left, right) => left.label.localeCompare(right.label));
  if (nodeItems.length > 0) {
    options.push({ group: t("schedule.nodes"), items: nodeItems });
  }
  return options;
}

function schedulePermissionFormError(
  form: JobFormState,
  permissions: Permissions | null,
  agents: Agent[],
  labels: string[],
  t: Translate
): string | null {
  const tokenError = permissionFormError({ ...form, agentId: "" }, permissions, agents, t);
  if (tokenError) {
    return tokenError;
  }
  for (const label of labels) {
    const candidates = agentsForScheduleLabel(agents, label);
    if (candidates.length === 0) {
      return t("errors.noAvailableNodes");
    }
    const candidateErrors = candidates.map((agent) => permissionFormError({ ...form, agentId: agent.id }, permissions, agents, t));
    if (candidateErrors.every(Boolean)) {
      return candidateErrors.find(Boolean) ?? t("errors.noAvailableNodes");
    }
  }
  return null;
}

function scheduleIPVersionOptions(
  permissions: Permissions | null,
  form: JobFormState,
  agents: Agent[],
  labels: string[],
  t: Translate
): Array<{ value: string; label: string }> {
  const options = ipVersionOptions(permissions, form.tool);
  if (form.tool === "dns" || labels.length === 0 || options.length === 0) {
    return options;
  }
  return options.filter((option) =>
    schedulePermissionFormError(
      { ...form, ipVersion: Number(option.value) as IPVersion },
      permissions,
      agents,
      labels,
      t
    ) === null
  );
}

function agentsForScheduleLabels(agents: Agent[], labels: string[]): Agent[] {
  const seen = new Set<string>();
  const out: Agent[] = [];
  for (const label of labels) {
    for (const agent of agentsForScheduleLabel(agents, label)) {
      if (!seen.has(agent.id)) {
        seen.add(agent.id);
        out.push(agent);
      }
    }
  }
  return out;
}

function agentsForScheduleLabel(agents: Agent[], label: string): Agent[] {
  const trimmed = label.trim();
  if (!trimmed) {
    return [];
  }
  if (trimmed === "agent") {
    return agents;
  }
  if (trimmed.startsWith("id:")) {
    const agentID = trimmed.slice(3);
    return agents.filter((agent) => agent.id === agentID || agentHasScheduleLabel(agent, trimmed));
  }
  return agents.filter((agent) => agentHasScheduleLabel(agent, trimmed));
}

function agentHasScheduleLabel(agent: Agent, label: string): boolean {
  return (agent.labels ?? []).some((item) => item.trim() === label);
}

function flatScheduleLabelOptions(options: ScheduleLabelOptionData[]): ScheduleLabelOption[] {
  return options.flatMap((option) => "items" in option ? option.items : [option]);
}

function arraysEqual(left: string[], right: string[]): boolean {
  return left.length === right.length && left.every((item, index) => item === right[index]);
}

function isReservedScheduleLabel(label: string): boolean {
  return label === "agent" || label.startsWith("id:");
}

function scheduleLabelDisplay(label: string, agents: Agent[], t: (key: string, options?: Record<string, unknown>) => string): string {
  if (label === "agent") {
    return t("schedule.allNodes");
  }
  const agentID = label.startsWith("id:") ? label.slice(3) : "";
  if (!agentID) {
    return label;
  }
  const agent = agents.find((item) => item.id === agentID);
  return agent ? agentSelectLabel(agent, t) : agentID;
}

function buildScheduleTargetDrafts(
  labels: string[],
  intervalSeconds: number | string,
  intervals: Record<string, number | string>
): ScheduleTargetRequest[] {
  if (labels.length <= 1) {
    return labels.map((label) => ({
      label,
      interval_seconds: Number(intervalSeconds)
    }));
  }
  return labels.map((label) => ({
    label,
    interval_seconds: Number(intervals[scheduleTargetKey(label)] ?? intervalSeconds)
  }));
}

function scheduleTargetKey(label: string): string {
  return label;
}

function intervalIsValid(value: number): boolean {
  return Number.isFinite(value) && value >= 10 && value <= 86400;
}

function scheduleTargetFormError(
  labels: string[],
  options: ScheduleLabelOptionData[],
  t: (key: string, options?: Record<string, unknown>) => string
): string | null {
  if (flatScheduleLabelOptions(options).length === 0) {
    return t("errors.noAvailableNodes");
  }
  if (labels.length === 0) {
    return t("errors.groupRequired");
  }
  return null;
}

function effectiveScheduleTargets(schedule: ScheduledJob): ScheduleTarget[] {
  return schedule.schedule_targets ?? [];
}

function scheduleTargetLabel(schedule: ScheduledJob): string {
  if (schedule.tool === "port" && schedule.args?.port) {
    const host = schedule.target.includes(":") && !schedule.target.startsWith("[") ? `[${schedule.target}]` : schedule.target;
    return `${host}:${schedule.args.port}`;
  }
  return schedule.target;
}

function scheduleNodesLabel(schedule: ScheduledJob, agents: Agent[], t: (key: string, options?: Record<string, unknown>) => string): string {
  const targets = effectiveScheduleTargets(schedule);
  return targets.map((target) => scheduleLabelDisplay(target.label, agents, t)).join(" / ");
}

function ScheduleNodesCell({
  schedule,
  agents,
  t
}: {
  schedule: ScheduledJob;
  agents: Agent[];
  t: (key: string, options?: Record<string, unknown>) => string;
}) {
  const label = scheduleNodesLabel(schedule, agents, t);
  return (
    <Table.Td className="schedule-nodes-column">
      <Tooltip label={label} disabled={!label} multiline withArrow>
        <span className="schedule-nodes-label">{label || "-"}</span>
      </Tooltip>
    </Table.Td>
  );
}

function scheduleIntervalLabel(schedule: ScheduledJob): string {
  const targets = effectiveScheduleTargets(schedule);
  const intervals = [...new Set(targets.map((target) => target.interval_seconds))];
  return intervals.length === 1 ? formatInterval(intervals[0]) : intervals.map(formatInterval).join(" / ");
}

function SchedulePingDashboard({
  compact,
  durationSeries,
  history,
  loading,
  lossSeries,
  schedule,
  timeRange,
  onCompactChange,
  onTimeRangeChange,
  t
}: {
  compact: boolean;
  durationSeries: ScheduleMetricSeries[];
  history: Job[];
  loading: boolean;
  lossSeries: ScheduleMetricSeries[];
  schedule: ScheduledJob;
  timeRange: ScheduleTimeRange;
  onCompactChange: (value: boolean) => void;
  onTimeRangeChange: (value: ScheduleTimeRange) => void;
  t: (key: string, options?: Record<string, unknown>) => string;
}) {
  return (
    <div className="schedule-dashboard">
      <ScheduleResultHeader compact={compact} runCount={history.length} schedule={schedule} timeRange={timeRange} onCompactChange={onCompactChange} onTimeRangeChange={onTimeRangeChange} t={t} />
      <div className="schedule-metrics-grid schedule-ping-grid">
        <ScheduleMetricCard
          columns={["min", "max", "mean", "stdev"]}
          formatValue={formatMS}
          loading={loading}
          compact={compact}
          series={durationSeries}
          title={t("schedule.icmpDuration")}
          tool={schedule.tool}
          t={t}
        />
        <ScheduleMetricCard
          columns={["mean", "max"]}
          formatValue={formatPercent}
          loading={loading}
          compact={compact}
          series={lossSeries}
          title={t("schedule.packetLoss")}
          tool={schedule.tool}
          t={t}
        />
      </div>
    </div>
  );
}

interface ScheduleMetricCardModel {
  columns: ScheduleStatColumn[];
  formatValue: (value?: number) => string;
  kind: ScheduleMetricKind;
  series: ScheduleMetricSeries[];
  title: string;
}

function ScheduleHTTPDashboard({
  compact,
  history,
  loading,
  metrics,
  schedule,
  timeRange,
  onCompactChange,
  onTimeRangeChange,
  t
}: {
  compact: boolean;
  history: Job[];
  loading: boolean;
  metrics: ScheduleMetricCardModel[];
  schedule: ScheduledJob;
  timeRange: ScheduleTimeRange;
  onCompactChange: (value: boolean) => void;
  onTimeRangeChange: (value: ScheduleTimeRange) => void;
  t: (key: string, options?: Record<string, unknown>) => string;
}) {
  const visibleMetrics = loading ? metrics.slice(0, 4) : metrics.filter((metric) => metric.series.some((series) => series.points.length));
  return (
    <div className="schedule-dashboard">
      <ScheduleResultHeader compact={compact} runCount={history.length} schedule={schedule} timeRange={timeRange} onCompactChange={onCompactChange} onTimeRangeChange={onTimeRangeChange} t={t} />
      {visibleMetrics.length ? (
        <div className="schedule-metrics-grid">
          {visibleMetrics.map((metric) => (
            <ScheduleMetricCard
              columns={metric.columns}
              formatValue={metric.formatValue}
              key={metric.kind}
              loading={loading}
              compact={compact}
              series={metric.series}
              title={metric.title}
              tool={schedule.tool}
              t={t}
            />
          ))}
        </div>
      ) : (
        <Paper className="schedule-panel schedule-metric-card" withBorder>
          <Text c="dimmed" ta="center" py="xl">{t("schedule.noTrendData")}</Text>
        </Paper>
      )}
    </div>
  );
}

function ScheduleDNSHistoryPanel({
  compact,
  geoIPByIP,
  loading,
  rows,
  schedule,
  timeRange,
  onCompactChange,
  onTimeRangeChange,
  t
}: {
  compact: boolean;
  geoIPByIP: GeoIPLookup;
  loading: boolean;
  rows: ScheduleDNSHistoryRow[];
  schedule: ScheduledJob;
  timeRange: ScheduleTimeRange;
  onCompactChange: (value: boolean) => void;
  onTimeRangeChange: (value: ScheduleTimeRange) => void;
  t: (key: string, options?: Record<string, unknown>) => string;
}) {
  return (
    <div className="schedule-dashboard">
      <ScheduleResultHeader compact={compact} runCount={rows.length} schedule={schedule} timeRange={timeRange} onCompactChange={onCompactChange} onTimeRangeChange={onTimeRangeChange} t={t} />
      <Paper className="schedule-panel schedule-history-panel schedule-dns-history-panel" withBorder>
        {loading ? (
          <Text c="dimmed" py="lg">{t("schedule.loadingHistory")}</Text>
        ) : rows.length ? (
          <ScrollArea>
            <Table className={`schedule-dns-history-table ${compact ? "compact-schedule-table" : ""}`} striped highlightOnHover verticalSpacing={compact ? 4 : "xs"}>
            <Table.Thead>
              <Table.Tr>
                <Table.Th>{t("results.region")}</Table.Th>
                <Table.Th>{t("results.provider")}</Table.Th>
                <Table.Th>{t("results.records")}</Table.Th>
                <Table.Th>{t("schedule.runAt")}</Table.Th>
                <Table.Th className="dns-history-status-column">{t("results.status")}</Table.Th>
              </Table.Tr>
            </Table.Thead>
            <Table.Tbody>
              {rows.map((row) => (
                <Table.Tr key={`${row.jobId}-${row.agentId}`}>
                  <Table.Td><RegionCell country={row.country} region={row.region} protocols={row.protocols} t={t} /></Table.Td>
                  <Table.Td><ProviderCell provider={row.provider} isp={row.isp} protocols={row.protocols} t={t} /></Table.Td>
                  <Table.Td><DNSRecordsCell records={row.records} geoIPByIP={geoIPByIP} /></Table.Td>
                  <Table.Td title={row.runAt ? formatDateTime(row.runAt) : undefined}>
                    {formatScheduleRunAt(row.runAt, t)}
                  </Table.Td>
                  <Table.Td className="dns-history-status-column"><StatusBadge status={row.status} t={t} /></Table.Td>
                </Table.Tr>
              ))}
            </Table.Tbody>
            </Table>
          </ScrollArea>
        ) : (
          <Text c="dimmed" py="lg">{t("schedule.noHistory")}</Text>
        )}
      </Paper>
    </div>
  );
}

function ScheduleMetricSummaryPanel({
  compact,
  loading,
  schedule,
  series,
  timeRange,
  onCompactChange,
  onTimeRangeChange,
  t
}: {
  compact: boolean;
  loading: boolean;
  schedule: ScheduledJob;
  series: ScheduleMetricSeries[];
  timeRange: ScheduleTimeRange;
  onCompactChange: (value: boolean) => void;
  onTimeRangeChange: (value: ScheduleTimeRange) => void;
  t: (key: string, options?: Record<string, unknown>) => string;
}) {
  return (
    <div className="schedule-dashboard">
      <ScheduleResultHeader compact={compact} runCount={uniqueSchedulePointRuns(series)} schedule={schedule} timeRange={timeRange} onCompactChange={onCompactChange} onTimeRangeChange={onTimeRangeChange} t={t} />
      <ScheduleMetricCard
        columns={["min", "max", "mean", "stdev"]}
        formatValue={scheduleMetricFormatter(schedule.tool)}
        loading={loading}
        compact={compact}
        series={series}
        title={schedule.tool === "port" ? t("results.rtt") : t("schedule.summary")}
        tool={schedule.tool}
        t={t}
      />
    </div>
  );
}

function ScheduleMetricCard({
  columns = ["last", "min", "max", "mean", "stdev"],
  compact,
  formatValue,
  loading,
  series,
  title,
  tool,
  t
}: {
  columns?: ScheduleStatColumn[];
  compact: boolean;
  formatValue: (value?: number) => string;
  loading: boolean;
  series: ScheduleMetricSeries[];
  title: string;
  tool?: Tool;
  t: (key: string, options?: Record<string, unknown>) => string;
}) {
  const points = series.flatMap((item) => item.points);
  return (
    <Paper className="schedule-panel schedule-metric-card" withBorder>
      <Text className="schedule-metric-title">{title}</Text>
      {loading ? (
        <Text c="dimmed" ta="center" py="xl">{t("schedule.loadingTrend")}</Text>
      ) : points.length ? (
        <>
          <ScheduleLineChart compact={compact} formatValue={formatValue} series={series} tool={tool} />
          <ScrollArea>
            <ScheduleMetricStatsTable columns={columns} compact={compact} formatValue={formatValue} series={series} t={t} />
          </ScrollArea>
        </>
      ) : (
        <Text c="dimmed" ta="center" py="xl">{t("schedule.noTrendData")}</Text>
      )}
    </Paper>
  );
}

function ScheduleMetricStatsTable({
  columns,
  compact,
  formatValue,
  series,
  t
}: {
  columns: ScheduleStatColumn[];
  compact: boolean;
  formatValue: (value?: number) => string;
  series: ScheduleMetricSeries[];
  t: (key: string, options?: Record<string, unknown>) => string;
}) {
  const rows = series.map((item) => ({ ...seriesStats(item), color: item.color, label: item.label }));
  return (
    <Table className={`schedule-metric-table stats-${columns.length} ${compact ? "compact-schedule-table" : ""}`} verticalSpacing={compact ? 4 : 6}>
      <Table.Thead>
        <Table.Tr>
          <Table.Th className="name-column">{t("schedule.nameColumn")}</Table.Th>
          {columns.map((column) => (
            <Table.Th key={column}>{t(`schedule.${column}`)}</Table.Th>
          ))}
        </Table.Tr>
      </Table.Thead>
      <Table.Tbody>
        {rows.map((row) => (
          <Table.Tr key={row.label}>
            <Table.Td className="name-column">
              <span className="schedule-chart-legend-item">
                <span className="schedule-chart-legend-swatch" style={{ background: row.color }} />
                <ScheduleAgentLabel label={row.label} />
              </span>
            </Table.Td>
            {columns.map((column) => (
              <Table.Td key={column}>{formatValue(row[column])}</Table.Td>
            ))}
          </Table.Tr>
        ))}
      </Table.Tbody>
    </Table>
  );
}

function ScheduleAgentLabel({ label }: { label: string }) {
  const match = label.match(/^(.*)\s(\[v[46]\])$/);
  if (!match) {
    return <>{label}</>;
  }
  return (
    <span className="schedule-agent-label">
      {match[1]}
      <span className="agent-protocol-suffix">{match[2]}</span>
    </span>
  );
}

function ScheduleLineChart({
  compact,
  formatValue,
  series,
  tool
}: {
  compact: boolean;
  formatValue: (value?: number) => string;
  series: ScheduleMetricSeries[];
  tool?: Tool;
}) {
  const [hover, setHover] = useState<{ label: string; point: ScheduleMetricPoint; x: number; y: number } | null>(null);
  const points = series.flatMap((item) => item.points);
  const timestamps = points.map((point) => point.timestamp);
  const values = points.map((point) => point.value);
  const minX = Math.min(...timestamps);
  const maxX = Math.max(...timestamps);
  const minValue = Math.min(...values);
  const maxValue = Math.max(...values);
  const yPadding = Math.max(1, (maxValue - minValue) * 0.15);
  const minY = Math.max(0, minValue - yPadding);
  const maxY = maxValue + yPadding;
  const width = 960;
  const height = compact ? 210 : 280;
  const padding = compact
    ? { top: 12, right: 18, bottom: 30, left: 64 }
    : { top: 18, right: 24, bottom: 38, left: 74 };
  const plotWidth = width - padding.left - padding.right;
  const plotHeight = height - padding.top - padding.bottom;
  const x = (timestamp: number) => padding.left + (maxX === minX ? plotWidth / 2 : ((timestamp - minX) / (maxX - minX)) * plotWidth);
  const y = (value: number) => padding.top + plotHeight - ((value - minY) / Math.max(1, maxY - minY)) * plotHeight;
  const gridValues = Array.from({ length: 5 }, (_, index) => minY + ((maxY - minY) / 4) * index);
  const showPointMarkers = points.length <= 64;

  return (
    <div className="schedule-chart-shell">
      <svg className="schedule-chart-svg" role="img" viewBox={`0 0 ${width} ${height}`} onPointerLeave={() => setHover(null)}>
        {gridValues.map((value) => {
          const lineY = y(value);
          return (
            <g key={value}>
              <line className="schedule-chart-grid" x1={padding.left} x2={width - padding.right} y1={lineY} y2={lineY} />
              <text className="schedule-chart-label" x={padding.left - 10} y={lineY + 4} textAnchor="end">
                {formatValue(value)}
              </text>
            </g>
          );
        })}
        <line className="schedule-chart-axis" x1={padding.left} x2={width - padding.right} y1={height - padding.bottom} y2={height - padding.bottom} />
        <text className="schedule-chart-label" x={padding.left} y={height - 10}>{formatShortDateTime(minX)}</text>
        <text className="schedule-chart-label" x={width - padding.right} y={height - 10} textAnchor="end">{formatShortDateTime(maxX)}</text>
        {series.map((item) => {
          const sorted = [...item.points].sort((left, right) => left.timestamp - right.timestamp);
          const path = sorted.map((point, index) => `${index === 0 ? "M" : "L"} ${x(point.timestamp).toFixed(2)} ${y(point.value).toFixed(2)}`).join(" ");
          return (
            <g key={item.agentId}>
              <path className="schedule-chart-line" d={path} stroke={item.color} />
              {sorted.map((point) => {
                const tooltipLines = scheduleChartTooltipLines(point.agentTooltipLabel, point, formatValue, tool);
                return (
                  <g key={`${point.jobId}-${point.timestamp}`}>
                    {showPointMarkers && (
                      <circle
                        className="schedule-chart-point"
                        cx={x(point.timestamp)}
                        cy={y(point.value)}
                        fill={item.color}
                        r={compact ? 3 : 3.5}
                      />
                    )}
                    <circle
                      className="schedule-chart-hit-point"
                      cx={x(point.timestamp)}
                      cy={y(point.value)}
                      onPointerEnter={(event) => setHover({ label: item.label, point, x: event.clientX, y: event.clientY })}
                      onPointerMove={(event) => setHover({ label: item.label, point, x: event.clientX, y: event.clientY })}
                      r={compact ? 6 : 7}
                    >
                      <title>{tooltipLines.join(" · ")}</title>
                    </circle>
                  </g>
                );
              })}
            </g>
          );
        })}
      </svg>
      {hover && (
        <div className="schedule-chart-tooltip" style={{ left: hover.x + 12, top: hover.y + 12 }}>
          {scheduleChartTooltipLines(hover.point.agentTooltipLabel, hover.point, formatValue, tool).map((line, index) => (
            <div className={index === 0 ? "schedule-chart-tooltip-title" : undefined} key={`${line}-${index}`}>
              {line}
            </div>
          ))}
        </div>
      )}
    </div>
  );
}

function scheduleChartTooltipLines(
  label: string,
  point: ScheduleMetricPoint,
  formatValue: (value?: number) => string,
  tool?: Tool
): string[] {
  const ip = chartTooltipIP(point.targetIP);
  const httpCode = chartTooltipHTTPCode(tool, point.status);
  const ipLine = tool === "http" && httpCode ? [ip, httpCode].filter(Boolean).join(" · ") : ip;
  const status = chartTooltipStatus(tool, point.status);
  const metricLine = status || formatValue(point.value);
  return [
    label,
    ipLine,
    metricLine,
    formatDateTime(new Date(point.timestamp).toISOString())
  ].filter((line): line is string => Boolean(line));
}

function chartTooltipIP(value: string | undefined): string | undefined {
  const trimmed = value?.trim();
  return trimmed && trimmed !== "-" ? trimmed : undefined;
}

function chartTooltipHTTPCode(tool: Tool | undefined, status: string | undefined): string | undefined {
  if (tool !== "http") {
    return undefined;
  }
  const trimmed = status?.trim();
  return trimmed && /^\d{3}$/.test(trimmed) ? trimmed : undefined;
}

function chartTooltipStatus(tool: Tool | undefined, status: string | undefined): string | undefined {
  const trimmed = status?.trim();
  if (!trimmed || chartTooltipHTTPCode(tool, trimmed)) {
    return undefined;
  }
  const normalized = trimmed.toLowerCase();
  if (normalized === "succeeded" || normalized === "open") {
    return undefined;
  }
  return trimmed;
}

function seriesStats(series: ScheduleMetricSeries): { last?: number; min?: number; max?: number; mean?: number; stdev?: number } {
  const values = series.points.map((point) => point.value);
  if (!values.length) {
    return {};
  }
  const mean = values.reduce((total, value) => total + value, 0) / values.length;
  const variance = values.reduce((total, value) => total + Math.pow(value - mean, 2), 0) / values.length;
  return {
    last: series.points[series.points.length - 1]?.value,
    min: Math.min(...values),
    max: Math.max(...values),
    mean,
    stdev: Math.sqrt(variance)
  };
}

function uniqueSchedulePointRuns(series: ScheduleMetricSeries[]): number {
  return new Set(series.flatMap((item) => item.points.map((point) => point.jobId))).size;
}

function buildScheduleMetricSeries(
  schedule: ScheduledJob | undefined,
  history: Job[],
  eventsByJobId: Record<string, JobEvent[]>,
  agents: Agent[],
  metric: ScheduleMetricKind,
  t: Translate
): ScheduleMetricSeries[] {
  if (!schedule) {
    return [];
  }
  const seriesByAgent = new Map<string, ScheduleMetricPoint[]>();
  for (const job of history) {
    if (job.tool !== schedule.tool) {
      continue;
    }
    const events = eventsByJobId[job.id];
    if (!events) {
      continue;
    }
    const rows = scheduleMetricRows(job, events, agents);
    for (const row of rows) {
      const value = scheduleMetricValue(job.tool, row, metric);
      if (value === undefined) {
        continue;
      }
      const runAt = scheduleRunAt(job);
      if (!runAt) {
        continue;
      }
      const timestamp = new Date(runAt).getTime();
      if (Number.isNaN(timestamp)) {
        continue;
      }
      const agentId = row.agentId || job.agent_id || "default";
      const region = row.region ? agentRegionLabel(row.region, t) : "";
      const agentName = row.region
        ? `${region} · ${ispProtocolLabel({ isp: row.isp, fallback: agentId, protocols: row.protocols, t })}`
        : ispProtocolLabel({ isp: row.isp, fallback: agentId, protocols: row.protocols, t });
      const agentTooltipLabel = row.region
        ? `${region} · ${ispProtocolProviderLabel({ isp: row.isp, provider: row.provider, fallback: agentId, protocols: row.protocols, t })}`
        : ispProtocolProviderLabel({ isp: row.isp, provider: row.provider, fallback: agentId, protocols: row.protocols, t });
      const targetIP = chartTooltipIP(row.ip) || chartTooltipIP(job.resolved_target);
      const point: ScheduleMetricPoint = {
        agentId,
        agentLabel: agentName,
        agentTooltipLabel,
        jobId: job.id,
        status: row.status,
        targetIP,
        timestamp,
        value
      };
      seriesByAgent.set(agentId, [...(seriesByAgent.get(agentId) ?? []), point]);
    }
  }
  return [...seriesByAgent.entries()]
    .map(([agentId, points], index) => ({
      agentId,
      color: chartColors[index % chartColors.length],
      label: points[0]?.agentLabel || agentId,
      points: points.sort((left, right) => left.timestamp - right.timestamp)
    }))
    .sort((left, right) => left.label.localeCompare(right.label, undefined, { sensitivity: "base" }));
}

function scheduleMetricRows(job: Job, events: JobEvent[], agents: Agent[]): ReturnType<typeof buildNodeRows> {
  if (isFanoutTool(job.tool)) {
    return buildNodeRows(job.tool, agents, [job], { [job.id]: events });
  }
  const agent = agents.find((item) => item.id === job.agent_id);
  const hopRows = buildMtrRows(agents, job.agent_id, events);
  const lastHop = hopRows[hopRows.length - 1];
  return lastHop
    ? [{
      ...lastHop,
      agentId: job.agent_id || "default",
      target: job.target,
      provider: agent?.provider,
      isp: agent?.isp,
      protocols: agent?.protocols
    }]
    : [];
}

function buildScheduleDNSRows(
  schedule: ScheduledJob,
  history: Job[],
  eventsByJobId: Record<string, JobEvent[]>,
  agents: Agent[]
): ScheduleDNSHistoryRow[] {
  return history
    .filter((job) => job.tool === schedule.tool)
    .flatMap((job) => {
      const events = eventsByJobId[job.id];
      if (!events) {
        return [];
      }
      return scheduleMetricRows(job, events, agents).map((row) => ({
        ...row,
        jobId: job.id,
        runAt: scheduleRunAt(job),
        sortKey: scheduleDNSRowSortKey(row)
      }));
    })
    .sort(compareScheduleDNSRows);
}

function scheduleDNSRowSortKey(row: ReturnType<typeof buildNodeRows>[number]): string {
  return [row.country, row.region, row.isp, row.provider, row.agentId]
    .map((value) => sortScheduleDNSRowText(value))
    .join("\u0000");
}

function compareScheduleDNSRows(left: ScheduleDNSHistoryRow, right: ScheduleDNSHistoryRow): number {
  return left.sortKey.localeCompare(right.sortKey, undefined, { numeric: true, sensitivity: "base" }) || compareScheduleRunAtDesc(left.runAt, right.runAt);
}

function compareScheduleRunAtDesc(left: string | undefined, right: string | undefined): number {
  return scheduleRunAtTime(right) - scheduleRunAtTime(left);
}

function scheduleRunAtTime(value: string | undefined): number {
  if (!value) {
    return 0;
  }
  const timestamp = new Date(value).getTime();
  return Number.isNaN(timestamp) ? 0 : timestamp;
}

function sortScheduleDNSRowText(value: string | undefined): string {
  const normalized = value?.trim();
  return normalized && normalized !== "-" ? normalized : "\uffff";
}

function scheduleMetricValue(_tool: Tool, row: ReturnType<typeof buildNodeRows>[number], metric: ScheduleMetricKind): number | undefined {
  switch (metric) {
    case "loss":
      return row.lossPct;
    case "pingDuration":
      return row.avgMS ?? row.lastMS ?? row.bestMS ?? row.worstMS;
    case "portConnect":
      return row.connectMS;
    case "httpTotal":
      return row.totalMS;
    case "httpDNS":
      return row.dnsMS;
    case "httpConnect":
      return row.connectMS;
    case "httpTLS":
      return row.tlsMS;
    case "httpFirstByte":
      return row.firstByteMS;
    case "httpDownload":
      return row.downloadMS;
    case "httpSpeed":
      return row.downloadSpeed;
    case "httpBytes":
      return row.bytesDownloaded;
  }
  return undefined;
}

function scheduleMetricFormatter(tool: Tool): (value?: number) => string {
  if (tool === "port") {
    return formatMS;
  }
  return formatMS;
}

function schedulePrimaryMetricKind(tool: Tool | undefined): ScheduleMetricKind {
  if (tool === "port") {
    return "portConnect";
  }
  if (tool === "http") {
    return "httpTotal";
  }
  return "pingDuration";
}

function httpScheduleMetricCards(t: (key: string) => string): Array<Omit<ScheduleMetricCardModel, "series">> {
  const timeColumns: ScheduleStatColumn[] = ["min", "max", "mean", "stdev"];
  return [
    { columns: timeColumns, formatValue: formatMS, kind: "httpTotal", title: t("results.total") },
    { columns: timeColumns, formatValue: formatMS, kind: "httpDNS", title: t("results.dnsTime") },
    { columns: timeColumns, formatValue: formatMS, kind: "httpConnect", title: t("results.connect") },
    { columns: timeColumns, formatValue: formatMS, kind: "httpTLS", title: t("results.tls") },
    { columns: timeColumns, formatValue: formatMS, kind: "httpFirstByte", title: t("results.firstByte") },
    { columns: timeColumns, formatValue: formatMS, kind: "httpDownload", title: t("results.download") },
    { columns: timeColumns, formatValue: formatSpeed, kind: "httpSpeed", title: t("results.speed") },
    { columns: timeColumns, formatValue: formatBytes, kind: "httpBytes", title: t("results.bytes") }
  ];
}

function isRouteTool(tool: Tool | undefined): boolean {
  return tool === "mtr" || tool === "traceroute";
}

function scheduleTimeRangeOptions(t: (key: string) => string): Array<{ value: ScheduleTimeRange; label: string }> {
  return [
    { value: "1h", label: t("schedule.range1h") },
    { value: "6h", label: t("schedule.range6h") },
    { value: "24h", label: t("schedule.range24h") },
    { value: "7d", label: t("schedule.range7d") },
    { value: "30d", label: t("schedule.range30d") }
  ];
}

function ScheduleList({
  agents,
  schedules,
  selectedID,
  onSelect,
  t
}: {
  agents: Agent[];
  schedules: ScheduledJob[];
  selectedID: string;
  onSelect: (schedule: ScheduledJob) => void;
  t: (key: string, options?: Record<string, unknown>) => string;
}) {
  if (!schedules.length) {
    return <Text c="dimmed" py="lg">{t("schedule.empty")}</Text>;
  }
  return (
    <ScrollArea>
      <Table className="schedule-table" highlightOnHover verticalSpacing="sm">
        <Table.Thead>
          <Table.Tr>
            <Table.Th className="name-column">{t("schedule.name")}</Table.Th>
            <Table.Th>{t("schedule.tool")}</Table.Th>
            <Table.Th className="schedule-target-column">{t("form.target")}</Table.Th>
            <Table.Th>{t("schedule.nodes")}</Table.Th>
            <Table.Th>{t("schedule.intervalShort")}</Table.Th>
            <Table.Th>{t("results.status")}</Table.Th>
          </Table.Tr>
        </Table.Thead>
        <Table.Tbody>
          {schedules.map((schedule) => (
            <Table.Tr
              className={schedule.id === selectedID ? "schedule-row selected" : "schedule-row"}
              key={schedule.id}
              onClick={() => onSelect(schedule)}
            >
              <Table.Td className="name-column">{schedule.name || "-"}</Table.Td>
              <Table.Td>{t(`nav.${schedule.tool}`)}</Table.Td>
              <Table.Td className="mono-label schedule-target-column">{scheduleTargetLabel(schedule)}</Table.Td>
              <ScheduleNodesCell schedule={schedule} agents={agents} t={t} />
              <Table.Td>{scheduleIntervalLabel(schedule)}</Table.Td>
              <Table.Td><StatusBadge status={schedule.enabled ? "enabled" : "disabled"} t={t} /></Table.Td>
            </Table.Tr>
          ))}
        </Table.Tbody>
      </Table>
    </ScrollArea>
  );
}

function ScheduleHistory({
  agents,
  compact,
  jobs,
  loading,
  selectedID,
  visibleCount,
  onLoadMore,
  onSelect,
  t
}: {
  agents: Agent[];
  compact: boolean;
  jobs: Job[];
  loading: boolean;
  selectedID: string;
  visibleCount: number;
  onLoadMore: () => void;
  onSelect: (id: string) => void;
  t: (key: string, options?: Record<string, unknown>) => string;
}) {
  if (loading) {
    return <Text c="dimmed" py="lg">{t("schedule.loadingHistory")}</Text>;
  }
  if (!jobs.length) {
    return <Text c="dimmed" py="lg">{t("schedule.noHistory")}</Text>;
  }
  const visibleJobs = jobs.slice(0, visibleCount);
  return (
    <Stack gap={0} m={0}>
      <ScrollArea>
        <Table className={`schedule-table schedule-history-table ${compact ? "compact-schedule-table" : ""}`} highlightOnHover verticalSpacing={compact ? 4 : "xs"}>
          <Table.Thead>
            <Table.Tr>
              <Table.Th className="schedule-history-node-column">{t("schedule.nodes")}</Table.Th>
              <Table.Th className="schedule-history-time-column">{t("schedule.runAt")}</Table.Th>
            </Table.Tr>
          </Table.Thead>
          <Table.Tbody>
            {visibleJobs.map((job) => (
              <Table.Tr
                className={job.id === selectedID ? "schedule-row selected" : "schedule-row"}
                key={job.id}
                onClick={() => onSelect(job.id)}
              >
                <Table.Td className="schedule-history-node-column" title={scheduleHistoryNodeLabel(job, agents, t)}>
                  {scheduleHistoryNodeLabel(job, agents, t)}
                </Table.Td>
                <Table.Td className="schedule-history-time-column" title={scheduleRunAt(job) ? formatDateTime(scheduleRunAt(job)) : undefined}>
                  {formatScheduleRunAt(scheduleRunAt(job), t)}
                </Table.Td>
              </Table.Tr>
            ))}
          </Table.Tbody>
        </Table>
      </ScrollArea>
      {visibleJobs.length < jobs.length && (
        <Button size="xs" variant="default" onClick={onLoadMore} style={{ borderRadius: 0, border: "none", borderTop: "1px solid var(--mantine-color-default-border)" }}>
          {t("schedule.loadMore")}
        </Button>
      )}
    </Stack>
  );
}

function scheduleHistoryNodeLabel(job: Job, agents: Agent[], t: (key: string, options?: Record<string, unknown>) => string): string {
  if (!job.agent_id) {
    return t("schedule.allNodes");
  }
  const agent = agents.find((item) => item.id === job.agent_id);
  return agent ? agentSelectLabel(agent, t) : job.agent_id;
}

function scheduleFormState(schedule: ScheduledJob): JobFormState {
  const args = schedule.args ?? {};
  return {
    ...defaultFormState,
    tool: schedule.tool,
    target: scheduleTargetForForm(schedule),
    ipVersion: (schedule.ip_version ?? 0) as IPVersion,
    agentId: "",
    protocol: args.protocol === "tcp" ? "tcp" : "icmp",
    method: args.method === "GET" ? "GET" : "HEAD",
    dnsType: scheduleDNSType(args.type),
    resolveOnAgent: schedule.resolve_on_agent ?? defaultFormState.resolveOnAgent
  };
}

function scheduleTargetForForm(schedule: ScheduledJob): string {
  if (schedule.tool !== "port") {
    return normalizeTargetForTool(schedule.target, schedule.tool);
  }
  const parsed = parseHostPort(schedule.target);
  const host = parsed?.host || schedule.target;
  const port = schedule.args?.port || parsed?.port || defaultFormState.port;
  const formattedHost = host.includes(":") && !host.startsWith("[") ? `[${host}]` : host;
  return normalizeTargetForTool(`${formattedHost}:${port}`, "port");
}

function jobFromScheduleSummary(schedule: ScheduledJob, event: JobEvent): Job {
  const metric = event.event?.metric ?? {};
  const statusText = metricString(metric.status);
  const exitCode = metricNumber(event.event?.exit_code ?? event.exit_code);
  const startedAt = metricString(metric.started_at);
  const status = isJobStatus(statusText)
    ? statusText
    : exitCode === undefined || exitCode === 0
      ? "succeeded"
      : "failed";
  const createdAt = event.created_at || schedule.last_run_at || schedule.created_at;
  return {
    id: event.job_id,
    scheduled_id: schedule.id,
    scheduled_revision: schedule.revision,
    tool: schedule.tool,
    target: schedule.target,
    resolved_target: metricString(metric.target_ip),
    args: schedule.args,
    ip_version: schedule.ip_version,
    agent_id: event.agent_id,
    resolve_on_agent: schedule.resolve_on_agent,
    status,
    created_at: createdAt,
    started_at: startedAt,
    updated_at: createdAt
  };
}

function scheduleRunAt(job: Job): string | undefined {
  return job.started_at || undefined;
}

function formatScheduleRunAt(value: string | undefined, t: (key: string) => string): string {
  return value ? formatHistoryDateTime(value) : t("statusValues.queued");
}

function metricString(value: unknown): string | undefined {
  return typeof value === "string" && value.trim() ? value : undefined;
}

function metricNumber(value: unknown): number | undefined {
  return typeof value === "number" && Number.isFinite(value) ? value : undefined;
}

function isJobStatus(value: string | undefined): value is Job["status"] {
  return value === "queued" || value === "running" || value === "succeeded" || value === "failed" || value === "canceled";
}

function scheduleDNSType(value: string | undefined): JobFormState["dnsType"] {
  const allowed: JobFormState["dnsType"][] = ["A", "AAAA", "CNAME", "MX", "TXT", "NS"];
  return allowed.includes(value as JobFormState["dnsType"]) ? (value as JobFormState["dnsType"]) : "A";
}
