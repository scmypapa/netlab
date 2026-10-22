import { randomUUID } from "../../foundation/id";
import {
  ActionIcon,
  Button,
  Collapse,
  Drawer,
  MultiSelect,
  NumberInput,
  Select,
  SegmentedControl,
  Textarea,
  TextInput,
} from "@mantine/core";
import { ChevronDown, Plus, Trash2 } from "lucide-react";
import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import {
  api,
  type Asset,
  type EnvironmentSpec,
  type Network,
  type Schema,
  type Template,
} from "../../api/client";
import { LoadMore, type CursorPagination } from "../../foundation/LoadMore";
import { useCursorList } from "../../foundation/useCursorList";
import { ErrorMessage } from "../../foundation/Feedback";

export function AssetEditor({
  asset,
  templates,
  pagination,
  search,
  onSearch,
  spec,
  onSave,
  onClose,
}: {
  asset?: Asset;
  templates: Template[];
  pagination: CursorPagination;
  search: string;
  onSearch: (value: string) => void;
  spec: EnvironmentSpec;
  onSave: (asset: Asset) => void;
  onClose: () => void;
}) {
  const [templateId, setTemplateId] = useState(asset?.templateId ?? "");
  const [chosenTemplate, setChosenTemplate] = useState(
    templates.find((item) => item.id === asset?.templateId),
  );
  const [media, setMedia] = useState(asset?.media);
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
  const [volumes, setVolumes] = useState<Schema<"Volume">[]>(
    asset?.volumes ?? [],
  );
  const [volumesOpen, setVolumesOpen] = useState(
    Boolean(asset?.volumes?.length),
  );
  const identity = useQuery({ queryKey: ["identity"], queryFn: api.identity });
  const [devicesOpen, setDevicesOpen] = useState(Boolean(asset?.pciBinding));
  const [pciBinding, setPciBinding] = useState(asset?.pciBinding);
  const deviceNodes = useCursorList(["nodes", "pci-devices"], api.nodes, {
    enabled: devicesOpen && Boolean(identity.data?.administrator),
  });
  const availableVolumes = useQuery({
    queryKey: ["volumes"],
    queryFn: api.volumes,
    enabled: volumesOpen && Boolean(identity.data?.administrator),
  });
  const [storagePoolId, setStoragePoolId] = useState<string | null>(
    asset?.storagePoolId ?? null,
  );
  const storage = useQuery({
    queryKey: ["storage-pools"],
    queryFn: api.storagePools,
    enabled: advanced && !asset,
  });
  const [restartPolicy, setRestartPolicy] = useState<Schema<"RestartPolicy">>(
    asset?.restartPolicy ?? "never",
  );
  const [guestOpen, setGuestOpen] = useState(Boolean(asset?.guest));
  const [hostname, setHostname] = useState(asset?.guest?.hostname ?? "");
  const [username, setUsername] = useState(asset?.guest?.username ?? "");
  const [sshKeys, setSshKeys] = useState(
    asset?.guest?.sshAuthorizedKeys?.join("\n") ?? "",
  );
  const template =
    chosenTemplate ?? templates.find((item) => item.id === templateId);
  const initialized =
    template?.kind === "vm" && template.initialization !== "none";
  const setTemplate = (id: string | null) => {
    setTemplateId(id ?? "");
    const template = templates.find((item) => item.id === id);
    setChosenTemplate(template);
    if (template) {
      if (!asset) setVolumes(template.volumes ?? []);
      setMedia(template.media?.map((item) => item.id));
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
      id: asset?.id ?? randomUUID(),
      name,
      templateId,
      storagePoolId: storagePoolId ?? undefined,
      resources: { cpu, memoryMiB: memoryGiB * 1024, diskGiB: disk },
      volumes,
      media: template?.media?.length ? media : undefined,
      restartPolicy: template?.kind === "container" ? restartPolicy : undefined,
      pciBinding:
        template?.kind === "vm" && pciBinding?.groupIds.length
          ? pciBinding
          : undefined,
      guest: initialized
        ? {
            hostname: hostname.trim() || undefined,
            username: username.trim() || undefined,
            sshAuthorizedKeys: sshKeys
              .split("\n")
              .map((key) => key.trim())
              .filter(Boolean),
          }
        : undefined,
      interfaces: networkIds.map(
        (networkId, index) =>
          asset?.interfaces.find((item) => item.networkId === networkId) ?? {
            id: randomUUID(),
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
          searchValue={search}
          onSearchChange={onSearch}
          required
          value={templateId}
          onChange={setTemplate}
          data={[
            ...new Map(
              [...templates, ...(chosenTemplate ? [chosenTemplate] : [])].map(
                (item) => [item.id, item],
              ),
            ).values(),
          ]
            .filter((item) => item.state === "ready")
            .map((item) => ({
              value: item.id,
              label: `${item.name} · ${item.kind === "vm" ? "虚拟机" : "容器"}`,
            }))}
        />
        <LoadMore list={pagination} />
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
        {template?.kind === "container" && (
          <Select
            label="进程退出后"
            value={restartPolicy}
            onChange={(value) =>
              setRestartPolicy(value as Schema<"RestartPolicy">)
            }
            allowDeselect={false}
            data={[
              { value: "never", label: "保持停止" },
              { value: "on-failure", label: "异常退出时重启" },
              { value: "always", label: "自动重启" },
            ]}
          />
        )}
        {Boolean(template?.media?.length) && (
          <MultiSelect
            label="光驱"
            data={template!.media!.map((item) => ({
              value: item.id,
              label: item.source.split("/").at(-1) ?? item.source,
            }))}
            value={media ?? template!.media!.map((item) => item.id)}
            onChange={setMedia}
          />
        )}
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
          {!asset && (
            <Select
              label="存储位置"
              placeholder="自动分配"
              clearable
              value={storagePoolId}
              onChange={setStoragePoolId}
              data={(storage.data ?? [])
                .filter(
                  (pool) =>
                    !pool.default &&
                    pool.state === "ready" &&
                    !pool.error &&
                    (template?.kind === "vm" ||
                      pool.capabilities.includes("volumes")),
                )
                .map((pool) => ({ value: pool.id, label: pool.name }))}
            />
          )}
          <ErrorMessage error={storage.error} />
          <NumberInput
            label="系统盘 · GiB"
            min={1}
            value={disk}
            onChange={(value) => setDisk(Number(value))}
          />
        </Collapse>
        {template?.kind === "vm" && identity.data?.administrator && (
          <>
            <button
              className="disclosure"
              type="button"
              aria-expanded={devicesOpen}
              onClick={() => setDevicesOpen(!devicesOpen)}
            >
              PCI 直通{" "}
              <ChevronDown size={16} className={devicesOpen ? "rotated" : ""} />
            </button>
            <Collapse in={devicesOpen}>
              <Select
                label="宿主节点"
                clearable
                value={pciBinding?.nodeId ?? null}
                data={(deviceNodes.data ?? [])
                  .filter((node) => node.vmHardware?.pciGroups?.length)
                  .map((node) => ({ value: node.id, label: node.name }))}
                onChange={(nodeId) =>
                  setPciBinding(nodeId ? { nodeId, groupIds: [] } : undefined)
                }
              />
              {pciBinding && (
                <MultiSelect
                  label="设备组"
                  searchable
                  value={pciBinding.groupIds}
                  data={(
                    deviceNodes.data?.find(
                      (node) => node.id === pciBinding.nodeId,
                    )?.vmHardware?.pciGroups ?? []
                  ).map((group) => ({
                    value: group.id,
                    label: group.name,
                    disabled:
                      !group.available &&
                      !(
                        asset?.pciBinding?.nodeId === pciBinding.nodeId &&
                        asset.pciBinding.groupIds.includes(group.id)
                      ),
                  }))}
                  onChange={(groupIds) =>
                    setPciBinding({ ...pciBinding, groupIds })
                  }
                />
              )}
              <ErrorMessage error={deviceNodes.error} />
              <LoadMore list={deviceNodes} />
            </Collapse>
          </>
        )}
        <button
          className="disclosure"
          type="button"
          aria-expanded={volumesOpen}
          onClick={() => setVolumesOpen(!volumesOpen)}
        >
          数据卷
          <ChevronDown size={16} className={volumesOpen ? "rotated" : ""} />
        </button>
        <Collapse in={volumesOpen}>
          <div className="form-stack">
            {volumes.map((volume, index) => {
              const update = (values: Partial<Schema<"Volume">>) =>
                setVolumes((items) =>
                  items.map((item, i) =>
                    i === index ? { ...item, ...values } : item,
                  ),
                );
              return (
                <div className="form-stack" key={volume.id}>
                  <div className="form-columns">
                    <Select
                      label="数据来源"
                      clearable
                      placeholder="随资产创建"
                      value={volume.persistentVolumeId ?? null}
                      onChange={(id) => {
                        const source = availableVolumes.data?.find(
                          (item) => item.id === id,
                        );
                        update({
                          persistentVolumeId: id ?? undefined,
                          sizeGiB: source?.sizeGiB ?? volume.sizeGiB,
                        });
                        if (source)
                          setStoragePoolId(
                            source.storagePoolId.startsWith("default:")
                              ? null
                              : source.storagePoolId,
                          );
                      }}
                      data={
                        !identity.data?.administrator &&
                        volume.persistentVolumeId
                          ? [
                              {
                                value: volume.persistentVolumeId,
                                label: "已附加数据卷",
                              },
                            ]
                          : (availableVolumes.data ?? [])
                              .filter(
                                (item) =>
                                  item.kind === template?.kind &&
                                  (item.state === "ready" ||
                                    item.id === volume.persistentVolumeId),
                              )
                              .map((item) => ({
                                value: item.id,
                                label: item.name,
                              }))
                      }
                    />
                    <ActionIcon
                      variant="subtle"
                      color="red"
                      aria-label={`移除数据卷 ${index + 1}`}
                      onClick={() =>
                        setVolumes((items) =>
                          items.filter((_, i) => i !== index),
                        )
                      }
                    >
                      <Trash2 size={16} />
                    </ActionIcon>
                  </div>
                  <div className="form-columns">
                    <TextInput
                      label={template?.kind === "vm" ? "盘名" : "挂载目录"}
                      value={volume.mountPath}
                      required
                      onChange={(event) =>
                        update({ mountPath: event.currentTarget.value })
                      }
                    />
                    <NumberInput
                      label="容量 · GiB"
                      value={volume.sizeGiB}
                      min={1}
                      disabled={Boolean(volume.persistentVolumeId)}
                      onChange={(value) => update({ sizeGiB: Number(value) })}
                    />
                  </div>
                </div>
              );
            })}
            <ErrorMessage error={availableVolumes.error} />
            <Button
              variant="default"
              size="compact-sm"
              leftSection={<Plus size={15} />}
              onClick={() =>
                setVolumes((items) => [
                  ...items,
                  {
                    id: randomUUID(),
                    mountPath:
                      template?.kind === "vm"
                        ? `数据盘 ${items.length + 1}`
                        : `/data${items.length ? items.length + 1 : ""}`,
                    sizeGiB: 10,
                  },
                ])
              }
            >
              添加数据卷
            </Button>
          </div>
        </Collapse>
        {initialized && (
          <>
            <button
              className="disclosure"
              type="button"
              aria-expanded={guestOpen}
              onClick={() => setGuestOpen(!guestOpen)}
            >
              来宾设置
              <ChevronDown size={16} className={guestOpen ? "rotated" : ""} />
            </button>
            <Collapse in={guestOpen}>
              <div className="form-stack">
                <div className="form-columns">
                  <TextInput
                    label="主机名"
                    value={hostname}
                    onChange={(event) => setHostname(event.currentTarget.value)}
                  />
                  <TextInput
                    label="登录用户"
                    value={username}
                    onChange={(event) => setUsername(event.currentTarget.value)}
                  />
                </div>
                <Textarea
                  label="SSH 公钥"
                  placeholder="ssh-ed25519 …"
                  minRows={3}
                  autosize
                  value={sshKeys}
                  onChange={(event) => setSshKeys(event.currentTarget.value)}
                />
              </div>
            </Collapse>
          </>
        )}
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
  const identity = useQuery({ queryKey: ["identity"], queryFn: api.identity });
  const [external, setExternal] = useState(Boolean(network?.external));
  const [nodeId, setNodeId] = useState(network?.external?.nodeId ?? "");
  const [interfaceName, setInterfaceName] = useState(
    network?.external?.interface ?? "",
  );
  const [vlan, setVlan] = useState<number | string>(
    network?.external?.vlan ?? "",
  );
  const [pool, setPool] = useState(network?.allocationPool ?? "");
  const nodes = useCursorList(
    ["nodes", "external-picker"],
    (page) => api.nodes(page),
    { enabled: external && identity.data?.administrator },
  );
  const selectedNode = nodes.data?.find((item) => item.id === nodeId);
  const interfaces = useQuery({
    queryKey: ["node-interfaces", nodeId],
    queryFn: () => api.nodeInterfaces(nodeId),
    enabled: external && Boolean(selectedNode) && identity.data?.administrator,
  });
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
      id: network?.id ?? randomUUID(),
      name,
      cidr,
      gateway: gateway || undefined,
      external: external
        ? {
            nodeId,
            interface: interfaceName,
            vlan: vlan === "" ? undefined : Number(vlan),
          }
        : undefined,
      allocationPool: external ? pool : undefined,
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
        {identity.data?.administrator && (
          <SegmentedControl
            aria-label="网络接入方式"
            value={external ? "external" : "internal"}
            onChange={(value) => {
              setExternal(value === "external");
              setGateway("");
            }}
            data={[
              { value: "internal", label: "虚拟网络" },
              { value: "external", label: "已有 LAN" },
            ]}
          />
        )}
        {external && (
          <>
            <ErrorMessage error={nodes.error ?? interfaces.error} />
            <Select
              label="接入节点"
              required
              searchable
              value={nodeId}
              onChange={(value) => {
                setNodeId(value ?? "");
                setInterfaceName("");
              }}
              data={(nodes.data ?? []).map((item) => ({
                value: item.id,
                label: item.name,
                disabled: item.state !== "ready",
              }))}
              disabled={!identity.data?.administrator}
            />
            <LoadMore list={nodes} />
            <Select
              label="外部接口"
              required
              value={interfaceName}
              onChange={(value) => setInterfaceName(value ?? "")}
              data={(interfaces.data ?? [])
                .filter((item) => item.available)
                .map((item) => ({
                  value: item.name,
                  label: `${item.name}${item.addresses.length ? ` · ${item.addresses.join(", ")}` : ""}`,
                }))}
              disabled={!identity.data?.administrator}
            />
            <NumberInput
              label="VLAN"
              placeholder="不打标签"
              value={vlan}
              min={1}
              max={4094}
              allowDecimal={false}
              onChange={setVlan}
              disabled={!identity.data?.administrator}
            />
          </>
        )}
        <TextInput
          label="网络地址 / 前缀"
          placeholder="10.1.0.0/24"
          required
          value={cidr}
          onChange={(event) => setCidr(event.currentTarget.value)}
        />
        {external && (
          <>
            <TextInput
              label="Netlab 可分配地址段"
              placeholder="192.168.1.128/26"
              required
              value={pool}
              onChange={(event) => setPool(event.currentTarget.value)}
              disabled={!identity.data?.administrator}
            />
            <TextInput
              label="LAN 网关"
              placeholder="无网关可留空"
              value={gateway}
              onChange={(event) => setGateway(event.currentTarget.value)}
            />
          </>
        )}
        <button
          className="disclosure"
          type="button"
          aria-expanded={advanced}
          onClick={() => setAdvanced(!advanced)}
        >
          网络参数
          <ChevronDown size={16} className={advanced ? "rotated" : ""} />
        </button>
        <Collapse in={advanced}>
          <div className="form-stack">
            {!external && (
              <TextInput
                label="网关"
                placeholder="自动分配"
                value={gateway}
                onChange={(event) => setGateway(event.currentTarget.value)}
              />
            )}
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
