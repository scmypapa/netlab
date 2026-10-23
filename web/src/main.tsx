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
      "#eff3ff",
      "#e0e8ff",
      "#c4d2ff",
      "#a0b5fa",
      "#819af0",
      "#6480e4",
      "#4d6bd9",
      "#3655bd",
      "#2b459b",
      "#233a7e",
    ],
    dark: [
      "#e4e7ee",
      "#b9c0cd",
      "#8b95a6",
      "#5c6576",
      "#3b4351",
      "#2c333f",
      "#232a35",
      "#1b212b",
      "#151a23",
      "#10151c",
    ],
  },
  primaryShade: { light: 7, dark: 6 },
  defaultRadius: "sm",
  radius: { xs: "3px", sm: "5px", md: "7px", lg: "10px", xl: "16px" },
  fontFamily: '"Segoe UI", Inter, "PingFang SC", "Microsoft YaHei", sans-serif',
  fontFamilyMonospace: '"Cascadia Code", "SFMono-Regular", Consolas, monospace',
  fontSizes: { xs: "12px", sm: "13px", md: "14px", lg: "16px", xl: "18px" },
  headings: { fontWeight: "600" },
  components: {
    Button: {
      defaultProps: { size: "sm" },
      styles: { root: { fontWeight: 550 } },
    },
    TextInput: { defaultProps: { size: "sm" } },
    Select: { defaultProps: { size: "sm" } },
    PasswordInput: { defaultProps: { size: "sm" } },
    NumberInput: { defaultProps: { size: "sm" } },
    MultiSelect: { defaultProps: { size: "sm" } },
    Drawer: {
      styles: {
        header: { borderBottom: "1px solid var(--line)", minHeight: 56 },
        body: { paddingTop: 20 },
      },
    },
    Modal: { defaultProps: { radius: "md" } },
    Menu: {
      defaultProps: { shadow: "sm", offset: 6 },
      styles: { item: { fontSize: 13 } },
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
