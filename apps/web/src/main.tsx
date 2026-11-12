import { initializeTheme, ThemeProvider } from "@persona/ui/theme";
import { IconContext } from "@phosphor-icons/react/dist/lib/context";
import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { ChatPage } from "./features/chat/page.tsx";
import { HostingRoot } from "./features/hosting/root.tsx";
import { I18nProvider } from "./features/i18n/context.tsx";
import "@fontsource-variable/geist/wght.css";
import "@fontsource-variable/noto-sans-sc/wght.css";
import "@persona/ui/style.css";
import "./features/chat/style.css";

const root = document.getElementById("root");
const initialPreference = initializeTheme();
if (root)
  createRoot(root).render(
    <StrictMode>
      <IconContext value={{ size: 18, weight: "regular", "aria-hidden": true }}>
        <ThemeProvider initialPreference={initialPreference}>
          <I18nProvider>
            <HostingRoot>
              <ChatPage />
            </HostingRoot>
          </I18nProvider>
        </ThemeProvider>
      </IconContext>
    </StrictMode>,
  );
