import {
  Alert,
  Anchor,
  Badge,
  Box,
  Button,
  Container,
  Group,
  Loader,
  Modal,
  Paper,
  PasswordInput,
  Select,
  Stack,
  Switch,
  Tabs,
  Text,
  TextInput,
  Title,
  useComputedColorScheme,
  useMantineColorScheme
} from "@mantine/core";
import { notifications } from "@mantine/notifications";
import { AlertTriangle, GitBranch, Globe2, Moon, Play, Sun, Wifi } from "lucide-react";
import { FormEvent, useEffect, useMemo, useRef, useState } from "react";
import type React from "react";
import { useTranslation } from "react-i18next";
import { agentSelectLabel } from "./agentDisplay";
import { ApiClient } from "./api";
import { loadConfig, saveStoredApiToken } from "./config";
import { DynamicFields, RemoteDNSSwitch, targetPlaceholder } from "./dynamicFields";
import { errorMessage } from "./errors";
import { formatDateTime, formatServerVersion } from "./formatters";
import { normalizeIPAddress } from "./geoip";
import { jobEventFailureType, shouldSuppressFanoutNodeFailure } from "./jobFailures";
import { buildCreateJobRequest, defaultFormState, formStateFromJob, formStateFromLocation, formStatePath, jobResultPath, locationHasExplicitTarget, locationHasExplicitTool, navTools, normalizeTargetForTool } from "./jobForm";
import { jobHasTerminalEvent } from "./jobStatus";
import { setLanguage, supportedLanguages, type SupportedLanguage } from "./i18n";
import { canReadSchedules, dnsTypeOptions, filterAgentsByPermissions, formWithPermissionDefaults, httpMethodOptions, ipVersionOptions, localizedFormError, permissionFormError, protocolOptions, requiresAgentForTool, toolAllowed } from "./permissions";
import { buildMtrRows, buildNodeRows, capableAgents, isFanoutTool } from "./pingRows";
import { collectGeoIPTargets, MtrResultTable, NodeResultTable } from "./resultTables";
import { SchedulePage } from "./SchedulePage";
import { jobEventFromStreamMessage, mergeEvent, targetResolvedIP } from "./streamEvents";
import type { Agent, GeoIPInfo, IPVersion, Job, JobEvent, JobFormState, Permissions, RuntimeConfig, Tool, VersionInfo } from "./types";
import { appVersionLabel } from "./version";

const streamErrorStatusFallbackMS = 4000;
const streamNames = ["message", "progress", "target_resolved", "target_blocked", "unsupported_tool", "unsupported_protocol", "job_timeout", "hop", "hop_summary", "metric", "summary", "completed", "succeeded", "failed", "canceled", "stderr", "stdout"];
type AppPage = "diagnostics" | "schedules";

export function App() {
  const { t, i18n } = useTranslation();
  const { colorScheme, setColorScheme } = useMantineColorScheme();
  const computedColorScheme = useComputedColorScheme("light");
  const [config, setConfig] = useState<RuntimeConfig | null>(null);
  const [agents, setAgents] = useState<Agent[]>([]);
  const [permissions, setPermissions] = useState<Permissions | null>(null);
  const [form, setForm] = useState<JobFormState>(() =>
    typeof window === "undefined" ? defaultFormState : formStateFromLocation(window.location)
  );
  const [page, setPage] = useState<AppPage>(() => (typeof window === "undefined" ? "diagnostics" : pageFromLocation(window.location)));
  const [routeJobID, setRouteJobID] = useState(() => (typeof window === "undefined" ? "" : jobIDFromLocation(window.location)));
  const [activeJobs, setActiveJobs] = useState<Job[]>([]);
  const [eventsByJobId, setEventsByJobId] = useState<Record<string, JobEvent[]>>({});
  const [loadingConfig, setLoadingConfig] = useState(true);
  const [loadingAgents, setLoadingAgents] = useState(false);
  const [submitting, setSubmitting] = useState(false);
  const [streaming, setStreaming] = useState(false);
  const [compact, setCompact] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [attemptedSubmit, setAttemptedSubmit] = useState(false);
  const [permissionsLoadFailed, setPermissionsLoadFailed] = useState(false);
  const [geoIPByIP, setGeoIPByIP] = useState<Record<string, GeoIPInfo | null>>({});
  const [serverVersion, setServerVersion] = useState<VersionInfo | null>(null);
  const [tokenDialogOpen, setTokenDialogOpen] = useState(false);
  const [apiTokenDraft, setApiTokenDraft] = useState("");
  const sourcesRef = useRef<EventSource[]>([]);
  const activeJobsRef = useRef<Job[]>([]);
  const eventsByJobIdRef = useRef<Record<string, JobEvent[]>>({});
  const pendingGeoIPRef = useRef<Set<string>>(new Set());
  const streamErrorFallbackTimersRef = useRef<Record<string, ReturnType<typeof setTimeout>>>({});
  const pendingStatusRefreshRef = useRef<Set<string>>(new Set());
  const restoreJobRequestRef = useRef(0);
  const versionClickCountRef = useRef(0);
  const versionClickTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null);

  const client = useMemo(() => (config ? new ApiClient(config) : null), [config]);
  const safeAgents = Array.isArray(agents) ? agents : [];
  const visibleAgents = useMemo(() => filterAgentsByPermissions(safeAgents, permissions), [permissions, safeAgents]);
  const allowedNavTools = useMemo(() => navTools.filter((tool) => toolAllowed(permissions, tool)), [permissions]);
  const visibleNavTools = useMemo(
    () => navTools.filter((tool) => allowedNavTools.includes(tool) || (page === "diagnostics" && tool === form.tool)),
    [allowedNavTools, form.tool, page]
  );
  const activeNavValue = page === "schedules" ? "schedules" : form.tool;
  const availableFanoutAgents = useMemo(() => capableAgents(visibleAgents, form.tool), [visibleAgents, form.tool]);
  const routeAgents = useMemo(() => capableAgents(visibleAgents, form.tool), [visibleAgents, form.tool]);
  const allEvents = useMemo(() => Object.values(eventsByJobId).flat(), [eventsByJobId]);
  const currentJob = activeJobs[0] ?? null;
  const resultTool = currentJob?.tool ?? form.tool;
  const mtrRows = useMemo(() => buildMtrRows(visibleAgents, currentJob?.agent_id || form.agentId, allEvents), [allEvents, currentJob?.agent_id, form.agentId, visibleAgents]);
  const mtrAgent = useMemo(
    () => visibleAgents.find((agent) => agent.id === (currentJob?.agent_id || form.agentId)),
    [currentJob?.agent_id, form.agentId, visibleAgents]
  );
  const mtrTargetIP = useMemo(() => targetResolvedIP(allEvents) || currentJob?.resolved_target || "", [allEvents, currentJob?.resolved_target]);
  const hasLiveJobs = useMemo(
    () => activeJobs.some((job) => !jobHasTerminalEvent(job, eventsByJobId[job.id] ?? [], safeAgents)),
    [activeJobs, eventsByJobId, safeAgents]
  );
  const jobRunning = activeJobs.length > 0 && hasLiveJobs;
  const controlsLocked = submitting || jobRunning;
  const nodeRows = useMemo(
    () => buildNodeRows(resultTool, visibleAgents, activeJobs.filter((job) => job.tool === resultTool), eventsByJobId),
    [activeJobs, eventsByJobId, resultTool, visibleAgents]
  );
  const showHTTPDownloadMetrics = resultTool === "http" && resultHTTPMethod(currentJob, form) === "GET";
  const isFanout = isFanoutTool(resultTool);
  const submissionIsFanout = isFanoutTool(form.tool);
  const geoIPTargets = useMemo(
    () => collectGeoIPTargets(isFanout, nodeRows, mtrRows, mtrTargetIP),
    [isFanout, mtrRows, mtrTargetIP, nodeRows]
  );
  const formError = localizedFormError(form, permissions, t) || permissionFormError(form, permissions, t);
  const onlineAgents = visibleAgents.filter((agent) => agent.status === "online").length;
  const noFanoutAgents = submissionIsFanout && availableFanoutAgents.length === 0;
  const canSubmitCurrentTool = toolAllowed(permissions, form.tool);
  const showReadonlyResultHeader = !canSubmitCurrentTool && Boolean(currentJob);
  const showFormError = attemptedSubmit && Boolean(formError) && !error && !permissionsLoadFailed;
  const showNoFanoutAgents = attemptedSubmit && noFanoutAgents && !error && !formError && !permissionsLoadFailed;
  const runDisabled = !client || permissionsLoadFailed || !canSubmitCurrentTool || controlsLocked;

  useEffect(() => {
    loadConfig()
      .then(setConfig)
      .catch(() => setConfig({ apiBaseUrl: "", apiToken: "" }))
      .finally(() => setLoadingConfig(false));
  }, []);

  useEffect(() => {
    if (!client) {
      return;
    }
    void refreshServerVersion(client);
    void refreshPermissionsAndAgents(client);
  }, [client]);

  useEffect(() => {
    if (!client || !routeJobID || page !== "diagnostics") {
      return;
    }
    const requestID = ++restoreJobRequestRef.current;
    closeStreams();
    setStreaming(false);
    activeJobsRef.current = [];
    setActiveJobs([]);
    eventsByJobIdRef.current = {};
    setEventsByJobId({});
    setError(null);
    void restoreJobFromRoute(client, routeJobID, requestID);
  }, [client, page, routeJobID]);

  useEffect(() => {
    if (toolAllowed(permissions, form.tool)) {
      return;
    }
    if (routeJobID || (typeof window !== "undefined" && locationHasExplicitTool(window.location))) {
      return;
    }
    const nextTool = allowedNavTools[0];
    if (nextTool) {
      setForm((current) => ({
        ...current,
        tool: nextTool,
        agentId: requiresAgentForTool(permissions, nextTool) ? capableAgents(visibleAgents, nextTool)[0]?.id ?? "" : "",
        target: normalizeTargetForTool(current.target, nextTool)
      }));
    }
  }, [allowedNavTools, form.tool, permissions, routeJobID, visibleAgents]);

  useEffect(() => {
    if (page === "schedules" && permissions && !canReadSchedules(permissions)) {
      setPage("diagnostics");
      window.history.replaceState(null, "", formStatePath(form));
    }
  }, [form, page, permissions]);

  useEffect(() => {
    const tool = permissions?.tools?.[form.tool];
    if (!tool) {
      return;
    }
    const ips = ipVersionOptions(permissions, form.tool);
    if (form.tool !== "dns" && ips.length > 0 && !ips.some((option) => option.value === String(form.ipVersion))) {
      updateForm("ipVersion", Number(ips[0].value) as IPVersion);
      return;
    }
    if (tool.resolve_on_agent !== undefined && form.resolveOnAgent !== tool.resolve_on_agent) {
      updateForm("resolveOnAgent", tool.resolve_on_agent);
      return;
    }
    const protocols = protocolOptions(permissions, form.tool);
    if ((form.tool === "ping" || form.tool === "mtr" || form.tool === "traceroute") && protocols.length > 0 && !protocols.some((option) => option.value === form.protocol)) {
      updateForm("protocol", protocols[0].value);
      return;
    }
    const dnsTypes = dnsTypeOptions(permissions);
    if (form.tool === "dns" && dnsTypes.length > 0 && !dnsTypes.includes(form.dnsType)) {
      updateForm("dnsType", dnsTypes[0] as JobFormState["dnsType"]);
      return;
    }
    const methods = httpMethodOptions(permissions);
    if (form.tool === "http" && methods.length > 0 && !methods.some((option) => option.value === form.method)) {
      updateForm("method", methods[0].value);
    }
  }, [form.dnsType, form.ipVersion, form.method, form.protocol, form.resolveOnAgent, form.tool, permissions]);

  useEffect(() => {
    const onPopState = () => {
      if (!controlsLocked) {
        const nextJobID = jobIDFromLocation(window.location);
        setPage(pageFromLocation(window.location));
        setForm(formStateFromLocation(window.location));
        setRouteJobID(nextJobID);
        if (!nextJobID) {
          cancelRouteRestore();
          closeStreams();
          setStreaming(false);
          activeJobsRef.current = [];
          setActiveJobs([]);
          eventsByJobIdRef.current = {};
          setEventsByJobId({});
        }
        setError(null);
      }
    };
    window.addEventListener("popstate", onPopState);
    return () => window.removeEventListener("popstate", onPopState);
  }, [controlsLocked]);

  useEffect(() => {
    if (!toolAllowed(permissions, form.tool)) {
      return;
    }
    if (!requiresAgentForTool(permissions, form.tool)) {
      if (form.agentId) {
        updateForm("agentId", "");
      }
      return;
    }
    if (!form.agentId || !routeAgents.some((agent) => agent.id === form.agentId)) {
      updateForm("agentId", routeAgents[0]?.id ?? "");
    }
  }, [form.tool, form.agentId, permissions, routeAgents]);

  useEffect(() => {
    if (streaming && activeJobs.length > 0 && !hasLiveJobs) {
      closeStreams();
      setStreaming(false);
    }
  }, [activeJobs.length, hasLiveJobs, streaming]);

  useEffect(() => {
    if (!client || geoIPTargets.length === 0) {
      return;
    }
    const missing = geoIPTargets.filter((ip) => !(ip in geoIPByIP) && !pendingGeoIPRef.current.has(ip));
    if (missing.length === 0) {
      return;
    }
    missing.forEach((ip) => pendingGeoIPRef.current.add(ip));
    void Promise.all(
      missing.map(async (ip) => {
        try {
          return [ip, await client.getGeoIP(ip)] as const;
        } catch {
          return [ip, null] as const;
        } finally {
          pendingGeoIPRef.current.delete(ip);
        }
      })
    ).then((entries) => {
      setGeoIPByIP((current) => {
        const next = { ...current };
        for (const [ip, info] of entries) {
          next[ip] = info;
        }
        return next;
      });
    });
  }, [client, geoIPByIP, geoIPTargets]);

  useEffect(() => () => closeStreams(), []);

  useEffect(() => {
    activeJobsRef.current = activeJobs;
  }, [activeJobs]);

  useEffect(() => {
    eventsByJobIdRef.current = eventsByJobId;
  }, [eventsByJobId]);

  useEffect(
    () => () => {
      if (versionClickTimerRef.current) {
        clearTimeout(versionClickTimerRef.current);
      }
    },
    []
  );

  async function refreshAgents(api = client, quiet = false) {
    if (!api) {
      return;
    }
    if (!quiet) {
      setLoadingAgents(true);
    }
    try {
      setAgents(await api.listAgents());
      if (!quiet) {
        setError(null);
      }
    } catch (err) {
      if (!quiet) {
        setError(errorMessage(err));
      }
    } finally {
      if (!quiet) {
        setLoadingAgents(false);
      }
    }
  }

  async function refreshPermissionsAndAgents(api: ApiClient) {
    try {
      const [nextPermissions] = await Promise.all([api.getPermissions(), refreshAgents(api, true)]);
      setPermissions(nextPermissions);
      setPermissionsLoadFailed(false);
      setError(null);
    } catch {
      setPermissions({ tools: {}, agents: [], schedule_access: "none" });
      setAgents([]);
      setPermissionsLoadFailed(true);
      setError(t("errors.permissionsUnavailable"));
    }
  }

  async function refreshServerVersion(api: ApiClient) {
    try {
      setServerVersion(await api.getVersion());
    } catch {
      setServerVersion(null);
    }
  }

  async function restoreJobFromRoute(api: ApiClient, jobID: string, requestID: number) {
    try {
      const job = await api.getJob(jobID);
      if (requestID !== restoreJobRequestRef.current) {
        return;
      }
      const base = typeof window === "undefined" ? defaultFormState : formStateFromLocation(window.location, form);
      if (typeof window !== "undefined" && !jobMatchesLocation(job, window.location)) {
        window.history.replaceState(null, "", formStatePath(base));
        setRouteJobID("");
        setError(t("errors.jobLinkMismatch"));
        return;
      }
      setPage("diagnostics");
      setForm(formStateFromJob(job, base));
      activeJobsRef.current = [job];
      setActiveJobs([job]);
      setAttemptedSubmit(false);

      if (jobHasTerminalEvent(job, [], [])) {
        const events = await api.listJobEvents(job.id);
        if (requestID !== restoreJobRequestRef.current) {
          return;
        }
        eventsByJobIdRef.current = { [job.id]: events };
        setEventsByJobId({ [job.id]: events });
        const eventFailureMessage = events.map((event) => jobErrorEventMessage(event)).find(Boolean);
        setError(eventFailureMessage ?? jobFailureMessage(job));
        return;
      }

      eventsByJobIdRef.current = {};
      setEventsByJobId({});
      setError(jobFailureMessage(job));
      openStream(api, job.id, job.agent_id);
    } catch (err) {
      if (requestID === restoreJobRequestRef.current) {
        setError(errorMessage(err));
      }
    }
  }

  function cancelRouteRestore() {
    restoreJobRequestRef.current += 1;
  }

  async function submitJob(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    setAttemptedSubmit(true);
    if (!client || !canSubmitCurrentTool || formError || noFanoutAgents || controlsLocked) {
      return;
    }
    setSubmitting(true);
    setError(null);
    closeStreams();
    setStreaming(false);
    cancelRouteRestore();
    setRouteJobID("");
    activeJobsRef.current = [];
    setActiveJobs([]);
    eventsByJobIdRef.current = {};
    setEventsByJobId({});
    const requestForm = formWithPermissionDefaults(form, permissions);

    try {
      if (submissionIsFanout) {
        const job = await client.createJob(buildCreateJobRequest({ ...requestForm, agentId: "" }));
        window.history.pushState(null, "", jobResultPath(requestForm, job.id));
        handleCreatedJob(client, job);
      } else {
        const job = await client.createJob(buildCreateJobRequest(requestForm));
        window.history.pushState(null, "", jobResultPath(requestForm, job.id));
        handleCreatedJob(client, job, job.agent_id);
      }
    } catch (err) {
      const message = errorMessage(err);
      setError(message);
      notifications.show({ color: "red", title: t("statusValues.error"), message });
    } finally {
      setSubmitting(false);
    }
  }

  function openStream(api: ApiClient, jobID: string, agentID?: string) {
    const source = new EventSource(api.jobStreamUrl(jobID));
    sourcesRef.current.push(source);
    setStreaming(true);

    const onEvent = (message: MessageEvent<string>) => {
      try {
        const event = jobEventFromStreamMessage(message, jobID, agentID);
        const eventFailureMessage = jobErrorEventMessage(event);
        if (eventFailureMessage) {
          setError(eventFailureMessage);
        }
        clearStatusFallback(streamErrorFallbackTimersRef, event.job_id);
        setEventsByJobId((current) => {
          const next = {
            ...current,
            [event.job_id]: mergeEvent(current[event.job_id] ?? [], event)
          };
          eventsByJobIdRef.current = next;
          return next;
        });
      } catch {
        setError(t("errors.invalidStream"));
      }
    };

    for (const name of streamNames) {
      source.addEventListener(name, onEvent as EventListener);
    }
    source.onmessage = onEvent;
    source.onerror = () => {
      const live = sourcesRef.current.some((item) => item.readyState !== EventSource.CLOSED);
      setStreaming(live);
      scheduleStatusFallback(api, jobID, streamErrorStatusFallbackMS, streamErrorFallbackTimersRef);
    };
  }

  function handleCreatedJob(api: ApiClient, job: Job, agentID?: string) {
    activeJobsRef.current = [job];
    setActiveJobs([job]);
    const failureMessage = jobFailureMessage(job);
    if (failureMessage) {
      setError(failureMessage);
      notifications.show({ color: "red", title: t("statusValues.error"), message: failureMessage });
      return;
    }
    openStream(api, job.id, agentID);
  }

  function jobFailureMessage(job: Job): string | null {
    if (job.status !== "failed") {
      return null;
    }
    if (job.error_type === "fanout_failed") {
      return null;
    }
    return jobErrorTypeMessage(job.error_type || "job_failed");
  }

  function jobErrorEventMessage(event: JobEvent): string | null {
    const failureType = jobEventFailureType(event);
    if (failureType) {
      const job = activeJobsRef.current.find((item) => item.id === event.job_id);
      if (job && shouldSuppressFanoutNodeFailure(job, event)) {
        return null;
      }
      if (failureType === "fanout_failed") {
        return null;
      }
      return jobErrorTypeMessage(failureType);
    }
    return null;
  }

  function jobErrorTypeMessage(type: string): string {
    const key = `jobErrorTypes.${type}`;
    const translated = t(key);
    return translated === key ? t("jobErrorTypes.job_failed") : translated;
  }

  function scheduleStatusFallback(
    api: ApiClient,
    jobID: string,
    delayMS: number,
    timersRef: React.MutableRefObject<Record<string, ReturnType<typeof setTimeout>>>
  ) {
    clearStatusFallback(timersRef, jobID);
    timersRef.current[jobID] = setTimeout(() => {
      delete timersRef.current[jobID];
      void refreshJobStatus(api, jobID);
    }, delayMS);
  }

  function clearStatusFallback(
    timersRef: React.MutableRefObject<Record<string, ReturnType<typeof setTimeout>>>,
    jobID: string
  ) {
    const timer = timersRef.current[jobID];
    if (!timer) {
      return;
    }
    clearTimeout(timer);
    delete timersRef.current[jobID];
  }

  function clearAllStatusFallbacks() {
    for (const timer of Object.values(streamErrorFallbackTimersRef.current)) {
      clearTimeout(timer);
    }
    streamErrorFallbackTimersRef.current = {};
  }

  async function refreshJobStatus(api: ApiClient, jobID: string) {
    if (pendingStatusRefreshRef.current.has(jobID)) {
      return;
    }
    pendingStatusRefreshRef.current.add(jobID);
    try {
      const job = await api.getJob(jobID);
      setActiveJobs((current) => current.map((item) => (item.id === job.id ? { ...item, ...job } : item)));
    } catch {
      // The stream itself remains the source of truth if the status refresh races a transient failure.
    } finally {
      pendingStatusRefreshRef.current.delete(jobID);
    }
  }

  function closeStreams() {
    for (const source of sourcesRef.current) {
      source.onerror = null;
      source.close();
    }
    sourcesRef.current = [];
    clearAllStatusFallbacks();
  }

  function updateForm<Key extends keyof JobFormState>(key: Key, value: JobFormState[Key]) {
    setAttemptedSubmit(false);
    setForm((current) => ({ ...current, [key]: value }));
  }

  function changeTool(tool: Tool) {
    if (controlsLocked) {
      return;
    }
    closeStreams();
    setStreaming(false);
    cancelRouteRestore();
    setRouteJobID("");
    activeJobsRef.current = [];
    setActiveJobs([]);
    eventsByJobIdRef.current = {};
    setEventsByJobId({});
    setError(null);
    setAttemptedSubmit(false);
    setPage("diagnostics");
    const next = {
      ...form,
      tool,
      agentId: requiresAgentForTool(permissions, tool) ? capableAgents(visibleAgents, tool)[0]?.id ?? "" : "",
      target: normalizeTargetForTool(form.target, tool)
    };
    setForm(next);
    window.history.pushState(null, "", formStatePath(next));
  }

  function changeNav(value: string) {
    if (value === "schedules") {
      if (controlsLocked || !canReadSchedules(permissions)) {
        return;
      }
      closeStreams();
      setStreaming(false);
      cancelRouteRestore();
      setRouteJobID("");
      activeJobsRef.current = [];
      setActiveJobs([]);
      eventsByJobIdRef.current = {};
      setEventsByJobId({});
      setError(null);
      setAttemptedSubmit(false);
      setPage("schedules");
      window.history.pushState(null, "", "/schedules");
      return;
    }
    changeTool(value as Tool);
  }

  function goHome() {
    if (controlsLocked) {
      return;
    }
    const tool = toolAllowed(permissions, defaultFormState.tool) ? defaultFormState.tool : allowedNavTools[0] ?? defaultFormState.tool;
    const next: JobFormState = {
      ...defaultFormState,
      tool,
      agentId: requiresAgentForTool(permissions, tool) ? capableAgents(visibleAgents, tool)[0]?.id ?? "" : ""
    };
    closeStreams();
    setStreaming(false);
    cancelRouteRestore();
    setRouteJobID("");
    activeJobsRef.current = [];
    setActiveJobs([]);
    eventsByJobIdRef.current = {};
    setEventsByJobId({});
    setError(null);
    setAttemptedSubmit(false);
    setPage("diagnostics");
    setForm(next);
    window.history.pushState(null, "", "/");
  }

  function onBrandClick(event: React.MouseEvent<HTMLAnchorElement>) {
    if (event.defaultPrevented || event.button !== 0 || event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) {
      return;
    }
    event.preventDefault();
    goHome();
  }

  function jumpToMtr(ip: string, agentId: string) {
    if (controlsLocked || !toolAllowed(permissions, "mtr")) {
      return;
    }
    const target = normalizeIPAddress(ip) ?? ip;
    const mtrAgents = capableAgents(visibleAgents, "mtr");
    const selectedAgentId = mtrAgents.some((agent) => agent.id === agentId) ? agentId : mtrAgents[0]?.id ?? "";
    if (requiresAgentForTool(permissions, "mtr") && !selectedAgentId) {
      return;
    }
    const next: JobFormState = {
      ...form,
      tool: "mtr",
      target,
      agentId: selectedAgentId,
      ipVersion: target.includes(":") ? 6 : 4
    };
    closeStreams();
    setStreaming(false);
    cancelRouteRestore();
    setRouteJobID("");
    activeJobsRef.current = [];
    setActiveJobs([]);
    eventsByJobIdRef.current = {};
    setEventsByJobId({});
    setError(null);
    setAttemptedSubmit(false);
    setPage("diagnostics");
    setForm(next);
    window.history.pushState(null, "", formStatePath(next));
  }

  function openTokenDialog() {
    setApiTokenDraft(config?.apiToken ?? "");
    setTokenDialogOpen(true);
  }

  function onVersionClick() {
    if (versionClickTimerRef.current) {
      clearTimeout(versionClickTimerRef.current);
      versionClickTimerRef.current = null;
    }
    versionClickCountRef.current += 1;
    if (versionClickCountRef.current >= 5) {
      versionClickCountRef.current = 0;
      openTokenDialog();
      return;
    }
    versionClickTimerRef.current = setTimeout(() => {
      versionClickCountRef.current = 0;
      versionClickTimerRef.current = null;
    }, 1800);
  }

  function saveApiTokenOverride() {
    const apiToken = apiTokenDraft.trim();
    saveStoredApiToken(apiToken);
    closeStreams();
    setStreaming(false);
    cancelRouteRestore();
    setRouteJobID("");
    activeJobsRef.current = [];
    setActiveJobs([]);
    eventsByJobIdRef.current = {};
    setEventsByJobId({});
    setAttemptedSubmit(false);
    setPermissions(null);
    setPermissionsLoadFailed(false);
    setAgents([]);
    setConfig((current) => ({
      apiBaseUrl: current?.apiBaseUrl ?? "",
      apiToken
    }));
    setError(null);
    setTokenDialogOpen(false);
    notifications.show({
      color: "green",
      title: t("token.savedTitle"),
      message: t("token.savedMessage")
    });
  }

  const settingsControls = (className: string) => (
    <Group gap="md" className={className}>
      <Select
        aria-label={t("language")}
        checkIconPosition="left"
        className="language-select"
        data={supportedLanguages.map((language) => ({
          value: language,
          label: language === "zh-CN" ? "中文" : "English"
        }))}
        leftSection={<Globe2 size={15} />}
        value={i18n.language}
        onChange={(value) => value && setLanguage(value as SupportedLanguage)}
      />
      <Switch
        aria-label={computedColorScheme === "dark" ? t("theme.dark") : t("theme.light")}
        checked={computedColorScheme === "dark"}
        offLabel={<Sun size={13} />}
        onLabel={<Moon size={13} />}
        onChange={(event) => setColorScheme(event.currentTarget.checked ? "dark" : "light")}
      />
    </Group>
  );

  return (
    <Box className="page">
      <Container className="page-shell" size="xl" px="md">
        <header className="top-nav">
          <Anchor href="/" underline="never" className="brand" c="inherit" onClick={onBrandClick}>
            <Title order={1} style={{ fontSize: "inherit", fontWeight: "inherit", lineHeight: "inherit" }}>
              {t("brand")}
            </Title>
          </Anchor>
          <Group gap="lg" justify="flex-end" className="nav-actions">
            <Tabs className="tool-tabs-root" value={activeNavValue} onChange={(value) => value && changeNav(value)}>
              <Tabs.List className="tool-tabs">
                {visibleNavTools.map((tool) => (
                  <Tabs.Tab key={tool} value={tool} disabled={controlsLocked || !toolAllowed(permissions, tool)}>
                    {t(`nav.${tool}`)}
                  </Tabs.Tab>
                ))}
                {canReadSchedules(permissions) && (
                  <Tabs.Tab className="watch-tab" value="schedules" disabled={controlsLocked}>
                    {t("nav.schedules")}
                  </Tabs.Tab>
                )}
              </Tabs.List>
            </Tabs>
            {settingsControls("settings-controls desktop-settings")}
          </Group>
        </header>

        {page === "schedules" ? (
          <SchedulePage client={client} permissions={permissions} agents={visibleAgents} t={t} />
        ) : (
          <>
            {canSubmitCurrentTool && (
              <form onSubmit={submitJob}>
                <Paper className="query-panel" withBorder>
                  <div className={`query-grid tool-${form.tool}`}>
                    <TextInput
                      className="target-input"
                      disabled={controlsLocked}
                      label={t("form.target")}
                      value={form.target}
                      onChange={(event) => updateForm("target", event.currentTarget.value)}
                      placeholder={targetPlaceholder(form.tool, t)}
                    />
                    {form.tool !== "dns" && (
                      <Select
                        className="ip-version-field"
                        checkIconPosition="left"
                        disabled={controlsLocked}
                        label={t("form.ipVersion")}
                        data={ipVersionOptions(permissions, form.tool)}
                        value={String(form.ipVersion)}
                        onChange={(value) => updateForm("ipVersion", Number(value ?? 0) as IPVersion)}
                      />
                    )}
                    <DynamicFields form={form} permissions={permissions} updateForm={updateForm} disabled={controlsLocked} t={t} />
                    {requiresAgentForTool(permissions, form.tool) && (
                      <Select
                        className="agent-field"
                        checkIconPosition="left"
                        disabled={controlsLocked}
                        label={t("form.agent")}
                        data={routeAgents.map((agent) => ({
                          value: agent.id,
                          label: agentSelectLabel(agent)
                        }))}
                        value={form.agentId || null}
                        onChange={(value) => updateForm("agentId", value ?? routeAgents[0]?.id ?? "")}
                        error={!form.agentId ? t("errors.agentRequired", { tool: form.tool }) : null}
                      />
                    )}
                    {form.tool !== "dns" && (
                      <RemoteDNSSwitch
                        className="remote-dns-field"
                        disabled={controlsLocked}
                        form={form}
                        permissions={permissions}
                        t={t}
                        updateForm={updateForm}
                      />
                    )}
                    <Button
                      className="run-button"
                      disabled={runDisabled}
                      leftSection={controlsLocked ? <Loader size={16} color="white" /> : <Play size={16} />}
                      type="submit"
                      variant="default"
                    >
                      {controlsLocked ? t("form.running") : t("form.run")}
                    </Button>
                  </div>
                </Paper>
              </form>
            )}
            {showReadonlyResultHeader && currentJob && (
              <ReadonlyResultHeader job={currentJob} t={t} />
            )}

            <Group justify="space-between" mt="xl" mb="md">
              <Group gap="xs">
                <Badge variant="light" color={onlineAgents > 0 ? "green" : "gray"} leftSection={<Wifi size={12} />}>
                  {t("status.agentsOnline", { online: onlineAgents, total: visibleAgents.length })}
                </Badge>
                <Badge variant="light" color={jobRunning ? "green" : "gray"}>
                  {jobRunning ? t("status.streamLive") : t("status.streamClosed")}
                </Badge>
              </Group>
              <Switch className="compact-switch" checked={compact} label={t("results.compact")} onChange={(event) => setCompact(event.currentTarget.checked)} />
            </Group>

            {error && (
              <Alert color="red" icon={<AlertTriangle size={18} />} mb="md" radius="md">
                {error}
              </Alert>
            )}
            {showFormError && (
              <Alert color="yellow" mb="md" radius="md">
                {formError}
              </Alert>
            )}
            {showNoFanoutAgents && (
              <Alert color="yellow" mb="md" radius="md">
                {t("form.noAvailableAgents")}
              </Alert>
            )}

            <Paper withBorder className="result-table">
              {isFanout ? (
                <NodeResultTable tool={resultTool} rows={nodeRows} compact={compact} geoIPByIP={geoIPByIP} onTraceIP={jumpToMtr} showHTTPDownloadMetrics={showHTTPDownloadMetrics} t={t} />
              ) : (
                <MtrResultTable tool={resultTool} agent={mtrAgent} targetIP={mtrTargetIP} rows={mtrRows} compact={compact} geoIPByIP={geoIPByIP} t={t} />
              )}
            </Paper>
          </>
        )}
        <footer className="app-footer">
          <div className="mobile-settings-footer">
            {settingsControls("settings-controls")}
          </div>
          <div className="footer-meta-row">
            <Text c="dimmed" size="sm" className="footer-copyright">
              {t("footer.copyright", { year: new Date().getFullYear(), brand: t("brand") })}
            </Text>
            <Group gap="sm" className="footer-version-group">
              <Anchor href="https://github.com/ztelliot/mtr" target="_blank" c="dimmed" size="sm" underline="never" className="footer-repo-link">
                <GitBranch size={14} /> ztelliot/mtr
              </Anchor>
              <Text c="dimmed" size="sm" className="footer-separator">|</Text>
              <button aria-label={t("footer.versionSettings")} className="version-button footer-version" type="button" onClick={onVersionClick}>
                <span>{t("footer.frontendVersion", { version: appVersionLabel })}</span>
                <span>{t("footer.serverVersion", { version: formatServerVersion(serverVersion, t) })}</span>
              </button>
            </Group>
          </div>
        </footer>
      </Container>
      <Modal centered opened={tokenDialogOpen} title={t("token.title")} onClose={() => setTokenDialogOpen(false)}>
        <form onSubmit={(event) => {
          event.preventDefault();
          saveApiTokenOverride();
        }}>
          <Stack gap="md">
            <PasswordInput
              autoFocus
              className="token-field"
              label={t("token.apiToken")}
              placeholder="api-token"
              value={apiTokenDraft}
              onChange={(event) => setApiTokenDraft(event.currentTarget.value)}
            />
            <Group justify="flex-end" gap="sm">
              <Button variant="default" type="button" onClick={() => setTokenDialogOpen(false)}>
                {t("actions.cancel")}
              </Button>
              <Button type="submit">
                {t("actions.save")}
              </Button>
            </Group>
          </Stack>
        </form>
      </Modal>
    </Box>
  );
}

function ReadonlyResultHeader({
  job,
  t
}: {
  job: Job;
  t: (key: string, options?: Record<string, unknown>) => string;
}) {
  return (
    <Paper className="schedule-panel readonly-result-summary-panel" withBorder>
      <div className="readonly-result-summary">
        <div className="readonly-result-summary-item">
          <Text className="mtr-summary-label">{t("results.target")}</Text>
          <div className="readonly-result-summary-value">{jobTargetLabel(job)}</div>
        </div>
        <div className="readonly-result-summary-item">
          <Text className="mtr-summary-label">{t("results.parameters")}</Text>
          <div className="readonly-result-summary-value">{jobParameterSummary(job) || "-"}</div>
        </div>
        <div className="readonly-result-summary-item">
          <Text className="mtr-summary-label">{t("schedule.runAt")}</Text>
          <div className="readonly-result-summary-value">{formatDateTime(job.started_at || job.created_at)}</div>
        </div>
      </div>
    </Paper>
  );
}

function jobTargetLabel(job: Job): string {
  if (job.tool === "port" && job.args?.port) {
    const host = job.target.includes(":") && !job.target.startsWith("[") ? `[${job.target}]` : job.target;
    return `${host}:${job.args.port}`;
  }
  return job.target;
}

function jobParameterSummary(job: Job): string {
  const args = job.args ?? {};
  const details: string[] = [];
  const protocol = typeof args.protocol === "string" ? args.protocol.trim().toUpperCase() : "";
  if (protocol) {
    details.push(protocol);
  }
  if (job.tool !== "dns" && job.ip_version !== undefined && job.ip_version !== 0) {
    details.push(`IPv${job.ip_version}`);
  }
  const method = typeof args.method === "string" ? args.method.trim().toUpperCase() : "";
  if (job.tool === "http" && method) {
    details.push(method);
  }
  const recordType = typeof args.type === "string" ? args.type.trim().toUpperCase() : "";
  if (job.tool === "dns" && recordType) {
    details.push(recordType);
  }
  return details.join(" · ");
}

function resultHTTPMethod(job: Job | null, form: JobFormState): JobFormState["method"] {
  const method = typeof job?.args?.method === "string" ? job.args.method.trim().toUpperCase() : "";
  return method === "GET" || method === "HEAD" ? method : form.method;
}

function pageFromLocation(location: Pick<Location, "pathname">): AppPage {
  return location.pathname.split("/").filter(Boolean)[0] === "schedules" ? "schedules" : "diagnostics";
}

function jobIDFromLocation(location: Pick<Location, "pathname" | "search">): string {
  const params = new URLSearchParams(location.search);
  const queryID = params.get("job_id") ?? params.get("jobId");
  if (queryID?.trim()) {
    return queryID.trim();
  }
  const segments = location.pathname.split("/").filter(Boolean).map((segment) => decodeURIComponent(segment));
  return segments[0] === "jobs" && segments[1] ? segments[1] : "";
}

function jobMatchesLocation(job: Job, location: Pick<Location, "pathname" | "search">): boolean {
  if (!locationHasJobContext(location)) {
    return true;
  }
  const params = new URLSearchParams(location.search);
  const expected = formStateFromLocation(location);
  const actual = formStateFromJob(job, expected);
  if (locationHasExplicitTool(location) && actual.tool !== expected.tool) {
    return false;
  }
  if (locationHasExplicitTarget(location) && actual.target !== expected.target) {
    return false;
  }
  if (expected.tool !== "dns" && hasAnyParam(params, ["ip_version", "ipVersion"]) && actual.ipVersion !== expected.ipVersion) {
    return false;
  }
  if (hasAnyParam(params, ["agent_id", "agentId"]) && actual.agentId !== expected.agentId) {
    return false;
  }
  if ((expected.tool === "ping" || expected.tool === "traceroute" || expected.tool === "mtr") && params.has("protocol") && actual.protocol !== expected.protocol) {
    return false;
  }
  if (expected.tool === "dns" && hasAnyParam(params, ["type", "dns_type", "dnsType"]) && actual.dnsType !== expected.dnsType) {
    return false;
  }
  if (expected.tool === "http" && params.has("method") && actual.method !== expected.method) {
    return false;
  }
  if (expected.tool !== "dns" && hasAnyParam(params, ["resolve_on_agent", "remote_dns", "remoteDns"]) && actual.resolveOnAgent !== expected.resolveOnAgent) {
    return false;
  }
  return true;
}

function locationHasJobContext(location: Pick<Location, "pathname" | "search">): boolean {
  const params = new URLSearchParams(location.search);
  if (hasAnyParam(params, ["tool", "target", "host"])) {
    return true;
  }
  const segments = location.pathname.split("/").filter(Boolean).map((segment) => decodeURIComponent(segment));
  return segments.some((segment) => navTools.includes(segment as Tool));
}

function hasAnyParam(params: URLSearchParams, names: string[]): boolean {
  return names.some((name) => params.has(name));
}
