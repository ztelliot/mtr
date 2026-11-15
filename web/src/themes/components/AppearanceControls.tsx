import { Group, UnstyledButton, useComputedColorScheme, useMantineColorScheme } from "@mantine/core";
import { Globe2, Moon, Sun } from "lucide-react";
import { useTranslation } from "react-i18next";
import { setLanguage } from "../../i18n";
import type { UITheme } from "../index";
import { transitionAppearance } from "../transitionAppearance";
import { PixelGlobe } from "./pixel/PixelGlobe";
import { ThemeMenu } from "./ThemeMenu";

export function AppearanceControls({ theme, onThemeChange, className, sidebar = false }: {
  theme: UITheme;
  onThemeChange: (theme: UITheme) => void;
  className: string;
  sidebar?: boolean;
}) {
  const { t, i18n } = useTranslation();
  const scheme = useComputedColorScheme("light");
  const { setColorScheme } = useMantineColorScheme();
  const isChinese = i18n.language === "zh-CN";
  const languageControl = (
    <UnstyledButton
      className={sidebar ? "sidebar-setting-button sidebar-language-button" : "appearance-language-button"}
      aria-label={t("appearance.toggleLanguage")}
      title={isChinese ? "Switch to English" : "切换到中文"}
      onClick={() => setLanguage(isChinese ? "en-US" : "zh-CN")}
    >
      {theme === "pixel" ? <PixelGlobe /> : <Globe2 size={19} aria-hidden="true" />}
      <span>{isChinese ? "中文" : "EN"}</span>
    </UnstyledButton>
  );
  const themeMenu = <ThemeMenu theme={theme} onThemeChange={onThemeChange} sidebar={sidebar} />;

  return (
    <Group gap="md" className={`${className} appearance-controls${sidebar ? " sidebar-settings" : ""}`}>
      {sidebar ? (
        <>
          {languageControl}
          {themeMenu}
          <UnstyledButton
            className="sidebar-setting-button"
            aria-label={t("appearance.toggleScheme")}
            aria-pressed={scheme === "dark"}
            title={scheme === "dark" ? t("appearance.useLight") : t("appearance.useDark")}
            onClick={() => transitionAppearance(() => setColorScheme(scheme === "dark" ? "light" : "dark"))}
          >
            {scheme === "dark" ? <Moon size={19} aria-hidden="true" /> : <Sun size={19} aria-hidden="true" />}
            <span>{t(`theme.${scheme}`)}</span>
          </UnstyledButton>
        </>
      ) : (
        <>
          <div className="appearance-language">{languageControl}</div>
          <div className="appearance-actions">{themeMenu}</div>
        </>
      )}
    </Group>
  );
}
