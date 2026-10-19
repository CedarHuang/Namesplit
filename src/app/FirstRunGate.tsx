import { useEffect } from "react";
import { useLocation, useNavigate } from "react-router-dom";

const STORAGE_KEY = "namesplit.wizard.completed";

export function isWizardCompleted(): boolean {
  try {
    return localStorage.getItem(STORAGE_KEY) === "1";
  } catch {
    return true;
  }
}

export function markWizardCompleted(): void {
  try {
    localStorage.setItem(STORAGE_KEY, "1");
  } catch {
    // 存储不可用时退化为每次都显示向导，不影响主流程。
  }
}

/** 首次启动时把用户带到向导；已完成或正在向导时不干预导航。 */
export function FirstRunGate() {
  const navigate = useNavigate();
  const location = useLocation();

  useEffect(() => {
    if (location.pathname === "/wizard" || isWizardCompleted()) {
      return;
    }
    navigate("/wizard", { replace: true });
  }, [location.pathname, navigate]);

  return null;
}
