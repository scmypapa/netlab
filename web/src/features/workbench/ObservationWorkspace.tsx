import { MetricChart } from "../../foundation/MetricChart";
import { ActionIcon, SegmentedControl, Switch } from "@mantine/core";
import { useQuery } from "@tanstack/react-query";
import { ArrowLeft, Activity, RefreshCw } from "lucide-react";
import { useState } from "react";
import { api, type Asset, type Network, type Schema } from "../../api/client";
import { Empty, ErrorMessage, Loading } from "../../foundation/Feedback";
import styles from "./ObservationWorkspace.module.css";

type Metric = Schema<"MetricSeries">["metric"];
const ranges = [
  { label: "15 分钟", value: "900" },
  { label: "1 小时", value: "3600" },
  { label: "6 小时", value: "21600" },
  { label: "24 小时", value: "86400" },
];

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
                    names={interfaces}
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
