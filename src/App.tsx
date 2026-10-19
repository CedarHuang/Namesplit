import { lazy, Suspense } from "react";
import { App as AntdApp, ConfigProvider, Spin, theme as antdTheme } from "antd";
import zhCN from "antd/locale/zh_CN";
import { HashRouter, Navigate, Route, Routes } from "react-router-dom";

import { AppShell } from "./app/AppShell";
import { FirstRunGate } from "./app/FirstRunGate";
import { I18nProvider } from "./i18n";
import { SettingsProvider, useSettings } from "./settings";
import { AppStateProvider } from "./state/appState";
import { UnsavedChangesProvider } from "./state/unsavedGuard";

const OverviewPage = lazy(() =>
  import("./pages/Overview").then((module) => ({ default: module.OverviewPage })),
);
const LogsPage = lazy(() =>
  import("./pages/Logs").then((module) => ({ default: module.LogsPage })),
);
const RulesPage = lazy(() =>
  import("./pages/Rules").then((module) => ({ default: module.RulesPage })),
);
const UpstreamsPage = lazy(() =>
  import("./pages/Upstreams").then((module) => ({ default: module.UpstreamsPage })),
);
const LocalRecordsPage = lazy(() =>
  import("./pages/LocalRecords").then((module) => ({ default: module.LocalRecordsPage })),
);
const DiagnosticsPage = lazy(() =>
  import("./pages/Diagnostics").then((module) => ({ default: module.DiagnosticsPage })),
);
const SettingsPage = lazy(() =>
  import("./pages/Settings").then((module) => ({ default: module.SettingsPage })),
);
const WizardPage = lazy(() =>
  import("./pages/Wizard").then((module) => ({ default: module.WizardPage })),
);

export default function App() {
  return (
    <SettingsProvider>
      <ThemedApp />
    </SettingsProvider>
  );
}

function ThemedApp() {
  const { prefs, dark } = useSettings();

  return (
    <ConfigProvider
      locale={zhCN}
      theme={{ algorithm: dark ? antdTheme.darkAlgorithm : antdTheme.defaultAlgorithm }}
    >
      <AntdApp>
        <UnsavedChangesProvider>
          <I18nProvider language={prefs.language}>
            <AppStateProvider>
              <HashRouter>
                <FirstRunGate />
                <Suspense
                  fallback={
                    <div style={{ display: "flex", justifyContent: "center", padding: 48 }}>
                      <Spin />
                    </div>
                  }
                >
                  <Routes>
                    <Route element={<AppShell />}>
                      <Route path="/" element={<Navigate to="/overview" replace />} />
                      <Route path="/overview" element={<OverviewPage />} />
                      <Route path="/logs" element={<LogsPage />} />
                      <Route path="/rules" element={<RulesPage />} />
                      <Route path="/upstreams" element={<UpstreamsPage />} />
                      <Route path="/local" element={<LocalRecordsPage />} />
                      <Route path="/settings" element={<SettingsPage />} />
                      <Route path="/diagnostics" element={<DiagnosticsPage />} />
                      <Route path="*" element={<Navigate to="/overview" replace />} />
                    </Route>
                    <Route path="/wizard" element={<WizardPage />} />
                  </Routes>
                </Suspense>
              </HashRouter>
            </AppStateProvider>
          </I18nProvider>
        </UnsavedChangesProvider>
      </AntdApp>
    </ConfigProvider>
  );
}
