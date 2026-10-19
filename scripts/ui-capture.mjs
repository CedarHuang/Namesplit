// 通过 WebView2 的 CDP 调试端口抓取 UI 截图，仅用于开发期验收证据。
// 用法：node scripts/ui-capture.mjs <debug-port> [output-dir]
import { mkdir, writeFile } from "node:fs/promises";
import { join } from "node:path";

const port = process.argv[2] ?? "9223";
const outputDir = process.argv[3] ?? "build/evidence/u3";
const routes = [
  ["overview", "#/overview"],
  ["logs", "#/logs"],
  ["rules", "#/rules"],
  ["upstreams", "#/upstreams"],
  ["local", "#/local"],
  ["settings", "#/settings"],
  ["diagnostics", "#/diagnostics"],
  ["wizard", "#/wizard"],
];

const targets = await (await fetch(`http://127.0.0.1:${port}/json`)).json();
const page = targets.find((target) => target.type === "page");
if (!page) {
  throw new Error(`没有找到页面目标：${JSON.stringify(targets)}`);
}

const socket = new WebSocket(page.webSocketDebuggerUrl);
let nextId = 1;
const pending = new Map();

socket.addEventListener("message", (event) => {
  const message = JSON.parse(event.data);
  if (message.id && pending.has(message.id)) {
    pending.get(message.id)(message);
    pending.delete(message.id);
  }
});

function send(method, params = {}) {
  return new Promise((resolve, reject) => {
    const id = nextId++;
    pending.set(id, (message) => {
      if (message.error) {
        reject(new Error(`${method}: ${JSON.stringify(message.error)}`));
      } else {
        resolve(message.result);
      }
    });
    socket.send(JSON.stringify({ id, method, params }));
  });
}

function delay(ms) {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

async function evaluate(expression, timeoutMs = 15_000) {
  const result = await Promise.race([
    send("Runtime.evaluate", { expression, awaitPromise: true, returnByValue: true }),
    delay(timeoutMs).then(() => ({ result: { value: "__timeout__" } })),
  ]);
  if (result.exceptionDetails) {
    throw new Error(result.exceptionDetails.text);
  }
  if (result.result?.value === "__timeout__") {
    throw new Error("页面求值超时");
  }
  return result.result?.value;
}

// 路由内容就绪判定：懒加载 chunk 已完成、没有 antd 加载态、内容区有高度，
// 再等两帧让布局稳定。固定等待时间在慢机器上会截到加载态，不能作为就绪依据。
async function waitForRoute(hash) {
  const ready = await evaluate(`new Promise((resolve) => {
    const deadline = Date.now() + 10000;
    const settle = () => requestAnimationFrame(() => requestAnimationFrame(() => resolve(true)));
    const check = () => {
      const content = document.querySelector(".ant-layout-content");
      const spinning = document.querySelector(".ant-spin-spinning");
      if (
        location.hash === ${JSON.stringify(hash)} &&
        !spinning &&
        content &&
        content.getBoundingClientRect().height > 0
      ) {
        settle();
        return;
      }
      if (Date.now() > deadline) {
        resolve(false);
        return;
      }
      setTimeout(check, 50);
    };
    check();
  })`);
  if (!ready) {
    throw new Error(`路由 ${hash} 在 10s 内未就绪`);
  }
}

// 虚拟表格只在视口内渲染行；截图前必须确认表格当前没有停在“空白帧”上。
// 内容行数为 0 但存在空态占位（如无数据）时不视为异常。
async function ensureTableRendered() {
  const hasVirtualBody = await evaluate(
    `Boolean(document.querySelector(".ant-table-tbody-virtual-holder"))`,
  );
  if (!hasVirtualBody) {
    return;
  }
  for (let attempt = 0; attempt < 5; attempt += 1) {
    const state = await evaluate(`(() => {
      const holder = document.querySelector(".ant-table-tbody-virtual-holder");
      const rows = Array.from(document.querySelectorAll(".ant-table-row"));
      if (!holder) return { rows: rows.length, visible: 0, empty: true };
      const box = holder.getBoundingClientRect();
      const visible = rows.filter((row) => {
        const rect = row.getBoundingClientRect();
        return rect.height > 0 && rect.bottom > box.top && rect.top < box.bottom;
      }).length;
      const empty = Boolean(document.querySelector(".ant-table-placeholder .ant-empty"));
      return { rows: rows.length, visible, empty };
    })()`);
    if (state.visible > 0 || (state.rows === 0 && state.empty)) {
      return;
    }
    // 虚拟列表的滚动状态与渲染可能短暂不同步：滚到底再等一帧让它重新计算可视区间。
    await evaluate(`(() => {
      const holder = document.querySelector(".ant-table-tbody-virtual-holder");
      if (!holder) return;
      holder.scrollTop = holder.scrollHeight;
      holder.dispatchEvent(new Event("scroll"));
    })()`);
    await delay(300);
  }
  throw new Error("日志表格没有渲染出可见行，拒绝写入空白截图");
}

// 整页截图：先临时把视口扩到内容高度，再截“视口”。直接使用
// Page.captureScreenshot({captureBeyondViewport:true}) 会在截图时触发一次重排，
// 虚拟列表借此把滚动位置复位，复位中间帧会被截成空白表体。
async function capture(path) {
  const size = await evaluate(`({
    width: window.innerWidth,
    height: Math.max(document.documentElement.scrollHeight, window.innerHeight),
  })`);
  await send("Emulation.setDeviceMetricsOverride", {
    width: size.width,
    height: size.height,
    deviceScaleFactor: 1,
    mobile: false,
  });
  await delay(200);
  await ensureTableRendered();
  const screenshot = await send("Page.captureScreenshot", {
    format: "png",
    captureBeyondViewport: false,
  });
  await send("Emulation.clearDeviceMetricsOverride");
  await writeFile(path, Buffer.from(screenshot.data, "base64"));
  console.log(`captured ${path}`);
}

await new Promise((resolve) => socket.addEventListener("open", resolve));
await send("Page.enable");
await send("Runtime.enable");
await evaluate(
  "window.__TAURI_INTERNALS__.invoke('prefs_set', { theme: 'light', language: 'zh-CN' })",
);
await mkdir(outputDir, { recursive: true });

for (const [name, hash] of routes) {
  await evaluate(`location.hash = ${JSON.stringify(hash)}`);
  await waitForRoute(hash);
  await delay(400);
  await capture(join(outputDir, `${name}.png`));
}

socket.close();
