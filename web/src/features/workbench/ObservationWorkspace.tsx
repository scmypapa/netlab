import {
  ActionIcon,
  SegmentedControl,
  Switch,
  useComputedColorScheme,
} from "@mantine/core";
import { useQuery } from "@tanstack/react-query";
import { ArrowLeft, Activity, RefreshCw } from "lucide-react";
import { useEffect, useRef, useState } from "react";
import * as echarts from "echarts/core";
import { LineChart } from "echarts/charts";
import {
  GridComponent,
  LegendComponent,
  TooltipComponent,
  AriaComponent,
} from "echarts/components";
import { CanvasRenderer } from "echarts/renderers";
import { api, type Asset, type Network, type Schema } from "../../api/client";
import { Empty, ErrorMessage, Loading } from "../../foundation/Feedback";
import styles from "./ObservationWorkspace.module.css";

echarts.use([
  LineChart,
  GridComponent,
  LegendComponent,
  TooltipComponent,
  AriaComponent,
  CanvasRenderer,
]);
type History = Schema<"MetricHistory">;
type Metric = Schema<"MetricSeries">["metric"];
const titles: Partial<Record<Metric, string>> = {
  cpu: "CPU",
  memory: "内存占用",
  disk_read: "读取",
  disk_write: "写入",
  receive: "接收",
  transmit: "发送",
};
const ranges = [
  { label: "15 分钟", value: "900" },
  { label: "1 小时", value: "3600" },
  { label: "6 小时", value: "21600" },
  { label: "24 小时", value: "86400" },
];

function MetricChart({
  history,
  measures,
  unit,
  scale = 1,
  interfaces,
}: {
  history: History;
  measures: Metric[];
  unit: string;
  scale?: number;
  interfaces: Map<string, string>;
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
          item.interfaceId && interfaces.get(item.interfaceId),
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
  }, [history, measures, unit, scale, interfaces, theme]);
  return (
    <div
      ref={host}
      className={styles.chart}
      role="img"
      aria-label={measures.map((item) => titles[item]).join("、") + "历史曲线"}
    />
  );
}

const plots = [
  { title: "CPU", unit: "核", measures: ["cpu"], scale: 1 },
  { title: "内存", unit: "GiB", measures: ["memory"], scale: 2 ** 30 },
  {
    title: "网络",
    unit: "MiB/s",
    measures: ["receive", "transmit"],
    scale: 2 ** 20,
  },
  {
    title: "磁盘",
    unit: "MiB/s",
    measures: ["disk_read", "disk_write"],
    scale: 2 ** 20,
  },
] satisfies {
  title: string;
  unit: string;
  measures: Metric[];
  scale: number;
}[];

export default function ObservationWorkspace({
  environmentId,
  asset,
  networks,
  onClose,
}: {
  environmentId: string;
  asset: Asset;
  networks: Network[];
  onClose: () => void;
}) {
  const [range, setRange] = useState("900");
  const [live, setLive] = useState(true);
  const query = useQuery({
    queryKey: ["metrics", environmentId, asset.id, range],
    queryFn: ({ signal }) =>
      api.metrics(environmentId, asset.id, Number(range), signal),
    refetchInterval: live ? 10_000 : false,
    refetchOnWindowFocus: live,
  });
  const interfaces = new Map(
    asset.interfaces.map((iface, index) => [
      iface.id,
      (networks.find((network) => network.id === iface.networkId)?.name ??
        "网卡") +
        " · 网卡" +
        (index + 1),
    ]),
  );
  const history = query.data;
  return (
    <div className={styles.workspace} aria-label="资源观察">
      <header className={styles.toolbar}>
        <div className={styles.title}>
          <ActionIcon
            variant="subtle"
            color="gray"
            aria-label="返回拓扑"
            onClick={onClose}
          >
            <ArrowLeft size={18} />
          </ActionIcon>
          <strong>{asset.name}</strong>
          <span>资源</span>
        </div>
        <div className={styles.controls}>
          <SegmentedControl
            size="xs"
            value={range}
            onChange={setRange}
            data={ranges}
            aria-label="时间范围"
          />
          <Switch
            size="xs"
            label="实时"
            checked={live}
            onChange={(event) => setLive(event.currentTarget.checked)}
          />
          <ActionIcon
            variant="subtle"
            color="gray"
            aria-label="刷新资源数据"
            loading={query.isFetching}
            onClick={() => void query.refetch()}
          >
            <RefreshCw size={16} />
          </ActionIcon>
        </div>
      </header>
      <ErrorMessage error={query.error} />
      {query.isLoading ? (
        <Loading />
      ) : (
        !query.error &&
        history &&
        (history.series.some((item) => item.points.length) ? (
          <>
            <div className={styles.grid}>
              {plots.map((plot) => (
                <section
                  key={plot.title}
                  className={styles.plot}
                  aria-label={plot.title + "指标"}
                >
                  <h3>
                    {plot.title}
                    <span>{plot.unit}</span>
                  </h3>
                  <MetricChart
                    history={history}
                    measures={plot.measures}
                    unit={plot.unit}
                    scale={plot.scale}
                    interfaces={interfaces}
                  />
                </section>
              ))}
            </div>
            <div className={styles.interfaceStats}>
              <table>
                <caption>接口速率</caption>
                <thead>
                  <tr>
                    <th>网卡</th>
                    <th>接收包/s</th>
                    <th>发送包/s</th>
                    <th>接收丢包/s</th>
                    <th>发送丢包/s</th>
                  </tr>
                </thead>
                <tbody>
                  {asset.interfaces.map((iface) => {
                    const value = (metric: Metric) => {
                      const latest = history.series
                        .filter(
                          (item) =>
                            item.interfaceId === iface.id &&
                            item.metric === metric,
                        )
                        .flatMap((item) => item.points)
                        .sort(
                          (a, b) => Date.parse(b.time) - Date.parse(a.time),
                        )[0];
                      return latest &&
                        Date.parse(history.end) - Date.parse(latest.time) <
                          20_000
                        ? latest.value.toLocaleString(undefined, {
                            maximumFractionDigits: 2,
                          })
                        : "—";
                    };
                    return (
                      <tr key={iface.id}>
                        <td>{interfaces.get(iface.id)}</td>
                        <td>{value("receive_packets")}</td>
                        <td>{value("transmit_packets")}</td>
                        <td>{value("receive_drops")}</td>
                        <td>{value("transmit_drops")}</td>
                      </tr>
                    );
                  })}
                </tbody>
              </table>
            </div>
          </>
        ) : (
          <Empty icon={<Activity size={32} />} title="暂无资源记录" />
        ))
      )}
    </div>
  );
}
