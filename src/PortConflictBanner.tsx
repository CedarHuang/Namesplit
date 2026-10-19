import { Alert, Button, Typography } from "antd";
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
    // 无法绑定且无法归因（保留端口/权限限制），按冲突提示处理。
    return true;
  }
  // in_use：系统表未给出持有者时不能排除外部占用；有持有者时只认非自身占用。
  return target.holders.length === 0 || externalHolders(target).length > 0;
}

function stateLabel(target: PortTarget): string {
  return target.status === "unknown" ? "无法绑定（可能为保留端口或权限受限）" : "已被占用";
}

function holderHint(holders: PortHolder[]): string | null {
  const names = holders.map((holder) => holder.name.toLowerCase()).join(" ");
  if (/svchost|system/.test(names)) {
    return "可能是 Internet 连接共享（ICS）、移动热点或系统服务占用。";
  }
  if (/docker/.test(names)) {
    return "可能是 Docker Desktop 的端口转发。";
  }
  if (/wsl/.test(names)) {
    return "可能是 WSL 的端口转发。";
  }
  if (holders.length === 0) {
    return "可能是 Hyper-V / WSL / Docker 的保留端口，或当前用户无权查询占用进程。";
  }
  return null;
}

export function PortConflictBanner(props: {
  status: PortStatus | null;
  error: string | null;
  onRefresh: () => void;
}) {
  const { status, error, onRefresh } = props;

  if (error) {
    return (
      <Alert
        type="warning"
        showIcon
        style={{ marginBottom: 16 }}
        message="53 端口检测失败"
        description={error}
        action={
          <Button size="small" onClick={onRefresh}>
            重新检测
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
        .filter((hint): hint is string => hint !== null),
    ),
  );
  const hasOccupied = conflicts.some((target) => target.status === "in_use");

  return (
    <Alert
      type="warning"
      showIcon
      style={{ marginBottom: 16 }}
      message={
        hasOccupied
          ? "53 端口冲突：DNS 解析无法监听"
          : "53 端口无法绑定：DNS 解析无法监听"
      }
      description={
        <div>
          {conflicts.map((target) => {
            const holders = externalHolders(target);
            return (
              <div key={`${target.protocol}:${target.address}`}>
                <Text code>
                  {target.address} / {target.protocol.toUpperCase()}
                </Text>{" "}
                {stateLabel(target)}
                {holders.length > 0 && (
                  <span>
                    ：{" "}
                    {holders
                      .map((holder) =>
                        holder.name
                          ? `${holder.name}（PID ${holder.pid}）`
                          : `PID ${holder.pid}（无法读取进程信息）`,
                      )
                      .join("、")}
                  </span>
                )}
              </div>
            );
          })}
          {hints.map((hint) => (
            <div key={hint}>{hint}</div>
          ))}
          <div>
            Namesplit 不会修改系统 DNS。请关闭占用 53
            端口的程序后重新检测；如为保留端口，可在管理员终端执行{" "}
            <Text code>netsh int ipv4 show excludedportrange protocol=udp</Text>{" "}
            查看。
          </div>
        </div>
      }
      action={
        <Button size="small" onClick={onRefresh}>
          重新检测
        </Button>
      }
    />
  );
}
