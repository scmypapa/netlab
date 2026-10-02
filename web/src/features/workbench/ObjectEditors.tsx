import {
  Button,
  Collapse,
  Drawer,
  MultiSelect,
  NumberInput,
  Select,
  TextInput,
} from "@mantine/core";
import { ChevronDown } from "lucide-react";
import { useState } from "react";
import type {
  Asset,
  EnvironmentSpec,
  Network,
  Template,
} from "../../api/client";

export function AssetEditor({
  asset,
  templates,
  spec,
  onSave,
  onClose,
}: {
  asset?: Asset;
  templates: Template[];
  spec: EnvironmentSpec;
  onSave: (asset: Asset) => void;
  onClose: () => void;
}) {
  const [templateId, setTemplateId] = useState(asset?.templateId ?? "");
  const [name, setName] = useState(asset?.name ?? "");
  const [networkIds, setNetworkIds] = useState(
    asset?.interfaces.map((item) => item.networkId) ??
      spec.networks.slice(0, 1).map((item) => item.id),
  );
  const [cpu, setCpu] = useState(asset?.resources.cpu ?? 2);
  const [memoryGiB, setMemoryGiB] = useState(
    (asset?.resources.memoryMiB ?? 2048) / 1024,
  );
  const [disk, setDisk] = useState(asset?.resources.diskGiB ?? 20);
  const [advanced, setAdvanced] = useState(false);
  const setTemplate = (id: string | null) => {
    setTemplateId(id ?? "");
    const template = templates.find((item) => item.id === id);
    if (template) {
      setCpu(template.resources.cpu);
      setMemoryGiB(template.resources.memoryMiB / 1024);
      setDisk(template.resources.diskGiB);
      if (!name)
        setName(
          `${template.name}-${spec.assets.filter((item) => item.templateId === id).length + 1}`,
        );
    }
  };
  const save = () =>
    onSave({
      ...asset,
      id: asset?.id ?? crypto.randomUUID(),
      name,
      templateId,
      resources: { cpu, memoryMiB: memoryGiB * 1024, diskGiB: disk },
      interfaces: networkIds.map(
        (networkId, index) =>
          asset?.interfaces.find((item) => item.networkId === networkId) ?? {
            id: crypto.randomUUID(),
            networkId,
            mac: "",
            address: "",
            primary: index === 0,
          },
      ),
    });
  return (
    <Drawer
      opened
      onClose={onClose}
      title={asset ? "编辑资产" : "添加资产"}
      position="right"
      size={416}
    >
      <form
        className="form-stack"
        onSubmit={(event) => {
          event.preventDefault();
          save();
        }}
      >
        <Select
          label="资产模板"
          placeholder="选择模板"
          searchable
          required
          value={templateId}
          onChange={setTemplate}
          data={templates
            .filter((item) => item.state === "ready")
            .map((item) => ({
              value: item.id,
              label: `${item.name} · ${item.kind === "vm" ? "虚拟机" : "容器"}`,
            }))}
        />
        <TextInput
          label="资产名称"
          value={name}
          onChange={(event) => setName(event.currentTarget.value)}
          required
        />
        <MultiSelect
          label="连接网段"
          data={spec.networks.map((network) => ({
            value: network.id,
            label: `${network.name} · ${network.cidr}`,
          }))}
          value={networkIds}
          onChange={setNetworkIds}
          searchable
        />
        <div className="form-columns">
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
        </div>
        <button
          className="disclosure"
          type="button"
          aria-expanded={advanced}
          onClick={() => setAdvanced(!advanced)}
        >
          存储
          <ChevronDown size={16} className={advanced ? "rotated" : ""} />
        </button>
        <Collapse in={advanced}>
          <NumberInput
            label="系统盘 · GiB"
            min={1}
            value={disk}
            onChange={(value) => setDisk(Number(value))}
          />
        </Collapse>
        <div className="drawer-footer">
          <Button fullWidth type="submit" disabled={!templateId}>
            {asset ? "更新资产" : "添加到环境"}
          </Button>
        </div>
      </form>
    </Drawer>
  );
}

export function NetworkEditor({
  network,
  spec,
  onSave,
  onClose,
}: {
  network?: Network;
  spec: EnvironmentSpec;
  onSave: (network: Network) => void;
  onClose: () => void;
}) {
  const [name, setName] = useState(
    network?.name ?? `网段 ${spec.networks.length + 1}`,
  );
  const [cidr, setCidr] = useState(
    network?.cidr ?? `10.${spec.networks.length + 1}.0.0/24`,
  );
  const [gateway, setGateway] = useState(network?.gateway ?? "");
  const [dnsAssetId, setDnsAssetId] = useState<string | null>(
    network?.dnsAssetId ?? null,
  );
  const [dnsServers, setDnsServers] = useState(
    network?.dnsServers?.join(", ") ?? "",
  );
  const [mtu, setMtu] = useState(network?.mtu ?? 1442);
  const [advanced, setAdvanced] = useState(
    Boolean(
      network?.gateway || network?.dnsAssetId || network?.dnsServers?.length,
    ),
  );
  const save = () =>
    onSave({
      id: network?.id ?? crypto.randomUUID(),
      name,
      cidr,
      gateway: gateway || undefined,
      dnsAssetId: dnsAssetId || undefined,
      dnsServers: dnsServers
        .split(",")
        .map((item) => item.trim())
        .filter(Boolean),
      mtu,
    });
  return (
    <Drawer
      opened
      onClose={onClose}
      title={network ? "编辑网段" : "添加网段"}
      position="right"
      size={416}
    >
      <form
        className="form-stack"
        onSubmit={(event) => {
          event.preventDefault();
          save();
        }}
      >
        <TextInput
          label="网段名称"
          required
          value={name}
          onChange={(event) => setName(event.currentTarget.value)}
        />
        <TextInput
          label="网络地址 / 前缀"
          placeholder="10.1.0.0/24"
          required
          value={cidr}
          onChange={(event) => setCidr(event.currentTarget.value)}
        />
        <button
          className="disclosure"
          type="button"
          aria-expanded={advanced}
          onClick={() => setAdvanced(!advanced)}
        >
          网关与 DNS
          <ChevronDown size={16} className={advanced ? "rotated" : ""} />
        </button>
        <Collapse in={advanced}>
          <div className="form-stack">
            <TextInput
              label="网关"
              placeholder="自动分配"
              value={gateway}
              onChange={(event) => setGateway(event.currentTarget.value)}
            />
            <Select
              label="环境内 DNS"
              clearable
              searchable
              placeholder="选择 DNS 资产"
              value={dnsAssetId}
              onChange={setDnsAssetId}
              data={spec.assets.map((asset) => ({
                value: asset.id,
                label: asset.name,
              }))}
            />
            <TextInput
              label="外部 DNS"
              placeholder="例如：1.1.1.1, 8.8.8.8"
              value={dnsServers}
              onChange={(event) => setDnsServers(event.currentTarget.value)}
            />
            <NumberInput
              label="MTU"
              min={576}
              max={9000}
              value={mtu}
              onChange={(value) => setMtu(Number(value))}
            />
          </div>
        </Collapse>
        <div className="drawer-footer">
          <Button fullWidth type="submit">
            {network ? "更新网段" : "添加到环境"}
          </Button>
        </div>
      </form>
    </Drawer>
  );
}
