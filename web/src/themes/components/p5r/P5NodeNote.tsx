import { useTranslation } from "react-i18next";
import { P5CutoutText } from "./P5CutoutText";

export function P5NodeNote({ online, total }: { online: number; total: number }) {
  const { t } = useTranslation();
  return (
    <div className="p5r-node-note" role="status" aria-label={t("status.agentsOnline", { online, total })}>
      <div className="p5r-note-main">
        <P5CutoutText text={t("workspace.onlineNodes")} variant="label" />
        <strong>{online}</strong>
      </div>
      <small>{t("workspace.totalNodes", { count: total })}</small>
    </div>
  );
}
