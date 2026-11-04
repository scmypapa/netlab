import { ActionIcon, Popover } from "@mantine/core";
import { useQuery } from "@tanstack/react-query";
import { ChevronDown, HardDrive, RefreshCw } from "lucide-react";
import { api } from "../../api/client";
import { ErrorMessage, Loading } from "../../foundation/Feedback";
import { byteUnit, reading } from "../../foundation/format";
import { StorageLocations } from "../../foundation/StorageLocations";
import styles from "./WorkbenchPage.module.css";

export function StorageUsage({
  id,
  revision,
}: {
  id: string;
  revision: number;
}) {
  const usage = useQuery({
    queryKey: ["environment-storage", id, revision],
    queryFn: ({ signal }) => api.environmentStorage(id, signal),
    staleTime: 60_000,
    retry: false,
  });
  const bytes = usage.data?.bytes;
  const { scale, unit } = byteUnit(bytes ?? 0);
  const value =
    bytes === undefined
      ? usage.error
        ? "读取失败"
        : "统计中"
      : `${reading(bytes, scale)} ${unit}`;
  return (
    <Popover
      position="bottom-start"
      width={340}
      shadow="md"
      withArrow
      trapFocus
      returnFocus
    >
      <Popover.Target>
        <button
          className={styles.storageUsage}
          aria-label={`环境数据 ${value}，查看存放位置`}
        >
          <HardDrive size={15} />
          <span>环境数据</span>
          <strong>{value}</strong>
          <ChevronDown size={13} />
        </button>
      </Popover.Target>
      <Popover.Dropdown className={styles.storageDetails}>
        <div className={styles.storageHeading}>
          <h3>实例与恢复点</h3>
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
              <span>实际占用</span>
              <strong>{value}</strong>
            </div>
            {usage.data.locations.length > 0 && (
              <StorageLocations locations={usage.data.locations} />
            )}
          </>
        )}
      </Popover.Dropdown>
    </Popover>
  );
}
