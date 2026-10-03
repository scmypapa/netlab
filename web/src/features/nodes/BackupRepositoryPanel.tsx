import {
  ActionIcon,
  Button,
  Checkbox,
  Menu,
  Modal,
  PasswordInput,
  TextInput,
} from "@mantine/core";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  Archive,
  Download,
  MoreHorizontal,
  Plus,
  RotateCcw,
  Trash2,
} from "lucide-react";
import { useState } from "react";
import { api, type Node, type Schema } from "../../api/client";
import { Empty, ErrorMessage, Loading } from "../../foundation/Feedback";
import { Status } from "../../foundation/Status";
import { BackupCatalog } from "./BackupCatalog";

export function BackupRepositoryPanel({ node }: { node: Node }) {
  const client = useQueryClient();
  const repositories = useQuery({
    queryKey: ["backup-repositories"],
    queryFn: api.backupRepositories,
    refetchInterval: (query) =>
      query.state.data?.some((repo) => repo.state === "connecting")
        ? 2000
        : false,
  });
  const [adding, setAdding] = useState(false);
  const [catalogId, setCatalogId] = useState<string>();
  const [removing, setRemoving] = useState<Schema<"BackupRepository">>();
  const [name, setName] = useState("");
  const [location, setLocation] = useState("");
  const [existing, setExisting] = useState(false);
  const [password, setPassword] = useState("");
  const [accessKey, setAccessKey] = useState("");
  const [secretKey, setSecretKey] = useState("");
  const refresh = () =>
    client.invalidateQueries({ queryKey: ["backup-repositories"] });
  const create = useMutation({
    mutationFn: () =>
      api.createBackupRepository({
        name,
        nodeId: node.id,
        location,
        initialize: !existing,
        ...(existing ? { password } : {}),
        ...(location.startsWith("s3:") ? { accessKey, secretKey } : {}),
      }),
    onSuccess: () => {
      setAdding(false);
      void refresh();
    },
  });
  const remove = useMutation({
    mutationFn: () => api.deleteBackupRepository(removing!.id),
    onSuccess: () => {
      setRemoving(undefined);
      void refresh();
    },
  });
  const retry = useMutation({
    mutationFn: api.retryOperation,
    onSuccess: refresh,
  });
  const download = useMutation({
    mutationFn: async (repo: Schema<"BackupRepository">) => {
      const credentials = await api.backupRepositoryCredentials(repo.id);
      const url = URL.createObjectURL(
        new Blob(
          [
            JSON.stringify(
              { location: repo.location, ...credentials },
              null,
              2,
            ),
          ],
          { type: "application/json" },
        ),
      );
      const link = document.createElement("a");
      link.href = url;
      link.download = "netlab-backup-repository.json";
      link.click();
      URL.revokeObjectURL(url);
    },
  });
  const items = (repositories.data ?? []).filter(
    (repo) => repo.nodeId === node.id,
  );
  const catalog = items.find((repo) => repo.id === catalogId);
  return (
    <section className="form-stack">
      <div className="collection-toolbar">
        <h2>备份仓库</h2>
        <Button
          size="xs"
          leftSection={<Plus size={15} />}
          onClick={() => {
            create.reset();
            setName("");
            setLocation("");
            setPassword("");
            setAccessKey("");
            setSecretKey("");
            setExisting(false);
            setAdding(true);
          }}
        >
          接入仓库
        </Button>
      </div>
      <ErrorMessage
        error={repositories.error ?? retry.error ?? download.error}
      />
      {repositories.isPending ? (
        <Loading />
      ) : items.length ? (
        <div className="table-surface">
          <table className="data-table">
            <thead>
              <tr>
                <th>仓库</th>
                <th>状态</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {items.map((repo) => (
                <tr key={repo.id}>
                  <td>
                    <strong>{repo.name}</strong>
                    <span className="secondary-line">{repo.location}</span>
                    {repo.error && (
                      <ErrorMessage error={new Error(repo.error)} />
                    )}
                  </td>
                  <td>
                    <Status value={repo.state} />
                  </td>
                  <td>
                    <Menu position="bottom-end">
                      <Menu.Target>
                        <ActionIcon
                          variant="subtle"
                          aria-label={repo.name + "操作"}
                        >
                          <MoreHorizontal size={17} />
                        </ActionIcon>
                      </Menu.Target>
                      <Menu.Dropdown>
                        <Menu.Item
                          leftSection={<Archive size={15} />}
                          disabled={repo.state !== "ready"}
                          onClick={() => setCatalogId(repo.id)}
                        >
                          查看备份
                        </Menu.Item>
                        {repo.state === "failed" && repo.operationId && (
                          <Menu.Item
                            leftSection={<RotateCcw size={15} />}
                            onClick={() => retry.mutate(repo.operationId!)}
                          >
                            重试连接
                          </Menu.Item>
                        )}
                        <Menu.Item
                          leftSection={<Download size={15} />}
                          onClick={() => download.mutate(repo)}
                        >
                          下载仓库配置
                        </Menu.Item>
                        <Menu.Item
                          color="red"
                          leftSection={<Trash2 size={15} />}
                          disabled={repo.state === "connecting"}
                          onClick={() => {
                            remove.reset();
                            setRemoving(repo);
                          }}
                        >
                          解除接入
                        </Menu.Item>
                      </Menu.Dropdown>
                    </Menu>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      ) : (
        <Empty icon={<Archive size={26} />} title="暂无备份仓库" />
      )}
      <Modal
        opened={adding}
        onClose={() => setAdding(false)}
        title="接入备份仓库"
        centered
      >
        <form
          className="form-stack"
          onSubmit={(event) => {
            event.preventDefault();
            create.mutate();
          }}
        >
          <ErrorMessage error={create.error} />
          <TextInput
            label="名称"
            value={name}
            onChange={(event) => setName(event.currentTarget.value)}
            autoFocus
            required
          />
          <TextInput
            label="仓库地址"
            placeholder="/mnt/backups 或 s3:https://地址/存储桶"
            value={location}
            onChange={(event) => setLocation(event.currentTarget.value)}
            required
          />
          {location.startsWith("s3:") && (
            <>
              <TextInput
                label="Access Key"
                value={accessKey}
                onChange={(event) => setAccessKey(event.currentTarget.value)}
                autoComplete="off"
              />
              <PasswordInput
                label="Secret Key"
                value={secretKey}
                onChange={(event) => setSecretKey(event.currentTarget.value)}
                autoComplete="new-password"
              />
            </>
          )}
          <Checkbox
            label="连接已存在的仓库"
            checked={existing}
            onChange={(event) => setExisting(event.currentTarget.checked)}
          />
          {existing && (
            <PasswordInput
              label="仓库密码"
              value={password}
              onChange={(event) => setPassword(event.currentTarget.value)}
              autoComplete="new-password"
              required
            />
          )}
          <div className="dialog-actions">
            <Button variant="default" onClick={() => setAdding(false)}>
              取消
            </Button>
            <Button
              type="submit"
              loading={create.isPending}
              disabled={
                !name.trim() || !location.trim() || (existing && !password)
              }
            >
              接入
            </Button>
          </div>
        </form>
      </Modal>
      {catalog && (
        <BackupCatalog
          repository={catalog}
          onClose={() => setCatalogId(undefined)}
        />
      )}
      <Modal
        opened={Boolean(removing)}
        onClose={() => setRemoving(undefined)}
        title="解除仓库接入"
        centered
      >
        <ErrorMessage error={remove.error} />
        <p>解除「{removing?.name}」的接入？仓库中的数据保留。</p>
        <div className="dialog-actions">
          <Button variant="default" onClick={() => setRemoving(undefined)}>
            取消
          </Button>
          <Button
            color="red"
            loading={remove.isPending}
            onClick={() => remove.mutate()}
          >
            解除接入
          </Button>
        </div>
      </Modal>
    </section>
  );
}
