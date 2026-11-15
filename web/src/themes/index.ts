// Classic uses the shared styles.css baseline. Keep IDs and the storage key
// stable so existing browser preferences continue to work.
export const themeStorageKey = "mtr.debug-theme";
export const themes = {
  pixel: {
    bodyClasses: ["theme-pixel"],
    navigation: "top",
    navIconSize: null,
    swatches: ["#f8fafb", "#dbe8e6", "#427e78"]
  },
  classic: {
    bodyClasses: [],
    navigation: "top",
    navIconSize: null,
    swatches: ["#ffffff", "#eef0f3", "#263445"]
  },
  metro: {
    bodyClasses: ["theme-metro"],
    navigation: "top",
    navIconSize: 26,
    swatches: ["#f5f7fa", "#edf4fa", "#006bb3"]
  },
  fluent: {
    bodyClasses: ["theme-modern", "theme-fluent"],
    navigation: "sidebar",
    navIconSize: 18,
    swatches: ["#f3f3f3", "#ffffff", "#0067c0"]
  },
  material: {
    bodyClasses: ["theme-modern", "theme-material"],
    navigation: "sidebar",
    navIconSize: 20,
    swatches: ["#fffbfe", "#eaddff", "#6750a4"]
  },
  p5r: {
    bodyClasses: ["theme-modern", "theme-p5r"],
    navigation: "top",
    navIconSize: null,
    swatches: ["#111111", "#f8f4e9", "#df102b"]
  }
} as const;

export type UITheme = keyof typeof themes;
export const themeIds = Object.keys(themes) as UITheme[];
export const themeClasses = [...new Set(themeIds.flatMap((id) => [...themes[id].bodyClasses]))];

export function isUITheme(value: unknown): value is UITheme {
  return typeof value === "string" && Object.prototype.hasOwnProperty.call(themes, value);
}
