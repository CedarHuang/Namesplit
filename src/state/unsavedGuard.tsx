import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useRef,
  type ReactNode,
} from "react";

interface UnsavedChangesApi {
  setDirty: (id: symbol, dirty: boolean) => void;
  hasUnsaved: () => boolean;
}

const UnsavedChangesContext = createContext<UnsavedChangesApi | null>(null);

/**
 * 汇总各页面注册的未保存修改状态。
 *
 * 脏状态只影响“是否拦截导航”这一瞬时判断，不参与渲染，用引用集合保存，
 * 避免每次输入都触发 Provider 及其子树重渲染。
 */
export function UnsavedChangesProvider({ children }: { children: ReactNode }) {
  const dirtyIds = useRef<Set<symbol>>(new Set());

  const setDirty = useCallback((id: symbol, dirty: boolean) => {
    if (dirty) {
      dirtyIds.current.add(id);
    } else {
      dirtyIds.current.delete(id);
    }
  }, []);

  const hasUnsaved = useCallback(() => dirtyIds.current.size > 0, []);

  const value = useMemo<UnsavedChangesApi>(
    () => ({ setDirty, hasUnsaved }),
    [setDirty, hasUnsaved],
  );

  return (
    <UnsavedChangesContext.Provider value={value}>
      {children}
    </UnsavedChangesContext.Provider>
  );
}

function useUnsavedChangesApi(): UnsavedChangesApi {
  const value = useContext(UnsavedChangesContext);
  if (!value) {
    throw new Error("useUnsavedChanges 必须在 UnsavedChangesProvider 内使用");
  }
  return value;
}

/** 页面注册未保存修改状态；dirty 变化或组件卸载时自动注销，重复注册不会残留。 */
export function useUnsavedChanges(dirty: boolean): void {
  const { setDirty } = useUnsavedChangesApi();
  // 同一个 hook 实例始终用同一个 id，重复注册只会覆盖状态而不会累积条目。
  const idRef = useRef<symbol>(Symbol("unsaved"));

  useEffect(() => {
    const id = idRef.current;
    setDirty(id, dirty);
    return () => setDirty(id, false);
  }, [dirty, setDirty]);
}

export interface UnsavedChangesGuard {
  hasUnsaved: () => boolean;
}

/** AppShell 用于在导航前判断当前页面是否有未保存修改。 */
export function useUnsavedChangesGuard(): UnsavedChangesGuard {
  const { hasUnsaved } = useUnsavedChangesApi();
  return useMemo<UnsavedChangesGuard>(() => ({ hasUnsaved }), [hasUnsaved]);
}
