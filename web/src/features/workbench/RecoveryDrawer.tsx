import {
  ActionIcon,
  Button,
  Drawer,
  Menu,
  Modal,
  Checkbox,
  TextInput,
} from "@mantine/core";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import {
  Archive,
  Copy,
  MoreHorizontal,
  Plus,
  RotateCcw,
  Trash2,
} from "lucide-react";
import { useState } from "react";
import { useNavigate } from "react-router-dom";
import { api, type Schema } from "../../api/client";
import { Empty, ErrorMessage, Loading } from "../../foundation/Feedback";
import { dateTime } from "../../foundation/format";
import { LoadMore } from "../../foundation/LoadMore";
import { Status } from "../../foundation/Status";
import { useCursorList } from "../../foundation/useCursorList";
import styles from "./RecoveryDrawer.module.css";

export function RecoveryDrawer({
  id,
  projectId,
  revision,
  busy,
  canManage,
  canClone,
  canCapture,
  onClose,
}: {
  id: string;
  projectId: string;
  revision: number;
  busy: boolean;
  canManage: boolean;
  canClone: boolean;
  canCapture: boolean;
  onClose: () => void;
}) {
  const client = useQueryClient();
  const navigate = useNavigate();
  const [creating, setCreating] = useState(false);
  const [name, setName] = useState("");
  const [removing, setRemoving] = useState<Schema<"RecoveryPointSummary">>();
  const [restoring, setRestoring] = useState<Schema<"RecoveryPointSummary">>();
  const [cloning, setCloning] = useState<Schema<"RecoveryPointSummary">>();
  const [cloneName, setCloneName] = useState("");
  const [runClone, setRunClone] = useState(false);
  const points = useCursorList(["recovery-points", id], (page) =>
    api.recoveryPoints(id, page),
  );
  const refresh = () => {
    for (const key of ["recovery-points", "state", "operations", "environment"])
      void client.invalidateQueries({ queryKey: [key, id] });
  };
  const capture = useMutation({
    mutationFn: () =>
      api.captureRecoveryPoint(id, { name, expectedRevision: revision }),
    onSuccess: () => {
      setCreating(false);
      refresh();
    },
  });
  const remove = useMutation({
    mutationFn: (pointId: string) => api.deleteRecoveryPoint(id, pointId),
    onSuccess: () => {
      setRemoving(undefined);
      refresh();
    },
  });
  const restore = useMutation({
    mutationFn: (pointId: string) =>
      api.restoreRecoveryPoint(id, pointId, revision),
    onSuccess: () => {
      setRestoring(undefined);
      refresh();
    },
  });
  const clone = useMutation({
    mutationFn: () =>
      api.createEnvironment({
        name: cloneName,
        projectId,
        recoveryPointId: cloning!.id,
        run: runClone,
      }),
    onSuccess: (environment) => {
      void client.invalidateQueries({ queryKey: ["environments"] });
      onClose();
      navigate(`/environments/${environment.id}`);
    },
  });
  return (
    <>
      <Drawer
        opened
        onClose={onClose}
        title="恢复点"
        position="right"
        size="lg"
      >
        <div className={styles.body}>
          <ErrorMessage error={points.error} />
          {canManage && (
            <div className={styles.toolbar}>
              <Button
                leftSection={<Plus size={15} />}
                disabled={busy || !canCapture}
                onClick={() => {
                  capture.reset();
                  setName(dateTime(new Date().toISOString()) + " 恢复点");
                  setCreating(true);
                }}
              >
                创建恢复点
              </Button>
            </div>
          )}
          {points.isLoading ? (
            <Loading />
          ) : points.data?.length ? (
            points.data.map((point) => (
              <article key={point.id} className={styles.point}>
                <div className={styles.icon}>
                  <Archive size={20} />
                </div>
                <div className={styles.content}>
                  <div className={styles.heading}>
                    <h3>{point.name}</h3>
                    <Status value={point.state} />
                  </div>
                  <div className={styles.metadata}>
                    <time dateTime={point.createdAt}>
                      {dateTime(point.createdAt)}
                    </time>
                    <span>{point.assetCount} 个资产</span>
                    {point.state === "ready" && (
                      <span>
                        {new Intl.NumberFormat("zh-CN", {
                          maximumFractionDigits: 1,
                        }).format(point.sizeBytes / 2 ** 20)}{" "}
                        MiB
                      </span>
                    )}
                  </div>
                  {point.error && (
                    <div className={styles.error} role="alert">
                      {point.error}
                    </div>
                  )}
                </div>
                {canManage && (
                  <Menu position="bottom-end">
                    <Menu.Target>
                      <ActionIcon
                        variant="subtle"
                        aria-label={`${point.name}操作`}
                      >
                        <MoreHorizontal size={18} />
                      </ActionIcon>
                    </Menu.Target>
                    <Menu.Dropdown>
                      {canClone && (
                        <Menu.Item
                          leftSection={<Copy size={15} />}
                          disabled={point.state !== "ready"}
                          onClick={() => {
                            clone.reset();
                            setCloneName(`${point.name} 副本`);
                            setRunClone(false);
                            setCloning(point);
                          }}
                        >
                          克隆为新环境
                        </Menu.Item>
                      )}
                      <Menu.Item
                        leftSection={<RotateCcw size={15} />}
                        disabled={busy || point.state !== "ready"}
                        onClick={() => {
                          restore.reset();
                          setRestoring(point);
                        }}
                      >
                        恢复
                      </Menu.Item>
                      <Menu.Item
                        color="red"
                        leftSection={<Trash2 size={15} />}
                        disabled={
                          busy ||
                          point.state === "capturing" ||
                          point.state === "deleting"
                        }
                        onClick={() => {
                          remove.reset();
                          setRemoving(point);
                        }}
                      >
                        删除
                      </Menu.Item>
                    </Menu.Dropdown>
                  </Menu>
                )}
              </article>
            ))
          ) : (
            <Empty icon={<Archive size={26} />} title="暂无恢复点" />
          )}
          <LoadMore list={points} />
        </div>
      </Drawer>
      <Modal
        opened={creating}
        onClose={() => setCreating(false)}
        title="创建恢复点"
        centered
      >
        <form
          onSubmit={(event) => {
            event.preventDefault();
            capture.mutate();
          }}
        >
          <ErrorMessage error={capture.error} />
          <TextInput
            label="名称"
            value={name}
            onChange={(event) => setName(event.currentTarget.value)}
            autoFocus
            required
          />
          <p>捕获期间环境会正常停机，完成后恢复原状态。</p>
          <div className="dialog-actions">
            <Button variant="default" onClick={() => setCreating(false)}>
              取消
            </Button>
            <Button
              type="submit"
              loading={capture.isPending}
              disabled={busy || !name.trim()}
            >
              开始捕获
            </Button>
          </div>
        </form>
      </Modal>
      <Modal
        opened={Boolean(removing)}
        onClose={() => setRemoving(undefined)}
        title="删除恢复点"
        centered
      >
        <ErrorMessage error={remove.error} />
        <p>删除「{removing?.name}」及其保存的数据？</p>
        <div className="dialog-actions">
          <Button variant="default" onClick={() => setRemoving(undefined)}>
            取消
          </Button>
          <Button
            color="red"
            loading={remove.isPending}
            onClick={() => removing && remove.mutate(removing.id)}
          >
            删除
          </Button>
        </div>
      </Modal>
      <Modal
        opened={Boolean(restoring)}
        onClose={() => setRestoring(undefined)}
        title="恢复环境"
        centered
      >
        <ErrorMessage error={restore.error} />
        <p>恢复到「{restoring?.name}」？当前资产配置与数据将被替换。</p>
        <div className="dialog-actions">
          <Button variant="default" onClick={() => setRestoring(undefined)}>
            取消
          </Button>
          <Button
            loading={restore.isPending}
            disabled={busy}
            onClick={() => restoring && restore.mutate(restoring.id)}
          >
            恢复
          </Button>
        </div>
      </Modal>
      <Modal
        opened={Boolean(cloning)}
        onClose={() => setCloning(undefined)}
        title="克隆为新环境"
        centered
      >
        <form
          onSubmit={(event) => {
            event.preventDefault();
            clone.mutate();
          }}
        >
          <ErrorMessage error={clone.error} />
          <TextInput
            label="环境名称"
            value={cloneName}
            onChange={(event) => setCloneName(event.currentTarget.value)}
            autoFocus
            required
          />
          <Checkbox
            mt="md"
            label="创建后启动"
            checked={runClone}
            onChange={(event) => setRunClone(event.currentTarget.checked)}
          />
          <div className="dialog-actions">
            <Button variant="default" onClick={() => setCloning(undefined)}>
              取消
            </Button>
            <Button
              type="submit"
              loading={clone.isPending}
              disabled={!cloneName.trim()}
            >
              创建环境
            </Button>
          </div>
        </form>
      </Modal>
    </>
  );
}
