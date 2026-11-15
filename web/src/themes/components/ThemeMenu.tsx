import { ActionIcon, Menu, UnstyledButton, useComputedColorScheme, useMantineColorScheme } from "@mantine/core";
import { Check, Moon, Palette, Sun } from "lucide-react";
import { useTranslation } from "react-i18next";
import { themeIds, type UITheme } from "../index";
import { transitionAppearance } from "../transitionAppearance";
import { P5CutoutText } from "./p5r/P5CutoutText";
import { ThemePreview } from "./ThemePreview";

export function ThemeMenu({ theme, onThemeChange, sidebar }: {
  theme: UITheme;
  onThemeChange: (theme: UITheme) => void;
  sidebar: boolean;
}) {
  const { t } = useTranslation();
  const scheme = useComputedColorScheme("light");
  const { setColorScheme } = useMantineColorScheme();
  const label = (text: string) => theme === "p5r" ? <P5CutoutText text={text} variant="label" /> : text;

  return (
    <Menu position={sidebar ? "right-end" : "bottom-end"} width={320}>
      <Menu.Target>
        {sidebar ? (
          <UnstyledButton
            className="sidebar-setting-button"
            aria-label={t("appearance.selectTheme")}
            title={t("appearance.selectTheme")}
          >
            <Palette size={19} aria-hidden="true" />
            <span>{t("appearance.theme")}</span>
          </UnstyledButton>
        ) : (
          <ActionIcon
            variant="default"
            className="appearance-trigger"
            aria-label={t("appearance.selectTheme")}
            title={t("appearance.selectTheme")}
          >
            <Palette size={19} aria-hidden="true" />
          </ActionIcon>
        )}
      </Menu.Target>
      <Menu.Dropdown className="appearance-menu theme-menu" data-has-modes={!sidebar}>
        <Menu.Label>{label(t("appearance.selectTheme"))}</Menu.Label>
        <div className="theme-choice-list" role="group" aria-label={t("appearance.selectTheme")}>
          {themeIds.map((id) => (
            <Menu.Item
              key={id}
              data-theme-id={id}
              data-selected={theme === id || undefined}
              aria-checked={theme === id}
              renderRoot={(props) => <button {...props} role="menuitemradio" />}
              leftSection={<ThemePreview theme={id} />}
              rightSection={theme === id ? <Check size={17} aria-hidden="true" /> : <span className="theme-check-space" />}
              onClick={() => onThemeChange(id)}
            >
              <span className="theme-option-name">{t(`appearance.themes.${id}`)}</span>
              <span className="theme-option-description">{t(`appearance.descriptions.${id}`)}</span>
            </Menu.Item>
          ))}
        </div>
        {!sidebar && (
          <>
            <Menu.Divider />
            <Menu.Label>{label(t("appearance.colorMode"))}</Menu.Label>
            <div className="theme-scheme-options" role="group" aria-label={t("appearance.colorMode")}>
              {(["light", "dark"] as const).map((mode) => (
                <Menu.Item
                  key={mode}
                  data-color-mode={mode}
                  data-selected={scheme === mode || undefined}
                  aria-checked={scheme === mode}
                  renderRoot={(props) => <button {...props} role="menuitemradio" />}
                  closeMenuOnClick={false}
                  leftSection={mode === "light" ? <Sun size={17} /> : <Moon size={17} />}
                  rightSection={scheme === mode ? <Check size={15} /> : undefined}
                  onClick={() => transitionAppearance(() => setColorScheme(mode))}
                >
                  {t(`theme.${mode}`)}
                </Menu.Item>
              ))}
            </div>
          </>
        )}
      </Menu.Dropdown>
    </Menu>
  );
}
