import type { Asset, Schema } from "../../api/client";

export type ConsoleSession = Pick<Asset, "id" | "name"> & {
  kind: Schema<"ConsoleKind">;
  revision?: number;
};

export function isConsoleKind(
  kind: string | undefined,
): kind is Schema<"ConsoleKind"> {
  return (
    kind !== undefined &&
    ["terminal", "serial", "vnc", "ssh", "rdp"].includes(kind)
  );
}

export function consoleHref(
  environmentId: string,
  assetId: string,
  kind: Schema<"ConsoleKind">,
) {
  const base = `/environments/${encodeURIComponent(environmentId)}`;
  return `${base}/console/${encodeURIComponent(assetId)}/${kind}`;
}
