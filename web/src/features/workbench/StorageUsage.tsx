import { useQuery } from "@tanstack/react-query";
import { HardDrive, RefreshCw } from "lucide-react";
import { api } from "../../api/client";
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
  const unit = bytes !== undefined && bytes >= 2 ** 30 ? "GiB" : "MiB";
  const value =
    bytes === undefined
      ? undefined
      : new Intl.NumberFormat("zh-CN", { maximumFractionDigits: 1 }).format(
          bytes / (unit === "GiB" ? 2 ** 30 : 2 ** 20),
        );
  return (
    <button
      className={styles.storageUsage}
      onClick={() => void usage.refetch()}
      disabled={usage.isFetching}
      title={
        usage.error ? usage.error.message : "实例与恢复点的实际占用，点击刷新"
      }
      aria-label={
        usage.error
          ? "读取存储占用失败，点击重试"
          : `磁盘实际占用 ${value ?? "读取中"} ${value ? unit : ""}`
      }
    >
      {usage.error ? <RefreshCw size={14} /> : <HardDrive size={14} />}
      <span>
        {usage.error
          ? "重试存储统计"
          : value === undefined
            ? "统计中"
            : `${value} ${unit}`}
      </span>
    </button>
  );
}
