import React from "react";
import ReactDOM from "react-dom/client";
import { MantineProvider, localStorageColorSchemeManager } from "@mantine/core";
import { Notifications } from "@mantine/notifications";
import { App } from "./App";
import "@mantine/core/styles/global.css";
import "@mantine/core/styles/baseline.css";
import "@mantine/core/styles/default-css-variables.css";
import "@mantine/core/styles/ActionIcon.css";
import "@mantine/core/styles/Alert.css";
import "@mantine/core/styles/Anchor.css";
import "@mantine/core/styles/Badge.css";
import "@mantine/core/styles/Button.css";
import "@mantine/core/styles/CloseButton.css";
import "@mantine/core/styles/Combobox.css";
import "@mantine/core/styles/Container.css";
import "@mantine/core/styles/Grid.css";
import "@mantine/core/styles/Group.css";
import "@mantine/core/styles/InlineInput.css";
import "@mantine/core/styles/Input.css";
import "@mantine/core/styles/Loader.css";
import "@mantine/core/styles/Modal.css";
import "@mantine/core/styles/ModalBase.css";
import "@mantine/core/styles/Notification.css";
import "@mantine/core/styles/NumberInput.css";
import "@mantine/core/styles/Overlay.css";
import "@mantine/core/styles/Paper.css";
import "@mantine/core/styles/PasswordInput.css";
import "@mantine/core/styles/PillsInput.css";
import "@mantine/core/styles/Popover.css";
import "@mantine/core/styles/ScrollArea.css";
import "@mantine/core/styles/Scroller.css";
import "@mantine/core/styles/SegmentedControl.css";
import "@mantine/core/styles/SimpleGrid.css";
import "@mantine/core/styles/Stack.css";
import "@mantine/core/styles/Switch.css";
import "@mantine/core/styles/Table.css";
import "@mantine/core/styles/Tabs.css";
import "@mantine/core/styles/Text.css";
import "@mantine/core/styles/Title.css";
import "@mantine/core/styles/Tooltip.css";
import "@mantine/core/styles/UnstyledButton.css";
import "@mantine/notifications/styles.css";
import "./i18n";
import "./styles.css";

const colorSchemeManager = localStorageColorSchemeManager({
  key: "mtr.color-scheme"
});

ReactDOM.createRoot(document.getElementById("root") as HTMLElement).render(
  <React.StrictMode>
    <MantineProvider
      colorSchemeManager={colorSchemeManager}
      defaultColorScheme="auto"
      theme={{
        fontFamily:
          "Inter, ui-sans-serif, system-ui, -apple-system, BlinkMacSystemFont, Segoe UI, sans-serif",
        primaryColor: "dark",
        radius: {
          xs: "4px",
          sm: "6px",
          md: "8px",
          lg: "8px",
          xl: "8px"
        }
      }}
    >
      <Notifications position="top-right" />
      <App />
    </MantineProvider>
  </React.StrictMode>
);
