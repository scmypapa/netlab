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
      "#edf3ff",
      "#dce8ff",
      "#b7ceff",
      "#91b2fc",
      "#7199f1",
      "#4e7ee5",
      "#3469d7",
      "#255acb",
      "#1e49a6",
      "#1b3e86",
    ],
    dark: [
      "#e6e9ef",
      "#bdc4ce",
      "#969fac",
      "#6b7584",
      "#454e5b",
      "#343c48",
      "#272e38",
      "#1c2129",
      "#15191f",
      "#101318",
    ],
  },
  primaryShade: { light: 7, dark: 6 },
  defaultRadius: "sm",
  radius: { xs: "3px", sm: "5px", md: "7px", lg: "10px", xl: "16px" },
  fontFamily: 'Inter, "Segoe UI", "PingFang SC", "Microsoft YaHei", sans-serif',
  fontFamilyMonospace: '"Cascadia Code", "SFMono-Regular", Consolas, monospace',
  fontSizes: { xs: "12px", sm: "13px", md: "14px", lg: "16px", xl: "18px" },
  headings: { fontWeight: "600" },
  components: {
    Button: {
      defaultProps: { size: "sm" },
      styles: { root: { fontWeight: 550, letterSpacing: 0 } },
    },
    TextInput: { defaultProps: { size: "sm" } },
    Select: { defaultProps: { size: "sm" } },
    PasswordInput: { defaultProps: { size: "sm" } },
    NumberInput: { defaultProps: { size: "sm" } },
    MultiSelect: { defaultProps: { size: "sm" } },
    InputWrapper: { styles: { label: { fontWeight: 500 } } },
    Drawer: {
      styles: {
        header: { borderBottom: "1px solid var(--line)", minHeight: 58 },
        title: { fontSize: 16, fontWeight: 600 },
        body: { paddingTop: 24 },
      },
    },
    Modal: {
      defaultProps: { radius: "md" },
      styles: { title: { fontSize: 16, fontWeight: 600 } },
    },
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
