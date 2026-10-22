import { useComputedColorScheme } from "@mantine/core";
import { useEffect, useRef } from "react";
import * as echarts from "echarts/core";
import { LineChart } from "echarts/charts";
import {
  GridComponent,
  LegendComponent,
  TooltipComponent,
  AriaComponent,
} from "echarts/components";
import { CanvasRenderer } from "echarts/renderers";
import type { Schema } from "../api/client";
import styles from "./MetricChart.module.css";

echarts.use([
  LineChart,
  GridComponent,
  LegendComponent,
  TooltipComponent,
  AriaComponent,
  CanvasRenderer,
]);
export type History = Schema<"MetricHistory">;
export type Metric = Schema<"MetricSeries">["metric"];
export const metricTitles: Partial<Record<Metric, string>> = {
  cpu: "CPU",
  memory: "内存",
  disk_read: "读取",
  disk_write: "写入",
  receive: "接收",
  transmit: "发送",
  cpu_capacity: "CPU 容量",
  memory_capacity: "内存容量",
  memory_available: "可用内存",
  disk_used: "磁盘占用",
  disk_capacity: "磁盘容量",
  receive_packets: "接收包",
  transmit_packets: "发送包",
  receive_drops: "接收丢包",
  transmit_drops: "发送丢包",
};
const titles = metricTitles;
const emptyNames = new Map<string, string>();

export function latestMetric(
  history: History | undefined,
  metric: Metric,
  nodeId?: string,
): number | undefined {
  const points = history?.series
    .filter(
      (series) =>
        series.metric === metric &&
        (nodeId === undefined || series.nodeId === nodeId),
    )
    .map((series) => series.points.at(-1))
    .filter(
      (point) =>
        point && Date.parse(history!.end) - Date.parse(point.time) <= 20_000,
    );
  if (!points?.length) return undefined;
  const time = Math.max(...points.map((point) => Date.parse(point!.time)));
  return points.reduce(
    (total, point) =>
      total + (Date.parse(point!.time) === time ? point!.value : 0),
    0,
  );
}
export function MetricChart({
  history,
  measures,
  unit,
  scale = 1,
  names = emptyNames,
  seriesBy = "interface",
}: {
  history: History;
  measures: Metric[];
  unit: string;
  scale?: number;
  names?: ReadonlyMap<string, string>;
  seriesBy?: "metric" | "asset" | "node" | "interface";
}) {
  const host = useRef<HTMLDivElement>(null);
  const chart = useRef<echarts.EChartsType>(null);
  const theme = useComputedColorScheme();
  useEffect(() => {
    const current = echarts.init(host.current!);
    chart.current = current;
    const observer = new ResizeObserver(() => current.resize());
    observer.observe(host.current!);
    return () => {
      observer.disconnect();
      current.dispose();
      chart.current = null;
    };
  }, [theme]);
  useEffect(() => {
    const css = getComputedStyle(host.current!);
    const muted = css.getPropertyValue("--muted").trim();
    const line = css.getPropertyValue("--line").trim();
    const readings: string[] = [];
    const series = history.series
      .filter((item) => measures.includes(item.metric))
      .map((item) => {
        const points: [number, number | null][] = [];
        const step = history.stepSeconds * 1000;
        let previous = Date.parse(history.start);
        for (const point of item.points) {
          const time = Date.parse(point.time);
          if (time - previous > step * 1.5)
            points.push([previous + step, null]);
          points.push([time, point.value / scale]);
          previous = time;
        }
        if (Date.parse(history.end) - previous > step * 1.5)
          points.push([previous + step, null]);
        const name = [
          seriesBy !== "metric" &&
            names.get(
              seriesBy === "node"
                ? item.nodeId
                : seriesBy === "asset"
                  ? item.assetId
                  : (item.interfaceId ?? ""),
            ),
          titles[item.metric],
        ]
          .filter(Boolean)
          .join(" · ");
        const latest = item.points.at(-1);
        if (latest)
          readings.push(
            name +
              " " +
              (latest.value / scale).toLocaleString(undefined, {
                maximumFractionDigits: 3,
              }) +
              " " +
              unit +
              "，" +
              new Date(latest.time).toLocaleTimeString(),
          );
        return {
          name,
          type: "line" as const,
          data: points,
          showSymbol: false,
          sampling: "lttb" as const,
          connectNulls: false,
          lineStyle: { width: 1.8 },
          emphasis: { focus: "series" as const },
        };
      });
    chart.current!.setOption(
      {
        animation: false,
        aria: {
          enabled: true,
          label: {
            description:
              measures.map((item) => titles[item]).join("、") +
              "历史曲线。" +
              (readings.length
                ? "最近记录：" + readings.join("；")
                : "暂无记录"),
          },
        },
        color: [
          "#3975d8",
          "#1ba790",
          "#dc8b39",
          "#9861c7",
          "#d25e77",
          "#5b95a7",
        ],
        grid: { left: 46, right: 16, top: 12, bottom: 42 },
        tooltip: {
          trigger: "axis",
          confine: true,
          valueFormatter: (value: number) =>
            Number(value).toLocaleString(undefined, {
              maximumFractionDigits: 3,
            }) +
            " " +
            unit,
        },
        xAxis: {
          type: "time",
          min: Date.parse(history.start),
          max: Date.parse(history.end),
          axisLine: { lineStyle: { color: line } },
          axisLabel: { color: muted, hideOverlap: true },
          axisTick: { show: false },
          splitLine: { show: false },
        },
        yAxis: {
          type: "value",
          min: 0,
          splitNumber: 3,
          axisLabel: { color: muted },
          splitLine: { lineStyle: { color: line, type: "dashed" } },
        },
        legend: {
          show: measures.length > 1 || series.length > 1,
          bottom: 0,
          type: "scroll",
          textStyle: { color: muted },
          icon: "roundRect",
          itemWidth: 12,
          itemHeight: 3,
        },
        series,
      },
      { replaceMerge: ["series"] },
    );
  }, [history, measures, unit, scale, names, seriesBy, theme]);
  return (
    <div
      ref={host}
      className={styles.chart}
      role="img"
      aria-label={measures.map((item) => titles[item]).join("、") + "历史曲线"}
    />
  );
}
