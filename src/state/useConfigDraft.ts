import { useCallback, useEffect, useRef, useState } from "react";

import { getConfig, saveConfig } from "../ipc/commands";
import { ipcErrorCode, ipcErrorPayload, ipcErrorMessage } from "../ipc/errors";
import type {
  ConfigDocument,
  ConfigEnvelope,
  ValidationDiagnostic,
} from "../ipc/types";
import { isTauri } from "../portStatus";

export interface SaveOutcome {
  ok: boolean;
  conflict: boolean;
  error?: string;
}

function extractDiagnostics(details: unknown): ValidationDiagnostic[] {
  if (!Array.isArray(details)) {
    return [];
  }
  return details.filter(
    (item): item is ValidationDiagnostic =>
      typeof item === "object" &&
      item !== null &&
      typeof (item as ValidationDiagnostic).code === "string" &&
      typeof (item as ValidationDiagnostic).message === "string",
  );
}

/**
 * 配置页共用：维护“服务器基线 + 本地草稿”。
 *
 * 草稿只存在本地，保存成功后才提升为基线；revision 冲突时保留用户的草稿，
 * 仅把基线切到服务器最新版本，让用户在最新配置上核对后重新保存。
 */
export function useConfigDraft() {
  const baselineRef = useRef<ConfigEnvelope | null>(null);
  const draftRef = useRef<ConfigDocument | null>(null);

  const [baseline, setBaseline] = useState<ConfigEnvelope | null>(null);
  const [draft, setDraft] = useState<ConfigDocument | null>(null);
  const [dirty, setDirty] = useState(false);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [validationErrors, setValidationErrors] = useState<ValidationDiagnostic[]>([]);

  /** 基线切换会丢弃草稿，仅用于加载/保存成功后的权威配置。 */
  const applyBaseline = useCallback((envelope: ConfigEnvelope) => {
    baselineRef.current = envelope;
    draftRef.current = null;
    setBaseline(envelope);
    setDraft(null);
    setDirty(false);
  }, []);

  const refresh = useCallback(async () => {
    setLoading(true);
    try {
      const envelope = await getConfig();
      applyBaseline(envelope);
      setError(null);
      setValidationErrors([]);
    } catch (loadError) {
      setError(ipcErrorMessage(loadError));
    } finally {
      setLoading(false);
    }
  }, [applyBaseline]);

  useEffect(() => {
    if (isTauri()) {
      void refresh();
    }
  }, [refresh]);

  const update = useCallback((updater: (current: ConfigDocument) => ConfigDocument) => {
    // 以 ref 读取，保证同一事件里连续调用 update 也能基于上一步草稿继续修改。
    const base = draftRef.current ?? baselineRef.current?.config ?? null;
    if (!base) {
      return;
    }
    const next = updater(base);
    draftRef.current = next;
    setDraft(next);
    setDirty(JSON.stringify(next) !== JSON.stringify(baselineRef.current?.config));
  }, []);

  const save = useCallback(async (): Promise<SaveOutcome> => {
    const baselineConfig = baselineRef.current;
    const configToSave = draftRef.current ?? baselineConfig?.config ?? null;
    if (!configToSave || !baselineConfig) {
      return { ok: false, conflict: false };
    }

    try {
      const result = await saveConfig(baselineConfig.revision, configToSave);
      applyBaseline({
        revision: result.revision,
        config: configToSave,
        config_error: null,
        warnings: result.warnings,
      });
      setError(null);
      setValidationErrors([]);
      return { ok: true, conflict: false };
    } catch (saveError) {
      if (ipcErrorCode(saveError) === "revision_conflict") {
        // 草稿与 dirty 原样保留，只更新基线的 revision/warnings；不静默重试保存。
        try {
          const envelope = await getConfig();
          baselineRef.current = envelope;
          setBaseline(envelope);
        } catch {
          const currentRevision = ipcErrorPayload(saveError)?.current_revision;
          if (typeof currentRevision === "number" && baselineRef.current) {
            const next = { ...baselineRef.current, revision: currentRevision };
            baselineRef.current = next;
            setBaseline(next);
          }
        }
        return { ok: false, conflict: true };
      }
      const payload = ipcErrorPayload(saveError);
      const message = ipcErrorMessage(saveError);
      setError(message);
      setValidationErrors(extractDiagnostics(payload?.details));
      return { ok: false, conflict: false, error: message };
    }
  }, [applyBaseline]);

  return {
    config: draft ?? baseline?.config ?? null,
    revision: baseline?.revision ?? 0,
    warnings: baseline?.warnings ?? [],
    dirty,
    loading,
    error,
    validationErrors,
    refresh,
    update,
    save,
  };
}
