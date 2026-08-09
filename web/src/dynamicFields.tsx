import { Input, Select, Switch, TextInput } from "@mantine/core";
import { dnsTypeOptions, effectivePermissions, httpMethodOptions, httpVersionOptions, protocolOptions, resolveOnAgentValue } from "./permissions";
import type { Agent, JobFormState, Permissions, Tool } from "./types";

export function DynamicFields({
  form,
  permissions,
  agents,
  updateForm,
  disabled,
  t
}: {
  form: JobFormState;
  permissions: Permissions | null;
  agents?: Agent[];
  updateForm: <Key extends keyof JobFormState>(key: Key, value: JobFormState[Key]) => void;
  disabled: boolean;
  t: (key: string, options?: Record<string, unknown>) => string;
}) {
  const selectedAgent = (agents ?? []).find((agent) => agent.id === form.agentId) ?? null;
  if (form.tool === "dns") {
    return (
      <Select
        className="dynamic-field"
        checkIconPosition="left"
        label={t("form.recordType")}
        disabled={disabled}
        data={dnsTypeOptions(permissions, selectedAgent)}
        value={form.dnsType}
        onChange={(value) => updateForm("dnsType", (value ?? "A") as JobFormState["dnsType"])}
      />
    );
  }
  if (form.tool === "http") {
    const methods = httpMethodOptions(permissions, selectedAgent);
    const versions = httpVersionOptions(permissions, selectedAgent);
    return (
      <>
        {methods.length > 0 && (
          <Select
            className="dynamic-field http-method-field"
            checkIconPosition="left"
            disabled={disabled || methods.length === 1}
            label={t("form.method")}
            data={methods}
            value={form.method}
            onChange={(value) => updateForm("method", (value ?? methods[0].value) as JobFormState["method"])}
          />
        )}
        {versions.length > 0 && (
          <Select
            className="dynamic-field http-version-field"
            checkIconPosition="left"
            disabled={disabled || versions.length === 1}
            label={t("form.httpVersion")}
            data={versions}
            value={form.httpVersion}
            onChange={(value) => updateForm("httpVersion", (value ?? versions[0].value) as JobFormState["httpVersion"])}
          />
        )}
      </>
    );
  }
  if (form.tool === "port") {
    return null;
  }
  const protocols = protocolOptions(permissions, form.tool, selectedAgent);
  if (protocols.length === 0) {
    return null;
  }
  return (
    <>
      <Select
        className="dynamic-field protocol-field"
        checkIconPosition="left"
        disabled={disabled || protocols.length === 1}
        label={t("form.protocol")}
        data={protocols}
        value={form.protocol}
        onChange={(value) => updateForm("protocol", (value ?? protocols[0].value) as JobFormState["protocol"])}
      />
      {form.protocol !== "icmp" && (
        <TextInput
          className="dynamic-field probe-port-field"
          disabled={disabled}
          inputMode="numeric"
          label={t("form.port")}
          placeholder={form.protocol === "udp" ? "33434" : form.protocol === "tcp" ? "80" : "80 / 33434"}
          value={form.port}
          onChange={(event) => updateForm("port", event.currentTarget.value)}
        />
      )}
    </>
  );
}

export function RemoteDNSSwitch({
  className,
  disabled,
  form,
  agents,
  permissions,
  t,
  updateForm
}: {
  className?: string;
  disabled: boolean;
  form: JobFormState;
  agents?: Agent[];
  permissions: Permissions | null;
  t: (key: string, options?: Record<string, unknown>) => string;
  updateForm: <Key extends keyof JobFormState>(key: Key, value: JobFormState[Key]) => void;
}) {
  const selectedAgent = (agents ?? []).find((agent) => agent.id === form.agentId) ?? null;
  return (
    <Input.Wrapper className={className} label={t("form.remoteDns")}>
      <Switch
        aria-label={t("form.remoteDns")}
        checked={resolveOnAgentValue(permissions, form, agents)}
        className="remote-dns-switch"
        disabled={disabled || effectivePermissions(permissions, selectedAgent)?.tools?.[form.tool]?.resolve_on_agent !== undefined}
        onChange={(event) => updateForm("resolveOnAgent", event.currentTarget.checked)}
      />
    </Input.Wrapper>
  );
}

export function targetPlaceholder(tool: Tool, t: (key: string) => string): string {
  if (tool === "http") {
    return t("form.targetPlaceholderHttp");
  }
  if (tool === "port") {
    return t("form.targetPlaceholderPort");
  }
  return t("form.targetPlaceholderHost");
}
