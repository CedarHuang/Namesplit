import { zhCN } from "./i18n/zh-CN";

/** 优先使用同步的旧式复制，避免 WebView2 弹出剪贴板权限提示；失败再回退到异步 API。 */
export async function copyText(text: string): Promise<void> {
  const textarea = document.createElement("textarea");
  textarea.value = text;
  textarea.setAttribute("readonly", "");
  textarea.style.position = "fixed";
  textarea.style.opacity = "0";
  textarea.style.pointerEvents = "none";
  document.body.appendChild(textarea);
  textarea.select();
  let copied = false;
  try {
    copied = document.execCommand("copy");
  } catch {
    copied = false;
  } finally {
    textarea.remove();
  }
  if (copied) {
    return;
  }
  if (!navigator.clipboard) {
    throw new Error(zhCN["common.clipboardUnavailable"]);
  }
  await navigator.clipboard.writeText(text);
}
