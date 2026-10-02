import { ActionIcon, Button, Menu } from "@mantine/core";
import {
  Box,
  Copy,
  Cpu,
  HardDrive,
  MemoryStick,
  Monitor,
  MoreHorizontal,
  Network,
  Pencil,
  Play,
  Power,
  RotateCcw,
  SquareTerminal,
  Trash2,
  X,
} from "lucide-react";
import type {
  Asset,
  EnvironmentSpec,
  EnvironmentState,
  Network as NetworkModel,
  Schema,
  Template,
} from "../../api/client";
import { memory } from "../../foundation/format";
import { Status } from "../../foundation/Status";
import { networkColors } from "./topology";

export function ObjectInspector({
  asset,
  network,
  spec,
  template,
  state,
  editing,
  busy,
  canOperate,
  canManage,
  canConnect,
  onClose,
  onEdit,
  onRemove,
  onDuplicate,
  onAction,
  onConnect,
  onSelect,
}: {
  asset?: Asset;
  network?: NetworkModel;
  spec: EnvironmentSpec;
  template?: Template;
  state?: EnvironmentState;
  editing: boolean;
  busy: boolean;
  canOperate: boolean;
  canManage: boolean;
  canConnect: boolean;
  onClose: () => void;
  onEdit: () => void;
  onRemove: () => void;
  onDuplicate: () => void;
  onAction: (action: Schema<"ActionRequest">["action"]) => void;
  onConnect: (kind: Schema<"ConsoleKind">) => void;
  onSelect: (id: string) => void;
}) {
  const assetState = state?.assets.find((item) => item.assetId === asset?.id);
  return (
    <aside className="object-inspector" aria-label="对象详情">
      <div className="inspector-title">
        <span>{asset ? "资产" : "网段"}</span>
        <ActionIcon
          variant="subtle"
          color="gray"
          aria-label="关闭对象详情"
          onClick={onClose}
        >
          <X size={17} />
        </ActionIcon>
      </div>
      <div className="object-identity">
        <span
          className={`inspector-symbol ${network ? "network" : template?.kind}`}
        >
          {network ? (
            <Network size={26} />
          ) : template?.kind === "vm" ? (
            <Monitor size={26} />
          ) : (
            <Box size={26} />
          )}
        </span>
        <div>
          <h2>{asset?.name ?? network?.name}</h2>
          {asset && <Status value={assetState?.state ?? "draft"} />}
          {network && <span className="muted mono">{network.cidr}</span>}
        </div>
      </div>
      {(editing || (assetState && (canOperate || canManage || canConnect))) && (
        <div className="inspector-actions">
          {editing ? (
            <Button
              variant="default"
              leftSection={<Pencil size={14} />}
              onClick={onEdit}
            >
              编辑
            </Button>
          ) : (
            asset &&
            (assetState?.state === "stopped" ? canOperate : canConnect) && (
              <Button
                variant="default"
                leftSection={
                  assetState?.state === "stopped" ? (
                    <Play size={14} />
                  ) : template?.kind === "vm" ? (
                    <Monitor size={14} />
                  ) : (
                    <SquareTerminal size={14} />
                  )
                }
                disabled={
                  busy ||
                  (assetState?.state === "suspended" &&
                    template?.kind === "container")
                }
                onClick={() =>
                  assetState?.state === "stopped"
                    ? onAction("start")
                    : onConnect(template?.kind === "vm" ? "vnc" : "terminal")
                }
              >
                {assetState?.state === "stopped"
                  ? "启动"
                  : template?.kind === "vm"
                    ? "控制台"
                    : "终端"}
              </Button>
            )
          )}
          {(editing ||
            (asset &&
              (canOperate ||
                canManage ||
                (canConnect && template?.kind === "vm")))) && (
            <Menu position="bottom-end">
              <Menu.Target>
                <ActionIcon variant="default" aria-label="对象操作">
                  <MoreHorizontal size={18} />
                </ActionIcon>
              </Menu.Target>
              <Menu.Dropdown>
                {editing ? (
                  <>
                    <Menu.Item
                      leftSection={<Pencil size={15} />}
                      onClick={onEdit}
                    >
                      编辑
                    </Menu.Item>
                    {asset && (
                      <Menu.Item
                        leftSection={<Copy size={15} />}
                        onClick={onDuplicate}
                      >
                        复制资产
                      </Menu.Item>
                    )}
                    <Menu.Divider />
                    <Menu.Item
                      color="red"
                      leftSection={<Trash2 size={15} />}
                      onClick={onRemove}
                    >
                      移除{asset ? "资产" : "网段"}
                    </Menu.Item>
                  </>
                ) : (
                  asset && (
                    <>
                      {canConnect && template?.kind === "vm" && (
                        <Menu.Item
                          leftSection={<SquareTerminal size={15} />}
                          disabled={assetState?.state === "stopped"}
                          onClick={() => onConnect("serial")}
                        >
                          串口控制台
                        </Menu.Item>
                      )}
                      {canOperate && (
                        <>
                          <Menu.Item
                            leftSection={<Power size={15} />}
                            disabled={busy}
                            onClick={() =>
                              onAction(
                                assetState?.state === "stopped"
                                  ? "start"
                                  : "stop",
                              )
                            }
                          >
                            {assetState?.state === "stopped"
                              ? "启动"
                              : template?.kind === "vm"
                                ? "关机"
                                : "停止"}
                          </Menu.Item>
                          <Menu.Item
                            leftSection={<RotateCcw size={15} />}
                            disabled={busy}
                            onClick={() => onAction("reboot")}
                          >
                            重启
                          </Menu.Item>
                          <Menu.Item
                            disabled={busy}
                            onClick={() =>
                              onAction(
                                assetState?.state === "suspended"
                                  ? "resume"
                                  : "suspend",
                              )
                            }
                          >
                            {assetState?.state === "suspended"
                              ? "继续运行"
                              : "暂停"}
                          </Menu.Item>
                          <Menu.Divider />
                          <Menu.Item
                            color="red"
                            disabled={busy}
                            onClick={() => onAction("force-stop")}
                          >
                            强制停止
                          </Menu.Item>
                        </>
                      )}
                      {canManage && (
                        <Menu.Item
                          color="red"
                          leftSection={<HardDrive size={15} />}
                          disabled={busy}
                          onClick={() => onAction("rebuild")}
                        >
                          重建资产
                        </Menu.Item>
                      )}
                    </>
                  )
                )}
              </Menu.Dropdown>
            </Menu>
          )}
        </div>
      )}
      {assetState?.error && (
        <div className="inspector-error" role="alert">
          {assetState.error}
        </div>
      )}
      {asset && (
        <>
          <section className="inspector-section">
            <h3>规格</h3>
            <dl className="property-list">
              <div>
                <dt>
                  <Cpu size={14} />
                  CPU
                </dt>
                <dd>{asset.resources.cpu} 核</dd>
              </div>
              <div>
                <dt>
                  <MemoryStick size={14} />
                  内存
                </dt>
                <dd>{memory(asset.resources.memoryMiB)}</dd>
              </div>
              <div>
                <dt>
                  <HardDrive size={14} />
                  磁盘
                </dt>
                <dd>{asset.resources.diskGiB} GiB</dd>
              </div>
            </dl>
            {template && (
              <dl className="property-list template-properties">
                <div>
                  <dt>模板</dt>
                  <dd>{template.name}</dd>
                </div>
                <div>
                  <dt>系统</dt>
                  <dd>{template.os}</dd>
                </div>
                {template.kind === "vm" && (
                  <div>
                    <dt>固件</dt>
                    <dd>{template.hardware?.firmware.toUpperCase()}</dd>
                  </div>
                )}
              </dl>
            )}
          </section>
          <section className="inspector-section">
            <h3>
              网络接口<span>{asset.interfaces.length}</span>
            </h3>
            <div className="interface-list">
              {asset.interfaces.map((item, index) => {
                const connected = spec.networks.find(
                  (network) => network.id === item.networkId,
                );
                const color =
                  networkColors[
                    spec.networks.findIndex(
                      (network) => network.id === item.networkId,
                    ) % networkColors.length
                  ];
                return (
                  <button
                    key={item.id}
                    onClick={() => onSelect(item.networkId)}
                    className="interface-item"
                  >
                    <span
                      className="interface-indicator"
                      style={{ background: color }}
                    />
                    <div>
                      <strong>{connected?.name}</strong>
                      <span className="mono">
                        {item.address || connected?.cidr}
                      </span>
                    </div>
                    <span className="interface-index">
                      eth{index}
                      {item.primary ? " · 主" : ""}
                    </span>
                  </button>
                );
              })}
            </div>
          </section>
        </>
      )}
      {network && (
        <>
          <section className="inspector-section">
            <h3>地址配置</h3>
            <dl className="property-list">
              <div>
                <dt>网络</dt>
                <dd className="mono">{network.cidr}</dd>
              </div>
              <div>
                <dt>网关</dt>
                <dd className="mono">{network.gateway || "自动"}</dd>
              </div>
              <div>
                <dt>DNS</dt>
                <dd>
                  {spec.assets.find((item) => item.id === network.dnsAssetId)
                    ?.name ||
                    network.dnsServers?.join(", ") ||
                    "默认"}
                </dd>
              </div>
              <div>
                <dt>MTU</dt>
                <dd>{network.mtu || 1442}</dd>
              </div>
            </dl>
          </section>
          <section className="inspector-section">
            <h3>连接资产</h3>
            <div className="interface-list">
              {spec.assets
                .filter((item) =>
                  item.interfaces.some(
                    (iface) => iface.networkId === network.id,
                  ),
                )
                .map((member) => (
                  <button
                    className="interface-item"
                    key={member.id}
                    onClick={() => onSelect(member.id)}
                  >
                    <Box size={16} />
                    <div>
                      <strong>{member.name}</strong>
                      <span className="mono">
                        {
                          member.interfaces.find(
                            (item) => item.networkId === network.id,
                          )?.address
                        }
                      </span>
                    </div>
                  </button>
                ))}
            </div>
          </section>
        </>
      )}
    </aside>
  );
}
