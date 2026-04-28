import { Input, Select, Switch } from "@mantine/core";
import { dnsTypeOptions, httpMethodOptions, protocolOptions, resolveOnAgentValue } from "./permissions";
import type { JobFormState, Permissions, Tool } from "./types";

export function DynamicFields({
  form,
  permissions,
  updateForm,
  disabled,
  t
}: {
  form: JobFormState;
  permissions: Permissions | null;
  updateForm: <Key extends keyof JobFormState>(key: Key, value: JobFormState[Key]) => void;
  disabled: boolean;
  t: (key: string, options?: Record<string, unknown>) => string;
}) {
  if (form.tool === "dns") {
    return (
      <Select
        className="dynamic-field"
        checkIconPosition="left"
        label={t("form.recordType")}
        disabled={disabled}
        data={dnsTypeOptions(permissions)}
        value={form.dnsType}
        onChange={(value) => updateForm("dnsType", (value ?? "A") as JobFormState["dnsType"])}
      />
    );
  }
  if (form.tool === "http") {
    const methods = httpMethodOptions(permissions);
    if (methods.length <= 1) {
      return null;
    }
    return (
      <Select
        className="dynamic-field"
        checkIconPosition="left"
        disabled={disabled}
        label={t("form.method")}
        data={methods}
        value={form.method}
        onChange={(value) => updateForm("method", (value ?? "HEAD") as JobFormState["method"])}
      />
    );
  }
  if (form.tool === "port") {
    return null;
  }
  return protocolOptions(permissions, form.tool).length > 1 ? (
    <Select
      className="dynamic-field"
      checkIconPosition="left"
      disabled={disabled}
      label={t("form.protocol")}
      data={protocolOptions(permissions, form.tool)}
      value={form.protocol}
      onChange={(value) => updateForm("protocol", (value ?? "icmp") as JobFormState["protocol"])}
    />
  ) : null;
}

export function RemoteDNSSwitch({
  className,
  disabled,
  form,
  permissions,
  t,
  updateForm
}: {
  className?: string;
  disabled: boolean;
  form: JobFormState;
  permissions: Permissions | null;
  t: (key: string, options?: Record<string, unknown>) => string;
  updateForm: <Key extends keyof JobFormState>(key: Key, value: JobFormState[Key]) => void;
}) {
  return (
    <Input.Wrapper className={className} label={t("form.remoteDns")}>
      <Switch
        aria-label={t("form.remoteDns")}
        checked={resolveOnAgentValue(permissions, form)}
        className="remote-dns-switch"
        disabled={disabled || permissions?.tools?.[form.tool]?.resolve_on_agent !== undefined}
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
