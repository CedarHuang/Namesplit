import { createContext, useContext, useMemo, type ReactNode } from "react";

import { zhCN, type MessageKey } from "./zh-CN";

export type { MessageKey };

type Variables = Record<string, string | number>;

interface I18nValue {
  language: string;
  t: (key: MessageKey, variables?: Variables) => string;
}

const I18nContext = createContext<I18nValue | null>(null);

/** 文案取值与变量替换：所有界面文案统一从 zhCN 词典进入。 */
export function I18nProvider(props: { language: string; children: ReactNode }) {
  const value = useMemo<I18nValue>(() => {
    return {
      language: props.language,
      t: (key, variables) => {
        let text: string = zhCN[key];
        if (variables) {
          for (const [name, replacement] of Object.entries(variables)) {
            text = text.replaceAll(`{${name}}`, String(replacement));
          }
        }
        return text;
      },
    };
  }, [props.language]);

  return <I18nContext.Provider value={value}>{props.children}</I18nContext.Provider>;
}

export function useI18n(): I18nValue {
  const value = useContext(I18nContext);
  if (!value) {
    throw new Error("useI18n 必须在 I18nProvider 内使用");
  }
  return value;
}
