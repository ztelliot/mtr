import { Wifi } from "lucide-react";
import { useTranslation } from "react-i18next";

export function NodeStatus({ online, total, className = "" }: { online: number; total: number; className?: string }) {
  const { t } = useTranslation();
  return (
    <div className={`node-status ${className}`} role="status" aria-label={t("status.agentsOnline", { online, total })}>
      <span className="node-status-label"><Wifi size={16} aria-hidden="true" /><span>{t("workspace.onlineNodes")}</span></span>
      <strong className="node-status-count">{online}<span> / {total}</span></strong>
    </div>
  );
}
