import { useEffect } from "react";
import { useMantineColorScheme } from "@mantine/core";
import { useLocation } from "react-router-dom";

export function HostBridge() {
  const location = useLocation();
  const { setColorScheme } = useMantineColorScheme();
  useEffect(() => {
    const receive = (event: MessageEvent) => {
      if (
        event.source !== window.parent ||
        event.origin !== window.location.origin
      )
        return;
      if (
        event.data?.type === "netlab:theme" &&
        ["light", "dark"].includes(event.data.theme)
      )
        setColorScheme(event.data.theme);
    };
    window.addEventListener("message", receive);
    window.parent.postMessage({ type: "netlab:ready" }, window.location.origin);
    return () => window.removeEventListener("message", receive);
  }, [setColorScheme]);
  useEffect(() => {
    window.parent.postMessage(
      {
        type: "netlab:navigation",
        path: location.pathname + location.search + location.hash,
      },
      window.location.origin,
    );
  }, [location]);
  return null;
}
