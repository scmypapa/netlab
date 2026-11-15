import { ActionIcon, Popover, Tooltip } from "@mantine/core";
import { useQuery } from "@tanstack/react-query";
import { ChevronDown, HardDrive, RefreshCw } from "lucide-react";
import { useId, useState } from "react";
import { api, type Schema } from "../../api/client";
import { ErrorMessage, Loading } from "../../foundation/Feedback";
import { byteUnit, reading } from "../../foundation/format";
import styles from "./WorkbenchPage.module.css";

export function StorageUsage({
  id,
  revision,
}: {
  id: string;
  revision: number;
}) {
  const [opened, setOpened] = useState(false);
  const usage = useQuery({
    queryKey: ["environment-storage", id, revision],
    queryFn: ({ signal }) => api.environmentStorage(id, signal),
    staleTime: 60_000,
    refetchInterval: opened ? 60_000 : false,
    retry: false,
  });
  const bytes = usage.data?.bytes;
  const value =
    bytes === undefined
      ? usage.error
        ? "读取失败"
        : "统计中"
      : storageSize(bytes);
  return (
    <Popover
      position="bottom-start"
      width={460}
      opened={opened}
      onChange={setOpened}
      shadow="md"
      withArrow
      trapFocus
      returnFocus
    >
      <Popover.Target>
        <button
          className={styles.storageUsage}
          aria-label={`环境占用 ${value}，查看存储详情`}
          aria-expanded={opened}
          onClick={() => setOpened(!opened)}
        >
          <HardDrive size={15} />
          <span>环境占用</span>
          <strong>{value}</strong>
          <ChevronDown size={13} />
        </button>
      </Popover.Target>
      <Popover.Dropdown className={styles.storageDetails} bg="var(--surface)">
        <div className={styles.storageHeading}>
          <h3>环境存储</h3>
          <ActionIcon
            data-autofocus
            variant="subtle"
            aria-label="刷新存储占用"
            loading={usage.isFetching}
            onClick={() => void usage.refetch()}
          >
            <RefreshCw size={16} />
          </ActionIcon>
        </div>
        <ErrorMessage error={usage.error} />
        {usage.isPending && <Loading />}
        {usage.data && (
          <>
            <div className={styles.storageTotal}>
              <span>总占用</span>
              <strong>{value}</strong>
            </div>
            <dl className={styles.storageLegend}>
              <div>
                <dt>基础镜像</dt>
                <dd>{storageSize(usage.data.baseBytes)}</dd>
              </div>
              <div>
                <dt>增量数据</dt>
                <dd>{storageSize(usage.data.bytes - usage.data.baseBytes)}</dd>
              </div>
            </dl>
            {usage.data.locations.map((location) => (
              <section className={styles.storageLocation} key={location.id}>
                <div className={styles.storageLocationHeading}>
                  <h4>{location.name}</h4>
                </div>
                <StorageWave id={id} location={location} />
                <div className={styles.storageBalance}>
                  <span>
                    环境占用 <strong>{storageSize(location.bytes)}</strong>
                  </span>
                  <span>
                    空闲 <strong>{storageSize(location.availableBytes)}</strong>
                  </span>
                </div>
              </section>
            ))}
          </>
        )}
      </Popover.Dropdown>
    </Popover>
  );
}

function storageSize(bytes: number) {
  const { scale, unit } = byteUnit(bytes);
  return `${reading(bytes, scale)} ${unit}`;
}

function environmentColor(id: string) {
  let hash = 2166136261;
  for (const character of id)
    hash = Math.imul(hash ^ character.charCodeAt(0), 16777619);
  return `hsl(${(((hash >>> 0) % 300) + 240) % 360} 52% 53%)`;
}

function StorageWave({
  id,
  location,
}: {
  id: string;
  location: Schema<"StorageLocation">;
}) {
  const clip = useId();
  const others = location.environments.filter(
    (environment) => environment.id !== id,
  );
  const otherBytes = others.reduce(
    (sum, environment) => sum + environment.bytes,
    0,
  );
  const remaining = Math.max(
    0,
    location.capacityBytes -
      location.bytes -
      location.availableBytes -
      otherBytes,
  );
  const current = location.environments.find(
    (environment) => environment.id === id,
  );
  const segments = [
    {
      id,
      name: current?.name ?? "本环境",
      bytes: location.bytes,
      color: environmentColor(id),
    },
    {
      id: "free",
      name: "空闲",
      bytes: location.availableBytes,
      color: "#3b82f6",
    },
    { id: "system", name: "系统与预留", bytes: remaining, color: "#8d98a8" },
    ...others.map((environment) => ({
      ...environment,
      color:
        environment.id === "other"
          ? "#8d98a8"
          : environmentColor(environment.id),
    })),
  ];
  let offset = 0;
  return (
    <svg
      className={styles.storageWave}
      viewBox="0 0 600 44"
      preserveAspectRatio="none"
      role="img"
      aria-label={`基础镜像 ${storageSize(location.baseBytes)}，增量数据 ${storageSize(location.bytes - location.baseBytes)}，当前可用 ${storageSize(location.availableBytes)}，其他环境 ${storageSize(otherBytes)}`}
    >
      <defs>
        <clipPath id={clip}>
          <path
            className={styles.storageWaveMotion}
            d="M0 8 Q75 0 150 8 T300 8 T450 8 T600 8 T750 8 T900 8 T1050 8 T1200 8 V44 H0 Z"
          />
        </clipPath>
      </defs>
      <g clipPath={`url(#${clip})`}>
        {segments
          .filter((segment) => segment.bytes > 0)
          .map((segment) => {
            const width =
              location.capacityBytes > 0
                ? (segment.bytes / location.capacityBytes) * 600
                : 0;
            const x = offset;
            offset += width;
            const label = `${segment.name} · ${storageSize(segment.bytes)}`;
            return (
              <Tooltip
                key={segment.id}
                label={label}
                openDelay={150}
                events={{ hover: true, focus: true, touch: false }}
              >
                <rect
                  x={x}
                  width={width}
                  height="44"
                  fill={segment.color}
                  tabIndex={0}
                  aria-label={label}
                  data-storage-segment={segment.id}
                >
                  <title>{label}</title>
                </rect>
              </Tooltip>
            );
          })}
      </g>
    </svg>
  );
}
