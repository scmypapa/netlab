import {
  Button,
  Drawer,
  NumberInput,
  Select,
  Switch,
  TextInput,
} from "@mantine/core";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Box, Boxes, Monitor, Plus, Search } from "lucide-react";
import { useState } from "react";
import { useSearchParams } from "react-router-dom";
import { api, type Template } from "../../api/client";
import { Empty, ErrorMessage, Loading } from "../../foundation/Feedback";
import { memory } from "../../foundation/format";
import { Status } from "../../foundation/Status";
import { BlueprintsPanel } from "./BlueprintsPanel";

export function TemplatesPage() {
  const [params, setParams] = useSearchParams();
  const section =
    params.get("tab") === "environments" ? "environments" : "assets";
  return (
    <main className="collection-page">
      <div className="page-heading">
        <h1>模板</h1>
        <div className="view-switch" role="group" aria-label="模板分类">
          {[
            ["assets", "资产模板"],
            ["environments", "环境模板"],
          ].map(([value, label]) => (
            <button
              key={value}
              className={section === value ? "selected" : ""}
              aria-pressed={section === value}
              onClick={() =>
                setParams(value === "assets" ? {} : { tab: value })
              }
            >
              {label}
            </button>
          ))}
        </div>
      </div>
      {section === "assets" ? <AssetTemplatesPanel /> : <BlueprintsPanel />}
    </main>
  );
}

function AssetTemplatesPanel() {
  const templates = useQuery({
    queryKey: ["templates"],
    queryFn: api.templates,
    refetchInterval: (query) =>
      query.state.data?.some((template) => template.state === "importing")
        ? 2500
        : false,
  });
  const [kind, setKind] = useState("all");
  const [query, setQuery] = useState("");
  const [creating, setCreating] = useState(false);
  const client = useQueryClient();
  const items = (templates.data ?? []).filter(
    (item) =>
      (kind === "all" || item.kind === kind) &&
      `${item.name} ${item.os}`
        .toLocaleLowerCase()
        .includes(query.toLocaleLowerCase()),
  );
  return (
    <>
      <div className="collection-toolbar">
        <div className="filter-tabs" role="group" aria-label="模板类型">
          {[
            ["all", "全部模板"],
            ["vm", "虚拟机"],
            ["container", "容器"],
          ].map(([value, label]) => (
            <button
              key={value}
              className={kind === value ? "selected" : ""}
              onClick={() => setKind(value)}
              aria-pressed={kind === value}
            >
              {label}
            </button>
          ))}
        </div>
        <TextInput
          aria-label="搜索模板"
          placeholder="搜索模板"
          leftSection={<Search size={16} />}
          value={query}
          onChange={(event) => setQuery(event.currentTarget.value)}
        />
        <Button
          leftSection={<Plus size={16} />}
          onClick={() => setCreating(true)}
        >
          导入模板
        </Button>
      </div>
      <ErrorMessage error={templates.error} />
      {templates.isPending ? (
        <Loading />
      ) : items.length ? (
        <div className="table-surface">
          <table className="data-table">
            <thead>
              <tr>
                <th>模板</th>
                <th>系统</th>
                <th>默认规格</th>
                <th>启动方式</th>
                <th>状态</th>
              </tr>
            </thead>
            <tbody>
              {items.map((template) => (
                <tr key={template.id}>
                  <td>
                    <div className="object-link">
                      <span className={`object-symbol ${template.kind}`}>
                        {template.kind === "vm" ? (
                          <Monitor size={20} />
                        ) : (
                          <Box size={20} />
                        )}
                      </span>
                      <span>
                        <strong>{template.name}</strong>
                        <span className="secondary-line">
                          {template.kind === "vm" ? "虚拟机" : "容器"} · v
                          {template.version}
                        </span>
                      </span>
                    </div>
                  </td>
                  <td>{template.os}</td>
                  <td className="numeric">
                    {template.resources.cpu} 核
                    <span className="muted">
                      {" "}
                      · {memory(template.resources.memoryMiB)} ·{" "}
                      {template.resources.diskGiB} GiB
                    </span>
                  </td>
                  <td className="muted">
                    {template.kind === "vm"
                      ? template.hardware?.firmware.toUpperCase()
                      : "OCI"}
                  </td>
                  <td>
                    <Status value={template.state ?? "importing"} />
                    {template.error && (
                      <span className="error-inline">{template.error}</span>
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      ) : (
        !templates.error && (
          <Empty
            icon={<Boxes size={30} />}
            title={
              query || kind !== "all"
                ? "没有匹配的模板"
                : "导入可复用的资产模板"
            }
            action={!query && kind === "all" ? "导入模板" : undefined}
            onAction={() => setCreating(true)}
          />
        )
      )}
      <Drawer
        opened={creating}
        onClose={() => setCreating(false)}
        title="导入资产模板"
        position="right"
        size={440}
      >
        <TemplateForm
          onCreated={() => {
            setCreating(false);
            void client.invalidateQueries({ queryKey: ["templates"] });
          }}
        />
      </Drawer>
    </>
  );
}

function TemplateForm({ onCreated }: { onCreated: () => void }) {
  const [kind, setKind] = useState<Template["kind"]>("container");
  const [name, setName] = useState("");
  const [os, setOs] = useState("Linux");
  const [source, setSource] = useState("");
  const [format, setFormat] = useState<NonNullable<Template["format"]>>("oci");
  const [cpu, setCpu] = useState(2);
  const [memoryGiB, setMemoryGiB] = useState(2);
  const [disk, setDisk] = useState(20);
  const [firmware, setFirmware] = useState<"bios" | "uefi">("uefi");
  const [diskBus, setDiskBus] = useState<"ide" | "sata" | "scsi" | "virtio">(
    "virtio",
  );
  const [nicModel, setNicModel] = useState<
    "virtio" | "e1000" | "e1000e" | "rtl8139"
  >("virtio");
  const [secureBoot, setSecureBoot] = useState(false);
  const [tpm, setTpm] = useState(false);
  const create = useMutation({
    mutationFn: () =>
      api.createTemplate({
        id: crypto.randomUUID(),
        name,
        kind,
        os,
        source,
        version: 1,
        format,
        resources: { cpu, memoryMiB: memoryGiB * 1024, diskGiB: disk },
        ...(kind === "vm"
          ? {
              hardware: {
                firmware,
                machine: firmware === "uefi" ? "q35" : "pc",
                diskBus,
                nicModel,
                secureBoot,
                tpm,
                guestAgent: true,
              },
            }
          : {}),
      }),
    onSuccess: onCreated,
  });
  return (
    <form
      className="form-stack"
      onSubmit={(event) => {
        event.preventDefault();
        create.mutate();
      }}
    >
      <div className="type-picker">
        <button
          type="button"
          className={kind === "container" ? "selected" : ""}
          onClick={() => {
            setKind("container");
            setFormat("oci");
          }}
        >
          <Box size={21} />
          容器
        </button>
        <button
          type="button"
          className={kind === "vm" ? "selected" : ""}
          onClick={() => {
            setKind("vm");
            setFormat("qcow2");
          }}
        >
          <Monitor size={21} />
          虚拟机
        </button>
      </div>
      <TextInput
        label="模板名称"
        required
        value={name}
        onChange={(event) => setName(event.currentTarget.value)}
      />
      <div className="form-columns">
        <Select
          label="操作系统"
          value={os}
          onChange={(value) => setOs(value!)}
          data={["Linux", "Windows", "其他"]}
        />
        <Select
          label="镜像格式"
          value={format}
          onChange={(value) => setFormat(value as typeof format)}
          data={
            kind === "container"
              ? [
                  { value: "oci", label: "OCI / Registry" },
                  { value: "docker", label: "Docker 镜像包" },
                ]
              : ["qcow2", "raw", "vmdk", "ova", "iso"]
          }
        />
      </div>
      <TextInput
        label={
          kind === "container" && format === "oci" ? "镜像地址" : "镜像文件地址"
        }
        placeholder={
          kind === "container" && format === "oci"
            ? "docker.io/library/ubuntu:24.04"
            : "https://storage.example.com/image.qcow2"
        }
        required
        value={source}
        onChange={(event) => setSource(event.currentTarget.value)}
      />
      <h3 className="form-section-title">默认规格</h3>
      <div className="form-columns three">
        <NumberInput
          label="CPU · 核"
          min={1}
          value={cpu}
          onChange={(value) => setCpu(Number(value))}
        />
        <NumberInput
          label="内存 · GiB"
          min={0.25}
          step={0.25}
          value={memoryGiB}
          onChange={(value) => setMemoryGiB(Number(value))}
        />
        <NumberInput
          label="磁盘 · GiB"
          min={1}
          value={disk}
          onChange={(value) => setDisk(Number(value))}
        />
      </div>
      {kind === "vm" && (
        <>
          <h3 className="form-section-title">虚拟硬件</h3>
          <div className="form-columns">
            <Select
              label="固件"
              value={firmware}
              onChange={(value) => {
                setFirmware(value as typeof firmware);
                if (value === "bios") setSecureBoot(false);
              }}
              data={[
                { value: "bios", label: "BIOS" },
                { value: "uefi", label: "UEFI" },
              ]}
            />
            <Select
              label="磁盘控制器"
              value={diskBus}
              onChange={(value) => setDiskBus(value as typeof diskBus)}
              data={["virtio", "sata", "scsi", "ide"]}
            />
            <Select
              label="网卡"
              value={nicModel}
              onChange={(value) => setNicModel(value as typeof nicModel)}
              data={["virtio", "e1000", "e1000e", "rtl8139"]}
            />
          </div>
          <Switch
            label="Secure Boot"
            checked={secureBoot}
            onChange={(event) => setSecureBoot(event.currentTarget.checked)}
            disabled={firmware === "bios"}
          />
          <Switch
            label="TPM 2.0"
            checked={tpm}
            onChange={(event) => setTpm(event.currentTarget.checked)}
          />
        </>
      )}
      <ErrorMessage error={create.error} />
      <div className="drawer-footer">
        <Button fullWidth type="submit" loading={create.isPending}>
          导入模板
        </Button>
      </div>
    </form>
  );
}
