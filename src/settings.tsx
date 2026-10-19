import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useState,
  type ReactNode,
} from "react";
import { getPrefs, setPrefs } from "./ipc/commands";
import { ipcErrorMessage } from "./ipc/errors";
import { isTauri } from "./portStatus";
import type { Prefs } from "./ipc/types";

export type { Prefs };

const DEFAULT_PREFS: Prefs = { language: "zh-CN", theme: "system" };

interface SettingsValue {
  prefs: Prefs;
  dark: boolean;
  error: string | null;
  updatePrefs: (patch: Partial<Prefs>) => Promise<void>;
}

const SettingsContext = createContext<SettingsValue | null>(null);

function systemPrefersDark(): boolean {
  return window.matchMedia("(prefers-color-scheme: dark)").matches;
}

export function SettingsProvider({ children }: { children: ReactNode }) {
  const [prefs, setPrefsState] = useState<Prefs>(DEFAULT_PREFS);
  const [systemDark, setSystemDark] = useState(systemPrefersDark);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    const query = window.matchMedia("(prefers-color-scheme: dark)");
    const onChange = (event: MediaQueryListEvent) => setSystemDark(event.matches);
    query.addEventListener("change", onChange);
    return () => query.removeEventListener("change", onChange);
  }, []);

  useEffect(() => {
    if (!isTauri()) {
      return;
    }
    let disposed = false;
    getPrefs()
      .then((value) => {
        if (!disposed) {
          setPrefsState(value);
          setError(null);
        }
      })
      .catch((loadError) => {
        if (!disposed) {
          setError(ipcErrorMessage(loadError));
        }
      });
    return () => {
      disposed = true;
    };
  }, []);

  const updatePrefs = useCallback(async (patch: Partial<Prefs>) => {
    if (!isTauri()) {
      setPrefsState((current) => ({ ...current, ...patch }));
      return;
    }
    try {
      const next = await setPrefs(patch);
      setPrefsState(next);
      setError(null);
    } catch (saveError) {
      setError(ipcErrorMessage(saveError));
    }
  }, []);

  const dark =
    prefs.theme === "dark" || (prefs.theme === "system" && systemDark);

  const value = useMemo<SettingsValue>(
    () => ({ prefs, dark, error, updatePrefs }),
    [prefs, dark, error, updatePrefs],
  );

  return (
    <SettingsContext.Provider value={value}>{children}</SettingsContext.Provider>
  );
}

export function useSettings(): SettingsValue {
  const value = useContext(SettingsContext);
  if (!value) {
    throw new Error("useSettings 必须在 SettingsProvider 内使用");
  }
  return value;
}
