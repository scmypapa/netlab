import { useComputedColorScheme } from "@mantine/core";
import { useEffect, useRef } from "react";
import * as echarts from "echarts/core";
import { GraphChart } from "echarts/charts";
import { TooltipComponent, AriaComponent } from "echarts/components";
import { CanvasRenderer } from "echarts/renderers";
import type { Environment } from "../../api/client";
import { byteUnit, reading } from "../../foundation/format";
import styles from "./TrafficWorkspace.module.css";

echarts.use([GraphChart, TooltipComponent, AriaComponent, CanvasRenderer]);

export type TrafficSelection =
  { asset: string } | { source: string; destination: string };
export type TrafficLink = {
  source: string;
  target: string;
  sourceName: string;
  targetName: string;
  value: number;
  label: string;
  severity?: number;
};
export const riskLevels = [
  { value: "high", label: "高风险", color: "#d95565" },
  { value: "medium", label: "中风险", color: "#d59639" },
  { value: "low", label: "低风险", color: "#557fd2" },
] as const;
export const riskLevel = (severity: number) =>
  riskLevels[severity === 1 ? 0 : severity === 2 ? 1 : 2];

export function TrafficGraph({
  environment,
  links,
  groups,
  colors,
  selection,
  onSelect,
}: {
  environment: Environment;
  links: TrafficLink[];
  groups: Map<string, string>;
  colors: Map<string, string>;
  selection?: TrafficSelection;
  onSelect: (selection: TrafficSelection) => void;
}) {
  const host = useRef<HTMLDivElement>(null);
  const chart = useRef<echarts.EChartsType>(null);
  const theme = useComputedColorScheme();
  useEffect(() => {
    const instance = echarts.init(host.current!);
    chart.current = instance;
    const observer = new ResizeObserver(() => instance.resize());
    observer.observe(host.current!);
    return () => {
      observer.disconnect();
      instance.dispose();
      chart.current = null;
    };
  }, []);
  useEffect(() => {
    const css = getComputedStyle(host.current!);
    const text = css.getPropertyValue("--text").trim();
    const muted = css.getPropertyValue("--muted").trim();
    const surface = css.getPropertyValue("--surface").trim();
    const nodes = new Map<string, { id: string; name: string; role: string }>();
    const spec = environment.appliedSpec ?? environment.spec;
    for (const asset of spec.assets)
      nodes.set(asset.id, {
        id: asset.id,
        name: asset.name,
        role: groups.get(asset.id)!,
      });
    const merged = new Map<string, TrafficLink>();
    for (const link of links) {
      for (const [id, name] of [
        [link.source, link.sourceName],
        [link.target, link.targetName],
      ])
        if (!nodes.has(id))
          nodes.set(id, { id, name, role: groups.get(id) ?? "外部" });
      const key = link.source + "\0" + link.target;
      const previous = merged.get(key);
      if (previous) {
        previous.value += link.value;
        const unit = byteUnit(previous.value);
        previous.label =
          link.severity !== undefined
            ? `${previous.value.toLocaleString()} 次命中`
            : `${reading(previous.value, unit.scale)} ${unit.unit}/s`;
        if (link.severity !== undefined)
          previous.severity = Math.min(previous.severity ?? 255, link.severity);
      } else merged.set(key, { ...link });
    }
    const edges = [...merged.values()]
      .sort((a, b) =>
        a.value === b.value
          ? a.source.localeCompare(b.source)
          : b.value - a.value,
      )
      .slice(0, 300);
    const visible = new Set(
      edges.flatMap((link) => [link.source, link.target]),
    );
    const roles = [...new Set([...nodes.values()].map((node) => node.role))];
    const rows = new Map<string, number>();
    const selected = (link: TrafficLink) =>
      !selection ||
      ("asset" in selection
        ? link.source === selection.asset || link.target === selection.asset
        : link.source === selection.source &&
          link.target === selection.destination);
    chart.current!.setOption(
      {
        animation: !window.matchMedia("(prefers-reduced-motion: reduce)")
          .matches,
        aria: {
          enabled: true,
          label: { description: "资产通信关系，可在右侧记录中选择相同连接" },
        },
        tooltip: {
          trigger: "item",
          renderMode: "richText",
          backgroundColor: surface,
          borderColor: css.getPropertyValue("--line").trim(),
          textStyle: { color: text },
          formatter: (item: {
            dataType: string;
            data: {
              name?: string;
              label?: string;
              sourceName?: string;
              targetName?: string;
            };
          }) =>
            item.dataType === "edge"
              ? `${item.data.sourceName} → ${item.data.targetName}\n${item.data.label}`
              : item.data.name,
        },
        series: [
          {
            type: "graph",
            layout: "none",
            roam: true,
            scaleLimit: { min: 0.25, max: 5 },
            data: [...nodes.values()]
              .filter((node) => visible.has(node.id) || groups.has(node.id))
              .map((node) => {
                const row = rows.get(node.role) ?? 0;
                rows.set(node.role, row + 1);
                const involved =
                  !selection ||
                  ("asset" in selection
                    ? selection.asset === node.id
                    : [selection.source, selection.destination].includes(
                        node.id,
                      ));
                return {
                  ...node,
                  x: roles.indexOf(node.role) * 230,
                  y: row * 95,
                  symbolSize: node.role === "外部" ? 23 : 37,
                  itemStyle: {
                    color: colors.get(node.role) ?? muted,
                    opacity: involved ? 1 : 0.45,
                    borderWidth: 5,
                    borderColor: surface,
                  },
                  label: {
                    show: true,
                    position: "bottom",
                    color: text,
                    fontSize: 12,
                    width: 160,
                    overflow: "truncate",
                    padding: [7, 0, 0, 0],
                  },
                };
              }),
            edges: edges.map((link) => ({
              ...link,
              lineStyle: {
                color: link.severity
                  ? riskLevel(link.severity).color
                  : (colors.get(groups.get(link.source) ?? "") ?? muted),
                width: 1.5 + Math.min(5, Math.log10(1 + link.value)),
                opacity: selected(link) ? 0.78 : 0.12,
                curveness: 0.12,
              },
            })),
            edgeSymbol: ["none", "arrow"],
            edgeSymbolSize: [0, 8],
            emphasis: { focus: "adjacency" },
          },
        ],
      },
      { replaceMerge: ["series"] },
    );
    chart.current!.off("click");
    chart.current!.on("click", (event: unknown) => {
      const item = event as {
        dataType: string;
        data: { id: string; source: string; target: string };
      };
      onSelect(
        item.dataType === "edge"
          ? { source: item.data.source, destination: item.data.target }
          : { asset: item.data.id },
      );
    });
  }, [environment, links, groups, colors, selection, onSelect, theme]);
  return (
    <div
      className={styles.graph}
      ref={host}
      role="img"
      aria-label="资产通信图"
    />
  );
}
