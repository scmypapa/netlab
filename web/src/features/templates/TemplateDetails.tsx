import { Drawer } from "@mantine/core";
import { Box, Monitor } from "lucide-react";
import type { Template } from "../../api/client";
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
                  : template.source}
              </dd>
            </>
          )}
        </dl>
      </section>
    </Drawer>
  );
}
