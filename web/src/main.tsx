import React from "react";
import ReactDOM from "react-dom/client";
import { MantineProvider, createTheme } from "@mantine/core";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { BrowserRouter } from "react-router-dom";
import "@mantine/core/styles.css";
import "@xyflow/react/dist/style.css";
import "./styles.css";
import { App } from "./app/App";

const queryClient = new QueryClient({
  defaultOptions: {
    queries: { retry: false, staleTime: 10_000, refetchOnWindowFocus: false },
  },
});
const theme = createTheme({
  primaryColor: "blue",
  colors: {
    blue: [
      "#edf6fc",
      "#dceefa",
      "#b7d9f1",
      "#8cc2e7",
      "#65addb",
      "#4599cf",
      "#3389c2",
      "#287bb5",
      "#216596",
      "#1c527a",
    ],
    dark: [
      "#e8e9eb",
      "#c5c8ce",
      "#a8adb5",
      "#818892",
      "#60666e",
      "#45494f",
      "#32353a",
      "#27292d",
      "#222428",
      "#1d1f22",
    ],
  },
  primaryShade: { light: 7, dark: 7 },
  defaultRadius: "sm",
  radius: { xs: "4px", sm: "6px", md: "8px", lg: "10px", xl: "16px" },
  fontFamily: "var(--font-ui)",
  fontFamilyMonospace: "var(--font-mono)",
  fontSizes: { xs: "12px", sm: "14px", md: "14px", lg: "16px", xl: "18px" },
  headings: { fontWeight: "600" },
  components: {
    Button: {
      defaultProps: { size: "sm" },
      styles: { root: { fontWeight: 400, letterSpacing: 0 } },
    },
    TextInput: { defaultProps: { size: "sm" } },
    Select: { defaultProps: { size: "sm" } },
    PasswordInput: { defaultProps: { size: "sm" } },
    NumberInput: { defaultProps: { size: "sm" } },
    MultiSelect: { defaultProps: { size: "sm" } },
    InputWrapper: { styles: { label: { fontWeight: 400 } } },
    Drawer: {
      styles: {
        content: { background: "var(--surface)" },
        header: {
          background: "var(--surface)",
          borderBottom: "1px solid var(--line)",
          minHeight: 58,
        },
        title: { fontSize: 16, fontWeight: 600 },
        body: { paddingTop: 24 },
      },
    },
    Modal: {
      defaultProps: { radius: "md" },
      styles: {
        content: { background: "var(--surface)" },
        header: { background: "var(--surface)" },
        title: { fontSize: 16, fontWeight: 600 },
      },
    },
    Menu: {
      defaultProps: { shadow: "sm", offset: 6 },
      styles: {
        dropdown: { background: "var(--surface)", borderColor: "var(--line)" },
        item: { fontSize: 14 },
      },
    },
  },
});

ReactDOM.createRoot(document.getElementById("root")!).render(
  <React.StrictMode>
    <MantineProvider theme={theme} defaultColorScheme="auto">
      <QueryClientProvider client={queryClient}>
        <BrowserRouter>
          <App />
        </BrowserRouter>
      </QueryClientProvider>
    </MantineProvider>
  </React.StrictMode>,
);
