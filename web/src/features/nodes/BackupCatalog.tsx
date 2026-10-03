import {
  ActionIcon,
  Button,
  Checkbox,
  Drawer,
  Menu,
  Modal,
  TextInput,
} from "@mantine/core";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { Archive, Copy, MoreHorizontal, RefreshCw, Trash2 } from "lucide-react";
import { useState } from "react";
import { useNavigate } from "react-router-dom";
import { api, type Schema } from "../../api/client";
import { Empty, ErrorMessage, Loading } from "../../foundation/Feedback";
import { dateTime, memory } from "../../foundation/format";
import { LoadMore } from "../../foundation/LoadMore";
import { Status } from "../../foundation/Status";
import { useCursorList } from "../../foundation/useCursorList";

export function BackupCatalog({
  repository,
  onClose,
}: {
  repository: Schema<"BackupRepository">;
  onClose: () => void;
}) {
  const client = useQueryClient(),
    navigate = useNavigate();
  const list = useCursorList(
    [
      "repository-backups",
      repository.id,
      repository.operationId,
      repository.state,
    ],
    (page) => api.repositoryBackups(repository.id, page),
    {
      refetchInterval: (items) =>
        repository.state === "connecting" ||
        items.some((item) => ["creating", "deleting"].includes(item.state))
          ? 2000
          : false,
    },
  );
  const [selected, setSelected] = useState<Schema<"BackupSummary">>();
  const [removing, setRemoving] = useState<Schema<"BackupSummary">>();
  const [name, setName] = useState(""),
    [run, setRun] = useState(true);
  const refresh = useMutation({
    mutationFn: () => api.refreshBackupRepository(repository.id),
    onSuccess: () =>
      client.invalidateQueries({ queryKey: ["backup-repositories"] }),
  });
  const restore = useMutation({
    mutationFn: () =>
      api.createEnvironment({ name, backupId: selected!.id, run }),
    onSuccess: (environment) => {
      void client.invalidateQueries({ queryKey: ["environments"] });
      onClose();
      navigate(`/environments/${environment.id}`);
    },
  });
  const remove = useMutation({
    mutationFn: () => api.deleteRepositoryBackup(repository.id, removing!.id),
    onSuccess: () => {
      setRemoving(undefined);
      void list.refetch();
    },
  });
  return (
    <>
      <Drawer
        opened
        onClose={onClose}
        position="right"
        size="lg"
        title={repository.name}
      >
        <div className="form-stack">
          <div className="collection-toolbar">
            <h2>备份</h2>
            <ActionIcon
              variant="subtle"
              aria-label="刷新仓库"
              loading={refresh.isPending || repository.state === "connecting"}
              onClick={() => refresh.mutate()}
            >
              <RefreshCw size={17} />
            </ActionIcon>
          </div>
          <ErrorMessage error={list.error ?? refresh.error} />
          {list.isPending ? (
            <Loading />
          ) : list.data?.length ? (
            <>
              <div className="table-surface">
                <table className="data-table">
                  <thead>
                    <tr>
                      <th>备份</th>
                      <th>数据量</th>
                      <th>状态</th>
                      <th />
                    </tr>
                  </thead>
                  <tbody>
                    {list.data.map((item) => (
                      <tr key={item.id}>
                        <td>
                          <strong>{item.name}</strong>
                          <span className="secondary-line">
                            {dateTime(item.createdAt)}
                          </span>
                          {item.error && (
                            <ErrorMessage error={new Error(item.error)} />
                          )}
                        </td>
                        <td>{memory(item.sizeBytes / 1024 ** 2)}</td>
                        <td>
                          <Status value={item.state} />
                        </td>
                        <td>
                          <Menu position="bottom-end">
                            <Menu.Target>
                              <ActionIcon
                                variant="subtle"
                                aria-label={item.name + "操作"}
                              >
                                <MoreHorizontal size={17} />
                              </ActionIcon>
                            </Menu.Target>
                            <Menu.Dropdown>
                              <Menu.Item
                                leftSection={<Copy size={15} />}
                                disabled={item.state !== "ready"}
                                onClick={() => {
                                  restore.reset();
                                  setSelected(item);
                                  setName(item.name + " · 恢复");
                                  setRun(true);
                                }}
                              >
                                恢复为新环境
                              </Menu.Item>
                              <Menu.Item
                                color="red"
                                leftSection={<Trash2 size={15} />}
                                disabled={["creating", "deleting"].includes(
                                  item.state,
                                )}
                                onClick={() => {
                                  remove.reset();
                                  setRemoving(item);
                                }}
                              >
                                删除备份
                              </Menu.Item>
                            </Menu.Dropdown>
                          </Menu>
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
              <LoadMore list={list} />
            </>
          ) : (
            <Empty icon={<Archive size={26} />} title="暂无备份" />
          )}
        </div>
      </Drawer>
      <Modal
        opened={Boolean(selected)}
        onClose={() => setSelected(undefined)}
        title="从备份创建环境"
        centered
      >
        <form
          className="form-stack"
          onSubmit={(event) => {
            event.preventDefault();
            restore.mutate();
          }}
        >
          <ErrorMessage error={restore.error} />
          <TextInput
            label="环境名称"
            value={name}
            onChange={(event) => setName(event.currentTarget.value)}
            autoFocus
            required
          />
          <Checkbox
            label="创建后启动"
            checked={run}
            onChange={(event) => setRun(event.currentTarget.checked)}
          />
          <div className="dialog-actions">
            <Button variant="default" onClick={() => setSelected(undefined)}>
              取消
            </Button>
            <Button
              type="submit"
              loading={restore.isPending}
              disabled={!name.trim()}
            >
              创建
            </Button>
          </div>
        </form>
      </Modal>
      <Modal
        opened={Boolean(removing)}
        onClose={() => setRemoving(undefined)}
        title="删除备份"
        centered
      >
        <ErrorMessage error={remove.error} />
        <p>删除「{removing?.name}」？</p>
        <div className="dialog-actions">
          <Button variant="default" onClick={() => setRemoving(undefined)}>
            取消
          </Button>
          <Button
            color="red"
            loading={remove.isPending}
            onClick={() => remove.mutate()}
          >
            删除
          </Button>
        </div>
      </Modal>
    </>
  );
}
