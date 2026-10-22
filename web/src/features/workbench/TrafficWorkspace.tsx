import {
  ActionIcon,
  Badge,
  Button,
  Menu,
  Modal,
  MultiSelect,
  NumberInput,
  Popover,
  SegmentedControl,
  Select,
  TextInput,
  Tooltip,
} from "@mantine/core";
import { useDebouncedValue } from "@mantine/hooks";
import {
  ArrowDownLeft,
  ArrowLeft,
  ArrowRight,
  ArrowUpRight,
  ChevronDown,
  Download,
  Network,
  Pause,
  Radio,
  Search,
  Shield,
  ShieldAlert,
  Trash2,
  X,
} from "lucide-react";
import { useMemo, useState, type CSSProperties } from "react";
import { api, type Environment, type Schema } from "../../api/client";
import { MetricChart } from "../../foundation/MetricChart";
import { latestMetric } from "../../foundation/metrics";
import { byteUnit, dateTime, reading } from "../../foundation/format";
import {
  TrafficGraph,
  riskLevel,
  riskLevels,
  type TrafficSelection,
} from "./TrafficGraph";
import {
  useTrafficObservation,
  endpoint,
  activeCapture,
  type Alert,
} from "./useTrafficObservation";
import styles from "./TrafficWorkspace.module.css";

const ranges = [
  { label: "15 分钟", value: "900" },
  { label: "1 小时", value: "3600" },
  { label: "24 小时", value: "86400" },
  { label: "7 天", value: "604800" },
];
const palette = [
  "#35a69b",
  "#6995d6",
  "#a78bcb",
  "#ce9b58",
  "#cb7f98",
  "#648f99",
];
const measures: Schema<"MetricSeries">["metric"][] = ["receive", "transmit"];
const volume = (value: number) => {
  const unit = byteUnit(value);
  return `${reading(value, unit.scale)} ${unit.unit}`;
};
const sensorTitles = {
  idle: "无运行资产",
  loading: "加载中",
  ready: "检测中",
  unavailable: "不可用",
  failed: "检测失败",
};
const segmentTitles = {
  starting: "启动中",
  running: "抓包中",
  stopped: "已结束",
  failed: "失败",
};

function AlertDetail({
  alert,
  onSelect,
}: {
  alert: Alert;
  onSelect: (selection: TrafficSelection) => void;
}) {
  const risk = riskLevel(alert.severity);
  return (
    <div className={styles.alertDetail}>
      <Badge color={risk.color} variant="light">
        {risk.label}
      </Badge>
      <h3>{alert.signature}</h3>
      <p>{alert.category}</p>
      <div className={styles.alertRoute}>
        {(["source", "destination"] as const).map((side, index) => (
          <div key={side}>
            {index === 1 && <ArrowDownLeft size={17} />}
            <span>{side === "source" ? "来源" : "目标"}</span>
            <button onClick={() => onSelect({ asset: endpoint(alert, side) })}>
              {alert[`${side}AssetName`] || alert[side]}
            </button>
            <code>
              {alert[side]}
              {alert[`${side}Port`] ? `:${alert[`${side}Port`]}` : ""}
            </code>
          </div>
        ))}
      </div>
      <dl className={styles.detailList}>
        <div>
          <dt>协议</dt>
          <dd>
            {[alert.protocol, alert.appProtocol].filter(Boolean).join(" · ")}
          </dd>
        </div>
        <div>
          <dt>命中次数</dt>
          <dd>{alert.count.toLocaleString()}</dd>
        </div>
        <div>
          <dt>首次出现</dt>
          <dd>{dateTime(alert.firstSeen)}</dd>
        </div>
        <div>
          <dt>最近出现</dt>
          <dd>{dateTime(alert.lastSeen)}</dd>
        </div>
      </dl>
      <details>
        <summary>规则信息</summary>
        <code>
          {alert.generatorId}:{alert.signatureId}:{alert.revision}
        </code>
      </details>
    </div>
  );
}

export default function TrafficWorkspace({
  environment,
  onClose,
  onView,
}: {
  environment: Environment;
  onClose: () => void;
  onView?: (view: Schema<"CanvasView">) => Promise<unknown>;
}) {
  const [captureId, setCaptureId] = useState("live");
  const [mode, setMode] = useState("traffic");
  const [range, setRange] = useState("3600");
  const [level, setLevel] = useState<string | null>(null);
  const [selection, setSelection] = useState<TrafficSelection>();
  const [selectedAlert, setSelectedAlert] = useState<Alert>();
  const [protocol, setProtocol] = useState<string | null>(null);
  const [role, setRole] = useState<string | null>(null);
  const [searchInput, setSearchInput] = useState("");
  const [search] = useDebouncedValue(searchInput, 250);
  const [creating, setCreating] = useState(false);
  const [roleError, setRoleError] = useState<string>();
  const spec = environment.appliedSpec ?? environment.spec;
  const [assetIds, setAssetIds] = useState(
    spec.assets.map((asset) => asset.id),
  );
  const [duration, setDuration] = useState(300);
  const [fileSize, setFileSize] = useState(256);
  const [captureFilter, setCaptureFilter] = useState("");
  const observedAsset =
    selection &&
    "asset" in selection &&
    spec.assets.some((asset) => asset.id === selection.asset)
      ? selection.asset
      : undefined;
  const data = useTrafficObservation(environment, captureId, setCaptureId, {
    range: Number(range),
    assetId: observedAsset,
    protocol: protocol ?? undefined,
    level: level ?? undefined,
    search: mode === "risk" ? search : undefined,
  });
  const groups = useMemo(
    () =>
      new Map(
        spec.assets.map((asset) => [
          asset.id,
          environment.view.roles?.[asset.id] ??
            spec.networks.find(
              (network) => network.id === asset.interfaces[0]?.networkId,
            )?.name ??
            "未分组",
        ]),
      ),
    [spec, environment.view.roles],
  );
  const colors = useMemo(
    () =>
      new Map(
        [...new Set(groups.values())]
          .sort()
          .map((name, index) => [name, palette[index % palette.length]]),
      ),
    [groups],
  );
  const name = (id: string) =>
    spec.assets.find((asset) => asset.id === id)?.name ??
    [...data.flows, ...data.alerts]
      .flatMap((item) => [
        [item.sourceAssetId, item.sourceAssetName],
        [item.destinationAssetId, item.destinationAssetName],
      ])
      .find(([key]) => key === id)?.[1] ??
    id;
  const matches = (item: {
    source: string;
    destination: string;
    sourceAssetId?: string;
    destinationAssetId?: string;
    protocol: string;
  }) =>
    (!protocol || item.protocol === protocol) &&
    (!role ||
      [
        groups.get(item.sourceAssetId ?? ""),
        groups.get(item.destinationAssetId ?? ""),
      ].includes(role));
  const flows = data.flows.filter(
    (flow) =>
      matches(flow) &&
      (!search ||
        [
          flow.source,
          flow.destination,
          flow.sourceAssetName,
          flow.destinationAssetName,
          flow.protocol,
        ]
          .join(" ")
          .toLowerCase()
          .includes(search.toLowerCase())),
  );
  const alerts = data.alerts.filter(matches);
  const selected = (item: (typeof flows)[number] | Alert) =>
    !selection ||
    ("asset" in selection
      ? [endpoint(item, "source"), endpoint(item, "destination")].includes(
          selection.asset,
        )
      : endpoint(item, "source") === selection.source &&
        endpoint(item, "destination") === selection.destination);
  const visibleFlows = flows.filter(selected),
    visibleAlerts = alerts.filter(selected);
  const links =
    mode === "risk"
      ? alerts.map((alert) => ({
          source: endpoint(alert, "source"),
          target: endpoint(alert, "destination"),
          sourceName: alert.sourceAssetName || alert.source,
          targetName: alert.destinationAssetName || alert.destination,
          value: alert.count,
          label: `${alert.count.toLocaleString()} 次命中`,
          severity: alert.severity,
        }))
      : flows.map((flow) => ({
          source: endpoint(flow, "source"),
          target: endpoint(flow, "destination"),
          sourceName: flow.sourceAssetName || flow.source,
          targetName: flow.destinationAssetName || flow.destination,
          value: flow.bytesPerSecond,
          label: `${volume(flow.bytes)} · ${volume(flow.bytesPerSecond)}/s`,
        }));
  const peak =
    data.history.data?.series.reduce(
      (max, series) =>
        measures.includes(series.metric)
          ? series.points.reduce(
              (value, point) => Math.max(value, point.value),
              max,
            )
          : max,
      0,
    ) ?? 0;
  const unit = byteUnit(peak);
  const rate = (metric: "receive" | "transmit") => {
    const value = latestMetric(
      data.current.isError ? undefined : data.current.data,
      metric,
    );
    return value === undefined ? "—" : volume(value) + "/s";
  };
  const sensors = data.summary?.sensors ?? [],
    ready = sensors.filter((sensor) => sensor.status === "ready").length;
  const counts = data.summary?.counts,
    totalAlerts = counts ? counts.high + counts.medium + counts.low : 0;
  const changeSource = (id: string) => {
    setCaptureId(id);
    setSelection(undefined);
    setSelectedAlert(undefined);
    if (id !== "live") setMode("traffic");
  };
  const select = (value: TrafficSelection) => {
    setSelection(value);
    setSelectedAlert(undefined);
  };
  const allSegments = data.captures.data?.segments ?? [];
  const sessions = [
    { value: "live", label: "实时流量" },
    ...new Map(
      allSegments.map((segment) => [
        segment.id,
        { value: segment.id, label: dateTime(segment.startedAt) },
      ]),
    ).values(),
  ];
  const running = allSegments.filter(activeCapture);
  const errors = [
    ...data.errors,
    ...(roleError ? [roleError] : []),
    ...Object.values(
      data.live
        ? (data.traffic.data?.errors ?? {})
        : (data.captures.data?.errors ?? {}),
    ),
    ...Object.values(data.start.data?.errors ?? {}),
  ];
  return (
    <section className={styles.workspace} aria-label="流量观测">
      <header className={styles.toolbar}>
        <div className={styles.title}>
          <ActionIcon variant="subtle" aria-label="返回画布" onClick={onClose}>
            <ArrowLeft size={18} />
          </ActionIcon>
          <div>
            <strong>流量观测</strong>
            <span>{environment.name}</span>
          </div>
        </div>
        <div className={styles.controls}>
          {data.live && (
            <SegmentedControl
              aria-label="流量时间范围"
              size="xs"
              data={ranges}
              value={range}
              onChange={setRange}
            />
          )}
          <Select
            aria-label="流量来源"
            data={sessions}
            value={captureId}
            onChange={(id) => changeSource(id ?? "live")}
            size="xs"
            w={144}
          />
          <Menu position="bottom-end">
            <Menu.Target>
              <Button
                variant="default"
                size="xs"
                leftSection={<Radio size={14} />}
                rightSection={<ChevronDown size={12} />}
              >
                抓包
              </Button>
            </Menu.Target>
            <Menu.Dropdown>
              <Menu.Item
                leftSection={<Radio size={14} />}
                onClick={() => {
                  data.start.reset();
                  setCreating(true);
                }}
              >
                开始抓包
              </Menu.Item>
              {running.length > 0 && (
                <Menu.Item
                  leftSection={<Pause size={14} />}
                  disabled={data.action.isPending}
                  onClick={() =>
                    data.action.mutate({ segments: running, remove: false })
                  }
                >
                  停止抓包
                </Menu.Item>
              )}
            </Menu.Dropdown>
          </Menu>
        </div>
      </header>
      {errors.length > 0 && (
        <div className={styles.errors} role="alert">
          {[...new Set(errors)].map((error) => (
            <div key={error}>{error}</div>
          ))}
        </div>
      )}
      {data.live && (
        <div className={styles.overview}>
          <section className={styles.bandwidth} aria-label="接口流量历史">
            <header>
              <span className={styles.eyebrow}>
                {observedAsset ? name(observedAsset) : "环境带宽"}
              </span>
              <div className={styles.rates}>
                <span>
                  <ArrowDownLeft size={14} />
                  <b>{rate("receive")}</b>
                </span>
                <span>
                  <ArrowUpRight size={14} />
                  <b>{rate("transmit")}</b>
                </span>
              </div>
            </header>
            {data.history.data && !data.history.isError ? (
              <MetricChart
                history={data.history.data}
                measures={measures}
                unit={unit.unit + "/s"}
                scale={unit.scale}
                seriesBy="metric"
                colors={["#5b86d1", "#35a69b"]}
              />
            ) : (
              <div className={styles.chartEmpty}>
                {data.history.isLoading ? "读取流量…" : "暂无接口记录"}
              </div>
            )}
          </section>
          <section className={styles.riskSummary} aria-label="风险概览">
            <header>
              <span className={styles.eyebrow}>风险命中</span>
              <Popover position="bottom-end" width={340} shadow="md">
                <Popover.Target>
                  <button
                    className={styles.sensorButton}
                    data-ready={sensors.length > 0 && ready === sensors.length}
                  >
                    <Shield size={14} />
                    Suricata{" "}
                    <span>
                      {ready}/{sensors.length}
                    </span>
                    <ChevronDown size={12} />
                  </button>
                </Popover.Target>
                <Popover.Dropdown>
                  <div className={styles.sensors}>
                    {sensors.length === 0 && <span>暂无检测节点</span>}
                    {sensors.map((sensor) => (
                      <section key={sensor.nodeId}>
                        <header>
                          <strong>{sensor.nodeName ?? "运行节点"}</strong>
                          <Badge
                            size="xs"
                            color={sensor.status === "ready" ? "teal" : "gray"}
                          >
                            {sensorTitles[sensor.status]}
                          </Badge>
                        </header>
                        <span>
                          {sensor.interfaces} 个接口 ·{" "}
                          {sensor.droppedPackets.toLocaleString()} 丢包
                        </span>
                        {sensor.rulesUpdatedAt && (
                          <span>
                            规则更新 {dateTime(sensor.rulesUpdatedAt)}
                          </span>
                        )}
                        {sensor.error && <p role="alert">{sensor.error}</p>}
                      </section>
                    ))}
                  </div>
                </Popover.Dropdown>
              </Popover>
            </header>
            <div className={styles.riskNumbers}>
              {riskLevels.map((risk) => (
                <button
                  key={risk.value}
                  style={{ "--risk": risk.color } as CSSProperties}
                  data-selected={mode === "risk" && level === risk.value}
                  onClick={() => {
                    setMode("risk");
                    setLevel(level === risk.value ? null : risk.value);
                    setSelectedAlert(undefined);
                  }}
                  aria-pressed={mode === "risk" && level === risk.value}
                >
                  <span>{risk.label}</span>
                  <strong>
                    {counts && !data.security.isError
                      ? counts[risk.value].toLocaleString()
                      : "—"}
                  </strong>
                </button>
              ))}
            </div>
            <div className={styles.riskTrack} aria-hidden="true">
              {riskLevels.map((risk) => (
                <span
                  key={risk.value}
                  style={{
                    background: risk.color,
                    flex: counts && totalAlerts ? counts[risk.value] : 0,
                  }}
                />
              ))}
            </div>
          </section>
        </div>
      )}
      <div className={styles.body}>
        <section className={styles.visual}>
          <div className={styles.graphToolbar}>
            <SegmentedControl
              aria-label="流量视图"
              value={mode}
              onChange={(value) => {
                setMode(value);
                setSelectedAlert(undefined);
              }}
              size="xs"
              data={[
                {
                  label: (
                    <span>
                      <Network size={14} />
                      通信
                    </span>
                  ),
                  value: "traffic",
                },
                {
                  label: (
                    <span>
                      <ShieldAlert size={14} />
                      风险
                    </span>
                  ),
                  value: "risk",
                  disabled: !data.live,
                },
              ]}
            />
            <div className={styles.filters}>
              <Select
                aria-label="流量协议"
                placeholder="全部协议"
                data={[
                  ...new Set(
                    (mode === "risk" ? data.alerts : data.flows).map(
                      (item) => item.protocol,
                    ),
                  ),
                ].sort()}
                value={protocol}
                onChange={setProtocol}
                clearable
                size="xs"
                w={112}
              />
              <Select
                aria-label="资产角色"
                placeholder="全部角色"
                data={[...colors.keys()]}
                value={role}
                onChange={setRole}
                clearable
                size="xs"
                w={125}
              />
              {mode === "risk" && level && (
                <Badge
                  color={riskLevels.find((risk) => risk.value === level)!.color}
                  variant="light"
                  rightSection={
                    <ActionIcon
                      size={14}
                      variant="transparent"
                      aria-label="清除风险筛选"
                      onClick={() => setLevel(null)}
                    >
                      <X size={11} />
                    </ActionIcon>
                  }
                >
                  {riskLevels.find((risk) => risk.value === level)!.label}
                </Badge>
              )}
            </div>
          </div>
          <div className={styles.graphArea}>
            <TrafficGraph
              environment={environment}
              links={links}
              groups={groups}
              colors={colors}
              selection={selection}
              onSelect={select}
            />
            <div className={styles.graphCaption}>
              <span>
                {mode === "risk"
                  ? `${alerts.length} 组告警`
                  : `${flows.length} 条连接`}
              </span>
              {data.live && mode === "traffic" && (
                <Tooltip
                  label={`近 ${data.traffic.data?.windowSeconds ?? 60} 秒 · 1/${data.traffic.data?.samplingRate ?? 512} 采样${data.traffic.data?.omittedSamples ? ` · 裁剪 ${data.traffic.data.omittedSamples} 条记录` : ""}`}
                >
                  <span>采样通信</span>
                </Tooltip>
              )}
            </div>
          </div>
          <footer className={styles.legend}>
            {(mode === "risk"
              ? riskLevels.map((risk) => [risk.label, risk.color] as const)
              : [...colors]
            ).map(([label, color]) => (
              <span key={label}>
                <i style={{ background: color }} />
                {label}
              </span>
            ))}
            <span className={styles.legendHint}>拖动画布 · 滚轮缩放</span>
          </footer>
        </section>
        <aside className={styles.inspector}>
          <header>
            <div>
              <strong>
                {selectedAlert
                  ? "告警详情"
                  : selection
                    ? "asset" in selection
                      ? name(selection.asset)
                      : "选中连接"
                    : mode === "risk"
                      ? "风险记录"
                      : "通信记录"}
              </strong>
              {selection && !selectedAlert && (
                <span>
                  {"source" in selection
                    ? `${name(selection.source)} → ${name(selection.destination)}`
                    : groups.get(selection.asset)}
                </span>
              )}
              {selection && "source" in selection && !selectedAlert && (
                <div className={styles.assetLinks}>
                  <button onClick={() => select({ asset: selection.source })}>
                    查看 {name(selection.source)}
                  </button>
                  <button
                    onClick={() => select({ asset: selection.destination })}
                  >
                    查看 {name(selection.destination)}
                  </button>
                </div>
              )}
            </div>
            {(selection || selectedAlert) && (
              <ActionIcon
                variant="subtle"
                aria-label={selectedAlert ? "返回风险记录" : "清除选择"}
                onClick={() =>
                  selectedAlert
                    ? setSelectedAlert(undefined)
                    : setSelection(undefined)
                }
              >
                {selectedAlert ? <ArrowLeft size={16} /> : <X size={16} />}
              </ActionIcon>
            )}
          </header>
          {selectedAlert ? (
            <AlertDetail alert={selectedAlert} onSelect={select} />
          ) : (
            <>
              <TextInput
                aria-label="筛选流量记录"
                placeholder={
                  mode === "risk"
                    ? "搜索规则、分类、地址"
                    : "搜索资产、协议、地址"
                }
                leftSection={<Search size={14} />}
                value={searchInput}
                onChange={(event) => setSearchInput(event.currentTarget.value)}
                size="xs"
              />
              {observedAsset && onView && (
                <TextInput
                  key={
                    observedAsset +
                    (environment.view.roles?.[observedAsset] ?? "")
                  }
                  aria-label="角色"
                  placeholder="角色名称"
                  defaultValue={environment.view.roles?.[observedAsset] ?? ""}
                  size="xs"
                  onBlur={async (event) => {
                    const value = event.currentTarget.value.trim();
                    if (
                      value === (environment.view.roles?.[observedAsset] ?? "")
                    )
                      return;
                    const roles = { ...environment.view.roles };
                    if (value) roles[observedAsset] = value;
                    else delete roles[observedAsset];
                    try {
                      await onView({ ...environment.view, roles });
                      setRoleError(undefined);
                    } catch (error) {
                      setRoleError(String(error));
                    }
                  }}
                />
              )}
              <div
                className={styles.records}
                aria-label={
                  mode === "risk" ? "可选择的风险记录" : "可选择的通信连接"
                }
              >
                {mode === "risk"
                  ? visibleAlerts.map((alert) => (
                      <button
                        key={alert.id}
                        className={styles.alertRow}
                        onClick={() => {
                          setSelectedAlert(alert);
                          setSelection({
                            source: endpoint(alert, "source"),
                            destination: endpoint(alert, "destination"),
                          });
                        }}
                      >
                        <div>
                          <i
                            style={{
                              background: riskLevel(alert.severity).color,
                            }}
                          />
                          <strong>{alert.signature}</strong>
                          <span>×{alert.count.toLocaleString()}</span>
                        </div>
                        <p>
                          {alert.sourceAssetName || alert.source}
                          <ArrowRight size={12} />
                          {alert.destinationAssetName || alert.destination}
                        </p>
                        <footer>
                          <span>
                            {alert.appProtocol || alert.protocol} ·{" "}
                            {alert.category}
                          </span>
                          <time>{dateTime(alert.lastSeen)}</time>
                        </footer>
                      </button>
                    ))
                  : visibleFlows.slice(0, 300).map((flow, index) => (
                      <button
                        key={[
                          flow.source,
                          flow.destination,
                          flow.sourcePort,
                          flow.destinationPort,
                          flow.protocol,
                          index,
                        ].join("/")}
                        className={styles.flowRow}
                        onClick={() =>
                          select({
                            source: endpoint(flow, "source"),
                            destination: endpoint(flow, "destination"),
                          })
                        }
                      >
                        <div>
                          <strong>{flow.sourceAssetName || flow.source}</strong>
                          <ArrowRight size={13} />
                          <strong>
                            {flow.destinationAssetName || flow.destination}
                          </strong>
                        </div>
                        <footer>
                          <span>
                            {flow.protocol} · {flow.sourcePort || "—"} →{" "}
                            {flow.destinationPort || "—"}
                          </span>
                          <b>{volume(flow.bytes)}</b>
                        </footer>
                        {selection && (
                          <p>
                            {flow.source} → {flow.destination}
                          </p>
                        )}
                      </button>
                    ))}
                {(mode === "risk"
                  ? visibleAlerts.length === 0
                  : visibleFlows.length === 0) && (
                  <div className={styles.empty}>
                    {mode === "risk" ? (
                      <Shield size={30} />
                    ) : (
                      <Network size={30} />
                    )}
                    <strong>
                      {mode === "risk"
                        ? sensors.length > 0 && ready === sensors.length
                          ? "暂无匹配告警"
                          : "检测尚未就绪"
                        : "暂无匹配通信"}
                    </strong>
                  </div>
                )}
                {mode === "risk" && data.security.hasNextPage && (
                  <Button
                    variant="subtle"
                    size="xs"
                    loading={data.security.isFetchingNextPage}
                    onClick={() => void data.security.fetchNextPage()}
                  >
                    加载更多
                  </Button>
                )}
              </div>
            </>
          )}
          {!data.live && (
            <section className={styles.segments}>
              <h3>抓包分段</h3>
              {data.segments.map((segment) => (
                <div key={segment.nodeId}>
                  <header>
                    <strong>{segment.nodeName ?? "运行节点"}</strong>
                    <span>{segmentTitles[segment.status]}</span>
                  </header>
                  <footer>
                    <span>
                      {volume(segment.bytes)} ·{" "}
                      {segment.packets.toLocaleString()} 包
                    </span>
                    {activeCapture(segment) ? (
                      <ActionIcon
                        variant="subtle"
                        aria-label="停止此分段"
                        loading={data.action.isPending}
                        onClick={() =>
                          data.action.mutate({
                            segments: [segment],
                            remove: false,
                          })
                        }
                      >
                        <Pause size={14} />
                      </ActionIcon>
                    ) : (
                      <ActionIcon
                        component="a"
                        href={api.captureDownload(
                          environment.id,
                          segment.nodeId,
                          segment.id,
                        )}
                        variant="subtle"
                        aria-label="下载抓包文件"
                        disabled={!segment.packets}
                      >
                        <Download size={14} />
                      </ActionIcon>
                    )}
                    <ActionIcon
                      variant="subtle"
                      color="gray"
                      aria-label="删除此分段"
                      disabled={activeCapture(segment) || data.action.isPending}
                      onClick={() =>
                        data.action.mutate({
                          segments: [segment],
                          remove: true,
                        })
                      }
                    >
                      <Trash2 size={14} />
                    </ActionIcon>
                  </footer>
                  {segment.error && <p role="alert">{segment.error}</p>}
                  {!!segment.kernelDroppedPackets && (
                    <p>
                      内核丢包 {segment.kernelDroppedPackets.toLocaleString()}
                    </p>
                  )}
                  {segment.omittedFlows > 0 && (
                    <p>
                      在线汇总裁剪 {segment.omittedFlows.toLocaleString()} 条
                    </p>
                  )}
                </div>
              ))}
            </section>
          )}
        </aside>
      </div>
      <Modal
        opened={creating}
        onClose={() => setCreating(false)}
        title="开始抓包"
        centered
      >
        <form
          className={styles.form}
          onSubmit={(event) => {
            event.preventDefault();
            data.start.mutate(
              {
                assetIds,
                durationSeconds: duration,
                fileSizeMiB: fileSize,
                filter: captureFilter,
              },
              {
                onSuccess: (result) => {
                  if (result.segments.length) {
                    setCreating(false);
                    setMode("traffic");
                    setSelection(undefined);
                  }
                },
              },
            );
          }}
        >
          <MultiSelect
            label="资产"
            data={spec.assets.map((asset) => ({
              value: asset.id,
              label: asset.name,
            }))}
            value={assetIds}
            onChange={setAssetIds}
            searchable
          />
          <div className={styles.limits}>
            <NumberInput
              label="时长（秒）"
              min={10}
              max={1800}
              value={duration}
              onChange={(value) => setDuration(Number(value))}
            />
            <NumberInput
              label="每节点额度（MiB）"
              min={1}
              max={1024}
              value={fileSize}
              onChange={(value) => setFileSize(Number(value))}
            />
          </div>
          <TextInput
            label="过滤条件"
            placeholder="tcp port 443"
            value={captureFilter}
            onChange={(event) => setCaptureFilter(event.currentTarget.value)}
          />
          {data.start.error && (
            <p className={styles.error} role="alert">
              {data.start.error.message}
            </p>
          )}
          <Button
            type="submit"
            loading={data.start.isPending}
            disabled={!assetIds.length}
          >
            开始
          </Button>
        </form>
      </Modal>
    </section>
  );
}
