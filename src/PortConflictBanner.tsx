import { Alert, Button, Typography } from "antd";

import { useI18n } from "./i18n";
import type { MessageKey } from "./i18n";
import type { PortHolder, PortStatus, PortTarget } from "./portStatus";

const { Text } = Typography;

/** 非自身占用才构成冲突：sidecar 自己占用端口属于正常状态。 */
function externalHolders(target: PortTarget): PortHolder[] {
  return target.holders.filter((holder) => !holder.is_self);
}

function isConflicting(target: PortTarget): boolean {
  if (target.status === "free" || target.status === "unavailable") {
    return false;
  }
  if (target.status === "unknown") {
    return true;
  }
  return target.holders.length === 0 || externalHolders(target).length > 0;
}

function holderHint(holders: PortHolder[]): MessageKey | null {
  const names = holders.map((holder) => holder.name.toLowerCase()).join(" ");
  if (/svchost|system/.test(names)) {
    return "port.hintIcs";
  }
  if (/docker/.test(names)) {
    return "port.hintDocker";
  }
  if (/wsl/.test(names)) {
    return "port.hintWsl";
  }
  if (holders.length === 0) {
    return "port.hintReserved";
  }
  return null;
}

export function PortConflictBanner(props: {
  status: PortStatus | null;
  error: string | null;
  onRefresh: () => void;
}) {
  const { t } = useI18n();
  const { status, error, onRefresh } = props;

  if (error) {
    return (
      <Alert
        type="warning"
        showIcon
        style={{ marginBottom: 16 }}
        title={t("port.title")}
        description={error}
        action={
          <Button size="small" onClick={onRefresh}>
            {t("port.retry")}
          </Button>
        }
      />
    );
  }

  const conflicts = status?.targets.filter(isConflicting) ?? [];
  if (conflicts.length === 0) {
    return null;
  }

  const hints = Array.from(
    new Set(
      conflicts
        .map((target) => holderHint(externalHolders(target)))
        .filter((hint): hint is MessageKey => hint !== null),
    ),
  );
  const hasOccupied = conflicts.some((target) => target.status === "in_use");

  return (
    <Alert
      type="warning"
      showIcon
      style={{ marginBottom: 16 }}
      title={hasOccupied ? t("port.conflict") : t("port.bindFailed")}
      description={
        <div>
          {conflicts.map((target) => {
            const holders = externalHolders(target);
            return (
              <div key={`${target.protocol}:${target.address}`}>
                <Text code>
                  {target.address} / {target.protocol.toUpperCase()}
                </Text>{" "}
                {target.status === "unknown" ? t("port.unknown") : t("port.occupied")}
                {holders.length > 0 && (
                  <span>
                    {": "}
                    {holders
                      .map((holder) =>
                        holder.name
                          ? `${holder.name} (PID ${holder.pid})`
                          : t("port.pidUnknown", { pid: holder.pid }),
                      )
                      .join(", ")}
                  </span>
                )}
              </div>
            );
          })}
          {hints.map((hint) => (
            <div key={hint}>{t(hint)}</div>
          ))}
          <div>
            {t("port.guidance")}{" "}
            <Text code>netsh int ipv4 show excludedportrange protocol=udp</Text>{" "}
            {t("port.guidanceEnd")}
          </div>
        </div>
      }
      action={
        <Button size="small" onClick={onRefresh}>
          {t("port.retry")}
        </Button>
      }
    />
  );
}
