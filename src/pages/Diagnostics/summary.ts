import type { PortStatus, PortTarget } from "../../portStatus";
import type { SystemDnsStatus } from "../../systemDns";
import type { StatusResponse, ValidationDiagnostic } from "../../ipc/types";

export interface SummaryInput {
  appVersion: string;
  sidecarVersion: string;
  commit: string;
  dataDir: string;
  configPath: string;
  logPath: string;
  sidecarState: string;
  sidecarReason?: string;
  status: StatusResponse | null;
  ports: PortStatus | null;
  systemDns: SystemDnsStatus | null;
  configRevision: number | null;
  validationValid: boolean | null;
  errors: ValidationDiagnostic[];
  warnings: ValidationDiagnostic[];
}

function describeTarget(target: PortTarget): string {
  const holders =
    target.holders.length === 0
      ? "none"
      : target.holders
          .map((holder) => `${holder.name || "unknown"}(${holder.pid})${holder.is_self ? "[self]" : ""}`)
          .join(", ");
  return `${target.address}/${target.protocol}: ${target.status} (${holders})`;
}

/** 生成可复制的纯文本摘要；不包含 token、端口或凭据。 */
export function buildDiagnosticSummary(input: SummaryInput): string {
  const lines = [
    "Namesplit diagnostic summary",
    `app=${input.appVersion}`,
    `sidecar=${input.sidecarVersion}`,
    `commit=${input.commit}`,
    `data=${input.dataDir}`,
    `config=${input.configPath}`,
    `log=${input.logPath}`,
    `sidecar_state=${input.sidecarState}${input.sidecarReason ? ` reason=${input.sidecarReason}` : ""}`,
    `dns_ready=${input.status?.dns.ready ?? "unknown"}`,
    `config_revision=${input.configRevision ?? "unknown"}`,
    `config_valid=${input.validationValid ?? "unknown"}`,
  ];

  if (input.status?.dns.bind_error) {
    lines.push(`dns_bind_error=${input.status.dns.bind_error.message}`);
  }
  if (input.ports) {
    lines.push(`ports=${input.ports.targets.map(describeTarget).join(" | ")}`);
  }
  if (input.systemDns) {
    lines.push(
      `interfaces=${input.systemDns.interfaces
        .map(
          (item) =>
            `${item.name}:${item.points_to_loopback ? "loopback" : "external"}`,
        )
        .join(", ")}`,
    );
  }
  if (input.errors.length > 0) {
    lines.push(`config_errors=${input.errors.map((item) => item.code).join(",")}`);
  }
  if (input.warnings.length > 0) {
    lines.push(`config_warnings=${input.warnings.map((item) => item.code).join(",")}`);
  }
  return lines.join("\n");
}
