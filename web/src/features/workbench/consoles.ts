import type { Asset, Schema } from "../../api/client";

export type ConsoleTab = Pick<Asset, "id" | "name"> & {
  kind: Schema<"ConsoleKind">;
};

export function consoleKey(tab: ConsoleTab) {
  return `${tab.id}:${tab.kind}`;
}
