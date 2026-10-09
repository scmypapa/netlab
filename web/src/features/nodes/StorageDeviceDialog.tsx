import { Button, Checkbox, Modal, Radio, Select } from "@mantine/core";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useState } from "react";
import { api, type Node } from "../../api/client";
import { ErrorMessage, Loading } from "../../foundation/Feedback";
import { Status } from "../../foundation/Status";
import styles from "./StorageWorkspace.module.css";

export function StorageDeviceDialog({
  nodes,
  nodeId,
  onClose,
}: {
  nodes: Node[];
  nodeId?: string;
  onClose: () => void;
}) {
  const client = useQueryClient();
  const [target, setTarget] = useState<string | null>(
    nodeId ??
      nodes.find((n) => n.state === "ready" && n.capabilities.includes("vm"))
        ?.id ??
      null,
  );
  const [device, setDevice] = useState<string>();
  const [confirmed, setConfirmed] = useState(false);
  const disks = useQuery({
    queryKey: ["storage-devices", target],
    queryFn: () => api.nodeStorageDevices(target!),
    enabled: Boolean(target),
    refetchInterval: 3000,
  });
  const prepare = useMutation({
    mutationFn: (path: string) => api.configureNodeStorage(target!, path),
    onSuccess: () => {
      void client.invalidateQueries({ queryKey: ["storage-devices", target] });
    },
  });
  const retry = useMutation({
    mutationFn: api.retryOperation,
    onSuccess: () => {
      void client.invalidateQueries({ queryKey: ["storage-devices", target] });
    },
  });
  const operation = disks.data?.operation;
  const busy =
    prepare.isPending ||
    operation?.state === "queued" ||
    operation?.state === "running";
  useEffect(() => {
    if (operation?.state === "succeeded" && operation.id === prepare.data?.id) {
      void client.invalidateQueries({ queryKey: ["nodes"] });
      void client.invalidateQueries({ queryKey: ["storage-pools"] });
      onClose();
    }
  }, [operation?.state, operation?.id, prepare.data?.id, client, onClose]);
  const available = disks.data?.devices.filter((d) => d.available) ?? [];
  const inUse = disks.data?.devices.some(
    (disk) => disk.path === disks.data.selected && !disk.available,
  );
  const selected = device ?? available[0]?.path;
  return (
    <Modal opened onClose={onClose} title="Ceph 专用盘" centered size="lg">
      <div className="form-stack">
        <Select
          label="节点"
          value={target}
          disabled={busy}
          onChange={(id) => {
            setTarget(id);
            setDevice(undefined);
            setConfirmed(false);
            prepare.reset();
          }}
          data={nodes
            .filter((n) => n.state === "ready" && n.capabilities.includes("vm"))
            .map((n) => ({ value: n.id, label: n.name }))}
        />
        <ErrorMessage error={disks.error ?? prepare.error ?? retry.error} />
        {disks.isPending && target ? (
          <Loading />
        ) : (
          <>
            {disks.data?.selected && (
              <div className={styles.label}>
                <strong>当前专用盘</strong>
                <span>{disks.data.selected}</span>
              </div>
            )}
            <div className={styles.disks}>
              {disks.data?.devices.map((disk) => (
                <div className={styles.disk} key={disk.path}>
                  <Radio
                    label={
                      <span>
                        <strong>{disk.model || disk.path}</strong>
                        <span className="secondary-line">{disk.path}</span>
                      </span>
                    }
                    checked={selected === disk.path}
                    disabled={!disk.available || busy}
                    onChange={() => {
                      setDevice(disk.path);
                      setConfirmed(false);
                    }}
                  />
                  <span>
                    {Math.floor(disk.sizeBytes / 2 ** 30)} GiB
                    {!disk.available && (
                      <span className="secondary-line">{disk.reason}</span>
                    )}
                  </span>
                </div>
              ))}
            </div>
            {!available.length && !disks.error && target && (
              <div>没有可用空盘</div>
            )}
            {available.length > 0 && (
              <Checkbox
                checked={confirmed}
                disabled={busy}
                onChange={(event) => setConfirmed(event.currentTarget.checked)}
                label="将所选空盘交给 Ceph 初始化"
              />
            )}
          </>
        )}
        {operation && operation.state !== "succeeded" && (
          <div className={styles.task}>
            <Status value={operation.state} />
            <span>准备节点存储</span>
            {operation.error && (
              <ErrorMessage error={new Error(operation.error)} />
            )}
            {operation.state === "failed" && (
              <Button
                size="compact-sm"
                variant="default"
                loading={retry.isPending}
                onClick={() => retry.mutate(operation.id)}
              >
                重试准备
              </Button>
            )}
          </div>
        )}
        <div className="dialog-actions">
          {disks.data?.selected && (
            <Button
              variant="subtle"
              color="gray"
              disabled={busy || inUse}
              onClick={() => prepare.mutate("")}
            >
              取消选盘
            </Button>
          )}
          <Button variant="default" onClick={onClose}>
            关闭
          </Button>
          <Button
            disabled={!selected || !confirmed || busy || Boolean(disks.error)}
            loading={busy}
            onClick={() => prepare.mutate(selected!)}
          >
            确认选盘
          </Button>
        </div>
      </div>
    </Modal>
  );
}
