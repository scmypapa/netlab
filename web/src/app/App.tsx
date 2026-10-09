import {
  ActionIcon,
  Avatar,
  Button,
  Menu,
  PasswordInput,
  TextInput,
  useComputedColorScheme,
  useMantineColorScheme,
} from "@mantine/core";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  Boxes,
  ChevronDown,
  Layers3,
  LogOut,
  Moon,
  Server,
  Sun,
  UsersRound,
  Download,
} from "lucide-react";
import { lazy, Suspense, useEffect, useState } from "react";
import { NavLink, Route, Routes } from "react-router-dom";
import { api, ApiError } from "../api/client";
import { ErrorMessage, Loading } from "../foundation/Feedback";
import { EnvironmentsPage } from "../features/environments/EnvironmentsPage";
const TemplatesPage = lazy(() =>
  import("../features/templates/TemplatesPage").then((module) => ({
    default: module.TemplatesPage,
  })),
);
const NodesPage = lazy(() =>
  import("../features/nodes/NodesPage").then((module) => ({
    default: module.NodesPage,
  })),
);
const WorkbenchPage = lazy(() =>
  import("../features/workbench/WorkbenchPage").then((module) => ({
    default: module.WorkbenchPage,
  })),
);
const AccountsPage = lazy(() =>
  import("../features/access/AccountsPage").then((module) => ({
    default: module.AccountsPage,
  })),
);
const UpdatePanel = lazy(() =>
  import("../features/system/UpdatePanel").then((module) => ({
    default: module.UpdatePanel,
  })),
);

export function App() {
  const [updatesOpen, setUpdatesOpen] = useState(false);
  const identity = useQuery({ queryKey: ["identity"], queryFn: api.identity });
  const scheme = useComputedColorScheme("light");
  const { setColorScheme } = useMantineColorScheme();
  const client = useQueryClient();
  const logout = useMutation({
    mutationFn: api.logout,
    onSuccess: () => {
      client.clear();
      void identity.refetch();
    },
  });
  if (identity.isPending) return <Loading />;
  if (identity.error instanceof ApiError && identity.error.status === 401)
    return <Login />;
  if (identity.error)
    return (
      <div className="login-shell">
        <ErrorMessage error={identity.error} />
        <Button variant="default" onClick={() => void identity.refetch()}>
          重新连接
        </Button>
      </div>
    );
  return (
    <div className="app-shell">
      <header className="global-header">
        <NavLink to="/environments" className="brand" aria-label="Netlab 首页">
          <img
            className="brand-mark"
            src="/netlab-icon.png"
            alt=""
            width={32}
            height={32}
          />
          <span>
            netlab<span className="brand-period">.</span>
          </span>
        </NavLink>
        <nav className="primary-nav" aria-label="主导航">
          <NavLink to="/environments">
            <Layers3 size={17} />
            环境
          </NavLink>
          <NavLink to="/templates">
            <Boxes size={17} />
            模板
          </NavLink>
          {identity.data?.administrator && (
            <NavLink to="/resources">
              <Server size={17} />
              资源
            </NavLink>
          )}
        </nav>
        <div className="global-tools">
          <ActionIcon
            variant="subtle"
            color="gray"
            aria-label={scheme === "dark" ? "切换日间主题" : "切换夜间主题"}
            onClick={() => setColorScheme(scheme === "dark" ? "light" : "dark")}
          >
            {scheme === "dark" ? <Sun size={18} /> : <Moon size={18} />}
          </ActionIcon>
          <Menu position="bottom-end">
            <Menu.Target>
              <button className="profile">
                <Avatar size={28} radius="xl" color="teal">
                  {identity.data?.name.slice(0, 1).toUpperCase()}
                </Avatar>
                <span>{identity.data?.name}</span>
                <ChevronDown size={14} />
              </button>
            </Menu.Target>
            <Menu.Dropdown>
              {identity.data?.administrator && (
                <Menu.Item
                  leftSection={<Download size={15} />}
                  onClick={() => setUpdatesOpen(true)}
                >
                  系统更新
                </Menu.Item>
              )}
              {identity.data?.administrator && (
                <Menu.Item
                  component={NavLink}
                  to="/accounts"
                  leftSection={<UsersRound size={15} />}
                >
                  访问管理
                </Menu.Item>
              )}
              <Menu.Item
                leftSection={<LogOut size={15} />}
                onClick={() => logout.mutate()}
              >
                退出登录
              </Menu.Item>
            </Menu.Dropdown>
          </Menu>
        </div>
      </header>
      <Suspense fallback={null}>
        {updatesOpen && <UpdatePanel onClose={() => setUpdatesOpen(false)} />}
      </Suspense>
      <Suspense fallback={<Loading />}>
        <Routes>
          <Route path="/environments/:id" element={<WorkbenchPage />} />
          <Route path="/templates" element={<TemplatesPage />} />
          <Route path="/resources" element={<NodesPage />} />
          <Route path="/resources/storage" element={<NodesPage />} />
          <Route path="/accounts" element={<AccountsPage />} />
          <Route path="*" element={<EnvironmentsPage />} />
        </Routes>
      </Suspense>
    </div>
  );
}

function Login() {
  const [name, setName] = useState("");
  const [password, setPassword] = useState("");
  const client = useQueryClient();
  useEffect(() => {
    const business = {
      predicate: (query: { queryKey: readonly unknown[] }) =>
        query.queryKey[0] !== "identity",
    };
    void client.cancelQueries(business);
    client.removeQueries(business);
  }, [client]);
  const login = useMutation({
    mutationFn: () => api.login(name, password),
    onSuccess: async (identity) => {
      await client.cancelQueries();
      client.removeQueries({
        predicate: (query) => query.queryKey[0] !== "identity",
      });
      client.setQueryData(["identity"], identity);
    },
  });
  return (
    <main className="login-shell">
      <div className="login-brand">
        <img src="/netlab-icon.png" alt="" width={48} height={48} />
        <span>netlab.</span>
      </div>
      <form
        className="login-form"
        onSubmit={(event) => {
          event.preventDefault();
          login.mutate();
        }}
      >
        <h1>欢迎回来</h1>
        <TextInput
          label="账号"
          autoComplete="username"
          value={name}
          onChange={(event) => setName(event.currentTarget.value)}
          required
        />
        <PasswordInput
          label="密码"
          autoComplete="current-password"
          value={password}
          onChange={(event) => setPassword(event.currentTarget.value)}
          required
        />
        <ErrorMessage error={login.error} />
        <Button fullWidth type="submit" loading={login.isPending}>
          登录
        </Button>
      </form>
    </main>
  );
}
