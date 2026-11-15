import { useCallback, useLayoutEffect, useRef, useState } from "react";
import { isUITheme, themes, themeClasses, themeStorageKey, type UITheme } from "./index";
import { transitionAppearance } from "./transitionAppearance";

function storedTheme(): UITheme {
  try {
    const stored = window.localStorage.getItem(themeStorageKey);
    const retired = stored === "memory" || stored === "glass";
    return isUITheme(stored) ? stored : retired ? "classic" : "pixel";
  } catch {
    return "pixel";
  }
}

export function useUITheme() {
  const [theme, setTheme] = useState<UITheme>(storedTheme);
  const currentTheme = useRef(theme);
  const themeRequest = useRef(0);

  useLayoutEffect(() => {
    document.body.classList.remove(...themeClasses);
    document.body.classList.add(...themes[theme].bodyClasses);
    document.body.dataset.uiTheme = theme;
    currentTheme.current = theme;
    try {
      window.localStorage.setItem(themeStorageKey, theme);
    } catch {
      // Theme switching remains available when browser storage is disabled.
    }
    return () => {
      document.body.classList.remove(...themeClasses);
      delete document.body.dataset.uiTheme;
    };
  }, [theme]);

  const changeTheme = useCallback((next: UITheme) => {
    if (currentTheme.current !== next) {
      currentTheme.current = next;
      const request = ++themeRequest.current;
      transitionAppearance(() => {
        if (themeRequest.current === request) {
          setTheme(next);
        }
      });
    }
  }, []);

  return [theme, changeTheme] as const;
}
