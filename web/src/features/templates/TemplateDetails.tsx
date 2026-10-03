import { Button, Drawer, Group, Modal } from "@mantine/core";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";
import { useNavigate } from "react-router-dom";
import { Box, Monitor, RotateCw, Trash2 } from "lucide-react";
import { api, type Template } from "../../api/client";
import { ErrorMessage } from "../../foundation/Feedback";
import { memory } from "../../foundation/format";
import { Status } from "../../foundation/Status";

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
  const [confirming, setConfirming] = useState(false);
  const refresh = () => {
    void client.invalidateQueries({ queryKey: ["templates"] });
    onClose();
  };
  const remove = useMutation({
    mutationFn: () => api.deleteTemplate(template.id),
    onSuccess: refresh,
  });
  const retry = useMutation({
    mutationFn: () => api.retryOperation(template.operationId!),
    onSuccess: refresh,
  });
  const install = useMutation({
    mutationFn: () => {
      const networkId = crypto.randomUUID();
      return api.createEnvironment({
        name: `${template.name} · 安装`,
        run: true,
        spec: {
          networks: [{ id: networkId, name: "安装网络", cidr: "10.0.0.0/24" }],
          assets: [
            {
              id: crypto.randomUUID(),
              name: template.name,
              templateId: template.id,
              resources: template.resources,
              interfaces: [
                {
                  id: crypto.randomUUID(),
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
    <Drawer
      opened
      onClose={onClose}
      title="资产模板"
      position="right"
      size={460}
    >
      <div className="template-detail-heading">
        <span className={`object-symbol ${template.kind}`}>
          {template.kind === "vm" ? <Monitor size={22} /> : <Box size={22} />}
        </span>
        <div>
          <h2>{template.name}</h2>
          <span>
            {template.os} · v{template.version}
          </span>
        </div>
        <Status value={template.state ?? "importing"} />
      </div>
      {template.error && (
        <div className="template-failure" role="alert">
          {template.error}
        </div>
      )}
      {template.format === "iso" && template.state === "ready" && (
        <>
          <Button
            fullWidth
            leftSection={<Monitor size={16} />}
            loading={install.isPending}
            onClick={() => install.mutate()}
          >
            安装系统
          </Button>
          <ErrorMessage error={install.error} />
        </>
      )}
      <div className="template-specs">
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
          <span>系统盘</span>
        </div>
      </div>
      {hardware && (
        <section className="template-detail-section">
          <h3>虚拟硬件</h3>
          <dl className="template-meta">
            <dt>机器</dt>
            <dd>{hardware.machine}</dd>
            <dt>固件</dt>
            <dd>
              {hardware.firmware.toUpperCase()}
              {hardware.secureBoot ? " · Secure Boot" : ""}
            </dd>
            <dt>磁盘</dt>
            <dd>
              {hardware.diskBus.toUpperCase()}
              {hardware.diskController ? ` · ${hardware.diskController}` : ""}
            </dd>
            <dt>网卡</dt>
            <dd>{template.nicModels?.join(" · ") || hardware.nicModel}</dd>
            {hardware.tpm && (
              <>
                <dt>安全芯片</dt>
                <dd>TPM 2.0</dd>
              </>
            )}
            <dt>初始化</dt>
            <dd>{initializationNames[template.initialization ?? "none"]}</dd>
          </dl>
        </section>
      )}
      {template.disks && template.disks.length > 1 && (
        <section className="template-detail-section">
          <h3>系统磁盘</h3>
          <div className="template-disks">
            {template.disks.map((disk, index) => (
              <div key={disk.id}>
                <strong>磁盘 {index + 1}</strong>
                <span>
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
      <section className="template-detail-section">
        <h3>镜像来源</h3>
        <dl className="template-meta">
          <dt>格式</dt>
          <dd>
            {template.format?.toUpperCase() ??
              (template.kind === "container" ? "OCI" : "—")}
          </dd>
          {template.source && (
            <>
              <dt>{localSource ? "文件" : "地址"}</dt>
              <dd className="template-source">
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
      {identity.data?.administrator && (
        <div className="drawer-footer">
          <ErrorMessage error={remove.error ?? retry.error} />
          <Group justify="space-between">
            {template.error && template.operationId && (
              <Button
                variant="light"
                leftSection={<RotateCw size={15} />}
                loading={retry.isPending}
                onClick={() => retry.mutate()}
              >
                {template.state === "deleting" ? "重试删除" : "重新导入"}
              </Button>
            )}
            {template.state !== "importing" &&
              template.state !== "deleting" && (
                <Button
                  variant="subtle"
                  color="red"
                  leftSection={<Trash2 size={15} />}
                  onClick={() => setConfirming(true)}
                >
                  删除模板
                </Button>
              )}
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
    </Drawer>
  );
}
