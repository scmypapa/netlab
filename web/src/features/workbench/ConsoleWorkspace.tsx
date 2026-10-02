import { ActionIcon, Button } from "@mantine/core";
import { Maximize2, Monitor, SquareTerminal, X } from "lucide-react";
import { useEffect, useRef, useState } from "react";
import { Terminal } from "@xterm/xterm";
import { FitAddon } from "@xterm/addon-fit";
import RFB from "@novnc/novnc";
import "@xterm/xterm/css/xterm.css";
import { consoleKey, type ConsoleTab } from "./consoles";
import styles from "./ConsoleWorkspace.module.css";

export default function ConsoleWorkspace({
  environmentId,
  tabs,
  selected,
  onSelect,
  onClose,
}: {
  environmentId: string;
  tabs: ConsoleTab[];
  selected: string;
  onSelect: (key: string) => void;
  onClose: (key: string) => void;
}) {
  const workspace = useRef<HTMLElement>(null);
  return (
    <section ref={workspace} className={styles.workspace} aria-label="资产连接">
      <div className={styles.toolbar}>
        <div className={styles.tabs} role="tablist" aria-label="当前连接">
          {tabs.map((tab) => {
            const key = consoleKey(tab);
            return (
              <div
                className={`${styles.tab} ${selected === key ? styles.active : ""}`}
                key={key}
              >
                <button
                  role="tab"
                  aria-selected={selected === key}
                  aria-controls={`console-${key}`}
                  onClick={() => onSelect(key)}
                >
                  {tab.kind === "vnc" ? (
                    <Monitor size={14} />
                  ) : (
                    <SquareTerminal size={14} />
                  )}
                  {tab.name}
                  {tab.kind === "serial" && " · 串口"}
                </button>
                <ActionIcon
                  variant="subtle"
                  color="gray"
                  aria-label={`结束 ${tab.name} 连接`}
                  onClick={() => onClose(key)}
                >
                  <X size={14} />
                </ActionIcon>
              </div>
            );
          })}
        </div>
        <ActionIcon
          variant="subtle"
          color="gray"
          aria-label="全屏连接"
          onClick={() => void workspace.current?.requestFullscreen()}
        >
          <Maximize2 size={16} />
        </ActionIcon>
      </div>
      {tabs.map((tab) => (
        <ConsoleConnection
          key={consoleKey(tab)}
          tab={tab}
          environmentId={environmentId}
          active={selected === consoleKey(tab)}
        />
      ))}
    </section>
  );
}

function ConsoleConnection({
  tab,
  environmentId,
  active,
}: {
  tab: ConsoleTab;
  environmentId: string;
  active: boolean;
}) {
  const host = useRef<HTMLDivElement>(null);
  const [status, setStatus] = useState("连接中");
  const [attempt, setAttempt] = useState(0);
  const vnc = useRef<RFB | null>(null);
  const fit = useRef<FitAddon | null>(null);
  useEffect(() => {
    if (active) fit.current?.fit();
  }, [active]);
  useEffect(() => {
    const element = host.current!;
    const url = new URL(
      `/api/v1/environments/${encodeURIComponent(environmentId)}/assets/${encodeURIComponent(tab.id)}/console?kind=${tab.kind}`,
      window.location.href,
    );
    url.protocol = location.protocol === "https:" ? "wss:" : "ws:";
    setStatus("连接中");
    if (tab.kind === "vnc") {
      const connection = new RFB(element, url.href, {
        wsProtocols: ["binary"],
      });
      vnc.current = connection;
      connection.scaleViewport = true;
      connection.resizeSession = true;
      connection.addEventListener("connect", () => setStatus("已连接"));
      connection.addEventListener("disconnect", (event) =>
        setStatus(event.detail.clean ? "连接已结束" : "连接中断"),
      );
      connection.addEventListener("securityfailure", (event) =>
        setStatus(event.detail.reason ?? "控制台认证失败"),
      );
      return () => {
        connection.disconnect();
        vnc.current = null;
      };
    }
    const terminal = new Terminal({
      cursorBlink: !window.matchMedia("(prefers-reduced-motion: reduce)")
        .matches,
      fontSize: 13,
      fontFamily: "Cascadia Code, Consolas, monospace",
      theme: {
        background: "#142128",
        foreground: "#d8e6ea",
        cursor: "#66d3bb",
        selectionBackground: "#345760",
      },
    });
    const fitter = new FitAddon();
    fit.current = fitter;
    terminal.loadAddon(fitter);
    terminal.open(element);
    fitter.fit();
    const socket = new WebSocket(url);
    socket.binaryType = "arraybuffer";
    socket.onopen = () => {
      setStatus("已连接");
      fitter.fit();
      if (tab.kind === "terminal")
        socket.send(
          JSON.stringify({ cols: terminal.cols, rows: terminal.rows }),
        );
      terminal.focus();
    };
    socket.onmessage = (event) =>
      terminal.write(new Uint8Array(event.data as ArrayBuffer));
    socket.onclose = (event) =>
      setStatus(
        event.reason || (event.code === 1000 ? "连接已结束" : "连接中断"),
      );
    const input = terminal.onData((data) => {
      if (socket.readyState === WebSocket.OPEN)
        socket.send(new TextEncoder().encode(data));
    });
    const resize = terminal.onResize((size) => {
      if (tab.kind === "terminal" && socket.readyState === WebSocket.OPEN)
        socket.send(JSON.stringify(size));
    });
    const observer = new ResizeObserver(() => {
      if (element.clientWidth && element.clientHeight) fitter.fit();
    });
    observer.observe(element);
    return () => {
      input.dispose();
      resize.dispose();
      observer.disconnect();
      socket.close();
      terminal.dispose();
      fit.current = null;
    };
  }, [environmentId, tab.id, tab.kind, attempt]);
  return (
    <div
      id={`console-${consoleKey(tab)}`}
      className={styles.connection}
      role="tabpanel"
      hidden={!active}
    >
      <div className={styles.status}>
        <span
          role="status"
          className={status === "已连接" ? styles.connected : ""}
        >
          <i />
          {status}
        </span>
        {tab.kind === "vnc" && status === "已连接" && (
          <Button
            variant="subtle"
            size="compact-xs"
            onClick={() => vnc.current?.sendCtrlAltDel()}
          >
            Ctrl · Alt · Del
          </Button>
        )}
        {status !== "已连接" && status !== "连接中" && (
          <Button
            variant="subtle"
            size="compact-xs"
            onClick={() => setAttempt((value) => value + 1)}
          >
            重新连接
          </Button>
        )}
      </div>
      <div
        ref={host}
        className={`${styles.surface} ${tab.kind === "vnc" ? styles.vnc : styles.terminal}`}
      />
    </div>
  );
}
