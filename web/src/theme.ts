export type Theme = "system" | "light" | "dark";

const key = "stackwright.theme";

export function savedTheme(): Theme {
  try {
    const t = window.localStorage.getItem(key);
    return t === "light" || t === "dark" ? t : "system";
  } catch {
    return "system";
  }
}

// applyTheme sets the theme on the page, and keeps it for this browser.
// "system" follows the operating system's setting.
export function applyTheme(t: Theme) {
  if (t === "system") delete document.documentElement.dataset.theme;
  else document.documentElement.dataset.theme = t;
  try {
    if (t === "system") window.localStorage.removeItem(key);
    else window.localStorage.setItem(key, t);
  } catch {
    // Storage can be refused; the choice then lasts until reload.
  }
}
