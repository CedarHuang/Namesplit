import react from "@vitejs/plugin-react";
import { defineConfig } from "vite";

// Tauri 在移动端调试时会注入 TAURI_DEV_HOST；桌面端为 undefined。
const host = process.env.TAURI_DEV_HOST;

// https://vite.dev/config/
export default defineConfig({
  plugins: [react()],
  // 避免 Vite 清屏覆盖 Tauri CLI 的报错信息。
  clearScreen: false,
  server: {
    port: 1420,
    strictPort: true,
    host: host || false,
    hmr: host
      ? {
          protocol: "ws",
          host,
          port: 1421,
        }
      : undefined,
    watch: {
      ignored: ["**/src-tauri/**"],
    },
  },
  test: {
    // 构建产物与 E2E 临时目录里的源码副本不应被当作测试收集。
    exclude: [
      "**/node_modules/**",
      "**/dist/**",
      "**/build/**",
      "**/src-tauri/**",
    ],
  },
});
