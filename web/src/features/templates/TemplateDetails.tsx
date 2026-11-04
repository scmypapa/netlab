import { randomUUID } from "../../foundation/id";
import { ActionIcon, Button, Group, Modal } from "@mantine/core";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";
import { useNavigate } from "react-router-dom";
import { Box, Monitor, RotateCw, Trash2 } from "lucide-react";
import { api, type Template } from "../../api/client";
import { ErrorMessage, Loading } from "../../foundation/Feedback";
import { StorageLocations } from "../../foundation/StorageLocations";
import { memory } from "../../foundation/format";
import { Status } from "../../foundation/Status";
import { allowsProject } from "../access/permissions";
import styles from "./Templates.module.css";

const initializationNames = {
  none: "保留镜像配置",
  "cloud-init": "cloud-init",
  "cloudbase-init": "Cloudbase-Init",
};

export function TemplateDetails({
  template,
  onClose,
}: {
  template: Template;
  onClose: () => void;
}) {
  const hardware = template.hardware;
  const navigate = useNavigate();
  const client = useQueryClient();
  const identity = useQuery({ queryKey: ["identity"], queryFn: api.identity });
  const canInstall = allowsProject(identity.data, "compose", "default");
  const [confirming, setConfirming] = useState(false);
  const refresh = () => {
    void client.invalidateQueries({ queryKey: ["templates"] });
    onClose();
  };
  const remove = useMutation({
    mutationFn: () => api.deleteTemplate(template.id),
    onSuccess: refresh,
  });
  const cache = useQuery({
    queryKey: ["template-cache", template.id],
    queryFn: () => api.templateCache(template.id),
    enabled:
      identity.data?.administrator === true &&
      template.kind === "vm" &&
      template.state === "ready",
  });
  const references = useQuery({
    queryKey: ["template-references", template.id],
    queryFn: () => api.templateReferences(template.id),
    enabled: identity.data?.administrator === true,
  });
  const retry = useMutation({
    mutationFn: () => api.retryOperation(template.operationId!),
    onSuccess: refresh,
  });
  const install = useMutation({
    mutationFn: () => {
      const networkId = randomUUID();
      return api.createEnvironment({
        name: `${template.name} · 安装`,
        run: true,
        spec: {
          networks: [{ id: networkId, name: "安装网络", cidr: "10.0.0.0/24" }],
          assets: [
            {
              id: randomUUID(),
              name: template.name,
              templateId: template.id,
              resources: template.resources,
              interfaces: [
                {
                  id: randomUUID(),
                  networkId,
                  mac: "",
                  address: "",
                  primary: true,
                },
              ],
            },
          ],
        },
      });
    },
    onSuccess: (environment) => navigate(`/environments/${environment.id}`),
  });
  const localSource =
    template.source.startsWith("/") || template.source.startsWith("file:");
  return (
    <section className={styles.detail} role="region" aria-label="资产模板">
      <div className={styles.detailHeading}>
        <div>
          <h2>{template.name}</h2>
          <span>
            {template.kind === "vm" ? <Monitor size={16} /> : <Box size={16} />}
            {template.os} · v{template.version}{" "}
            <Status value={template.state ?? "importing"} />
          </span>
        </div>
        {identity.data?.administrator &&
          template.state !== "importing" &&
          template.state !== "deleting" && (
            <ActionIcon
              variant="subtle"
              color="red"
              aria-label="删除模板"
              onClick={() => setConfirming(true)}
            >
              <Trash2 size={17} />
            </ActionIcon>
          )}
      </div>
      {template.error && (
        <div className="template-failure" role="alert">
          {template.error}
        </div>
      )}
      {canInstall &&
        template.format === "iso" &&
        template.state === "ready" && (
          <>
            <Button
              leftSection={<Monitor size={16} />}
              loading={install.isPending}
              onClick={() => install.mutate()}
            >
              安装系统
            </Button>
            <ErrorMessage error={install.error} />
          </>
        )}
      <div className={styles.specs}>
        <div>
          <strong>
            {template.resources.cpu}
            <span> 核</span>
          </strong>
          <span>CPU</span>
        </div>
        <div>
          <strong>{memory(template.resources.memoryMiB)}</strong>
          <span>内存</span>
        </div>
        <div>
          <strong>
            {template.resources.diskGiB}
            <span> GiB</span>
          </strong>
          <span>{template.kind === "container" ? "磁盘预算" : "磁盘容量"}</span>
        </div>
      </div>
      {identity.data?.administrator && (
        <div className={styles.detailColumns}>
          {template.kind === "vm" && template.state === "ready" && (
            <section className={styles.section} aria-label="镜像文件">
              <h3>镜像文件</h3>
              <ErrorMessage error={cache.error} />
              {cache.isPending && <Loading />}
              {cache.data && cache.data.length > 0 && (
                <StorageLocations
                  locations={cache.data.map((item) => ({
                    id: item.nodeId,
                    name: item.nodeName,
                    bytes: item.bytes,
                  }))}
                />
              )}
              {cache.data?.length === 0 && (
                <p className={styles.emptyStorage}>节点上无镜像副本</p>
              )}
            </section>
          )}
          <section className={styles.section} aria-label="使用此镜像">
            <h3>使用此镜像</h3>
            <ErrorMessage error={references.error} />
            {references.isPending && <Loading />}
            {references.data?.length === 0 && (
              <p className={styles.emptyStorage}>暂无引用</p>
            )}
            {references.data && references.data.length > 0 && (
              <ul className={styles.references}>
                {references.data.map((reference) => (
                  <li key={reference}>{reference}</li>
                ))}
              </ul>
            )}
          </section>
        </div>
      )}
      <div className={styles.detailColumns}>
        {hardware && (
          <section className={styles.section}>
            <h3>虚拟硬件</h3>
            <dl className={styles.meta}>
              <dt>机器</dt>
              <dd className={styles.technical}>{hardware.machine}</dd>
              <dt>固件</dt>
              <dd className={styles.technical}>
                {hardware.firmware.toUpperCase()}
                {hardware.secureBoot ? " · Secure Boot" : ""}
              </dd>
              <dt>磁盘</dt>
              <dd className={styles.technical}>
                {hardware.diskBus.toUpperCase()}
                {hardware.diskController ? ` · ${hardware.diskController}` : ""}
              </dd>
              <dt>网卡</dt>
              <dd className={styles.technical}>
                {template.nicModels?.join(" · ") || hardware.nicModel}
              </dd>
              {hardware.tpm && (
                <>
                  <dt>安全芯片</dt>
                  <dd className={styles.technical}>TPM 2.0</dd>
                </>
              )}
              <dt>初始化</dt>
              <dd>{initializationNames[template.initialization ?? "none"]}</dd>
            </dl>
          </section>
        )}
        {template.disks && template.disks.length > 1 && (
          <section className={styles.section}>
            <h3>系统磁盘</h3>
            <div className={styles.disks}>
              {template.disks.map((disk, index) => (
                <div key={disk.id}>
                  <strong>磁盘 {index + 1}</strong>
                  <span className={styles.technical}>
                    {disk.bus.toUpperCase()} · {disk.sizeGiB} GiB
                  </span>
                  {disk.bootOrder === 1 && (
                    <span className="asset-count">启动盘</span>
                  )}
                </div>
              ))}
            </div>
          </section>
        )}
        <section className={styles.section}>
          <h3>镜像来源</h3>
          <dl className={styles.meta}>
            <dt>格式</dt>
            <dd className={styles.technical}>
              {template.format?.toUpperCase() ??
                (template.kind === "container" ? "OCI" : "—")}
            </dd>
            {template.source && (
              <>
                <dt>{localSource ? "文件" : "地址"}</dt>
                <dd className={`${styles.technical} template-source`}>
                  {localSource
                    ? template.source.split("/").at(-1)
                    : template.source.startsWith("asset:")
                      ? "运行资产"
                      : template.source}
                </dd>
              </>
            )}
          </dl>
        </section>
      </div>
      {identity.data?.administrator &&
        template.error &&
        template.operationId && (
          <div className={styles.section}>
            <ErrorMessage error={remove.error ?? retry.error} />
            <Group justify="space-between">
              <Button
                variant="light"
                leftSection={<RotateCw size={15} />}
                loading={retry.isPending}
                onClick={() => retry.mutate()}
              >
                {template.state === "deleting" ? "重试删除" : "重新导入"}
              </Button>
            </Group>
          </div>
        )}
      <Modal
        opened={confirming}
        onClose={() => setConfirming(false)}
        title={`删除 ${template.name}？`}
        centered
        size="sm"
      >
        <ErrorMessage error={remove.error} />
        <Group justify="flex-end">
          <Button variant="default" onClick={() => setConfirming(false)}>
            取消
          </Button>
          <Button
            color="red"
            loading={remove.isPending}
            onClick={() => remove.mutate()}
          >
            删除
          </Button>
        </Group>
      </Modal>
    </section>
  );
}
