import { ActionIcon, Button, Menu, Modal, TextInput } from "@mantine/core";
import {
  ArrowLeft,
  Box,
  ChevronDown,
  Columns3,
  GitBranch,
  LayoutGrid,
  List,
  MoreHorizontal,
  Monitor,
  Network,
  Pencil,
  Play,
  Plus,
  Search,
  Save,
  Trash2,
  UsersRound,
  KeyRound,
} from "lucide-react";
import { lazy, Suspense, useMemo, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { Link, useParams } from "react-router-dom";
import {
  api,
  type Asset,
  type Network as NetworkModel,
  type Schema,
} from "../../api/client";
import { CaptureTemplateDialog } from "../templates/CaptureTemplateDialog";
import { Empty, ErrorMessage, Loading } from "../../foundation/Feedback";
import { Status } from "../../foundation/Status";
import { SaveBlueprintDialog } from "../templates/SaveBlueprintDialog";
import { SharingDrawer } from "../access/SharingDrawer";
import { AssetEditor, NetworkEditor } from "./ObjectEditors";
import { ObjectInspector } from "./ObjectInspector";
import { LogDrawer } from "./LogDrawer";
import { ServiceDrawer } from "./ServiceDrawer";
import { TaskTray } from "./TaskTray";
import { TopologyCanvas } from "./TopologyCanvas";
import { connectAsset, networkColors, topology } from "./topology";
import { useWorkbench } from "./useWorkbench";
import { useVPNAccess } from "./useVPNAccess";
import { VPNDrawer } from "./VPNDrawer";
import { RecoveryDrawer } from "./RecoveryDrawer";
import { consoleKey, type ConsoleTab } from "./consoles";
import { allows, allowsProject } from "../access/permissions";

const ConsoleWorkspace = lazy(() => import("./ConsoleWorkspace"));

export function WorkbenchPage() {
  const { id = "" } = useParams();
  const workbench = useWorkbench(id);
  const identity = useQuery({ queryKey: ["identity"], queryFn: api.identity });
  const [capturing, setCapturing] = useState<Asset>();
  const [selection, setSelection] = useState<string>();
  const [tree, setTree] = useState(false);
  const [list, setList] = useState(false);
  const [query, setQuery] = useState("");
  const [editor, setEditor] = useState<"asset" | "network">();
  const [editingObject, setEditingObject] = useState(false);
  const [destroying, setDestroying] = useState(false);
  const [recoveryOpened, setRecoveryOpened] = useState(false);
  const [rebuilding, setRebuilding] = useState<Asset>();
  const [savingBlueprint, setSavingBlueprint] = useState(false);
  const [connections, setConnections] = useState<ConsoleTab[]>([]);
  const [selectedConnection, setSelectedConnection] = useState("");
  const [sharing, setSharing] = useState(false);
  const [logging, setLogging] = useState<Asset>();
  const [serving, setServing] = useState<string>();
  const [vpnOpened, setVPNOpened] = useState(false);
  const [context, setContext] = useState<{
    x: number;
    y: number;
    id: string;
  }>();
  const environment = workbench.environment.data;
  const vpn = useVPNAccess(
    id,
    vpnOpened,
    environment,
    workbench.operations.data ?? [],
    workbench.state.data?.operation,
    workbench.refresh,
  );
  const canCompose = allows(environment, "compose");
  const canOperate = allows(environment, "operate");
  const canManage = allows(environment, "manage");
  const canRead = allows(environment, "read");
  const templates = workbench.templates.data ?? [];
  const spec = workbench.editing
    ? workbench.spec
    : (environment?.appliedSpec ??
      environment?.spec ?? { assets: [], networks: [] });
  const templatesById = useMemo(
    () => new Map(templates.map((template) => [template.id, template])),
    [templates],
  );
  const assetStates = useMemo(
    () =>
      new Map(
        (workbench.state.data?.assets ?? []).map((asset) => [
          asset.assetId,
          asset.state,
        ]),
      ),
    [workbench.state.data?.assets],
  );
  const layout = useMemo(
    () => topology(spec, templates, environment?.view ?? {}),
    [spec, templates, environment?.view],
  );
  const graph = useMemo(
    () => ({
      ...layout,
      nodes: layout.nodes.map((node) => ({
        ...node,
        data: { ...node.data, state: assetStates.get(node.id) ?? "draft" },
      })),
    }),
    [layout, assetStates],
  );
  const asset = spec.assets.find((item) => item.id === selection);
  const servedAsset = spec.assets.find((item) => item.id === serving);
  const connect = (kind: Schema<"ConsoleKind">) => {
    if (!asset) return;
    const connection = { id: asset.id, name: asset.name, kind };
    const key = consoleKey(connection);
    setConnections((items) =>
      items.some((item) => consoleKey(item) === key)
        ? items
        : [...items, connection],
    );
    setSelectedConnection(key);
  };
  const closeConnection = (key: string) => {
    const remaining = connections.filter((item) => consoleKey(item) !== key);
    setConnections(remaining);
    if (selectedConnection === key)
      setSelectedConnection(
        remaining.length ? consoleKey(remaining[remaining.length - 1]) : "",
      );
  };
  const network = spec.networks.find((item) => item.id === selection);
  const status =
    workbench.state.data?.status ?? environment?.status ?? "unknown";
  const busy = ["deploying", "changing", "destroying"].includes(status);
  const activeAction = ["running", "suspended"].includes(status)
    ? status === "suspended"
      ? "resume"
      : "stop"
    : "start";
  const operation = workbench.state.data?.operation;
  const adding = (kind: "asset" | "network") => {
    if (!workbench.editing) workbench.beginEdit();
    setEditingObject(false);
    setEditor(kind);
  };
  const editObject = () => {
    setEditingObject(true);
    setEditor(asset ? "asset" : "network");
  };
  const updateAsset = (value: Asset) => {
    workbench.setSpec((current) => ({
      ...current,
      assets: current.assets.some((item) => item.id === value.id)
        ? current.assets.map((item) => (item.id === value.id ? value : item))
        : [...current.assets, value],
    }));
    setSelection(value.id);
    setEditor(undefined);
  };
  const updateNetwork = (value: NetworkModel) => {
    workbench.setSpec((current) => ({
      ...current,
      networks: current.networks.some((item) => item.id === value.id)
        ? current.networks.map((item) => (item.id === value.id ? value : item))
        : [...current.networks, value],
    }));
    setSelection(value.id);
    setEditor(undefined);
  };
  const remove = () => {
    workbench.setSpec((current) => ({
      ...current,
      assets: current.assets
        .filter((item) => item.id !== selection)
        .map((item) => ({
          ...item,
          interfaces: item.interfaces.filter(
            (iface) => iface.networkId !== selection,
          ),
        })),
      networks: current.networks.filter((item) => item.id !== selection),
      routes: current.routes?.filter((item) => item.networkId !== selection),
      policies: current.policies?.filter(
        (item) => item.networkId !== selection,
      ),
      services: current.services?.filter((service) =>
        current.assets.some(
          (item) =>
            item.id === service.assetId &&
            item.id !== selection &&
            item.interfaces.some(
              (iface) =>
                iface.id === service.interfaceId &&
                iface.networkId !== selection,
            ),
        ),
      ),
    }));
    setSelection(undefined);
  };
  const duplicate = () => {
    if (asset)
      updateAsset({
        ...structuredClone(asset),
        id: crypto.randomUUID(),
        name: `${asset.name}-副本`,
        interfaces: asset.interfaces.map((item) => ({
          ...item,
          id: crypto.randomUUID(),
          address: "",
          mac: "",
        })),
        volumes: asset.volumes?.map((volume) => ({
          ...volume,
          id: crypto.randomUUID(),
        })),
      });
  };
  const errors = [
    workbench.environment.error,
    workbench.templates.error,
    workbench.state.error,
    workbench.action.error,
    workbench.retry.error,
    workbench.saveDraft.error,
    workbench.discard.error,
    workbench.saveView.error,
  ];
  if (workbench.environment.isPending) return <Loading />;
  if (!environment)
    return (
      <main className="collection-page">
        <ErrorMessage error={workbench.environment.error} />
      </main>
    );
  return (
    <main className={`workbench ${workbench.editing ? "editing" : ""}`}>
      <header className="environment-header">
        <div className="environment-heading">
          <ActionIcon
            component={Link}
            to="/environments"
            variant="subtle"
            color="gray"
            aria-label="返回环境列表"
          >
            <ArrowLeft size={18} />
          </ActionIcon>
          <div className="environment-heading-text">
            <h1>{environment.name}</h1>
            <Status value={status} />
          </div>
          {workbench.editing && <span className="edit-indicator">调整中</span>}
        </div>
        <div className="environment-actions">
          {environment.permissions?.includes("access") && (
            <ActionIcon
              variant="default"
              aria-label="VPN"
              onClick={() => setVPNOpened(true)}
            >
              <KeyRound size={18} />
            </ActionIcon>
          )}
          {environment.permissions?.includes("manage") && (
            <ActionIcon
              variant="default"
              aria-label="共享环境"
              onClick={() => setSharing(true)}
            >
              <UsersRound size={18} />
            </ActionIcon>
          )}
          {workbench.editing ? (
            <>
              <Button
                variant="default"
                onClick={() => workbench.saveDraft.mutate()}
                loading={workbench.saveDraft.isPending}
              >
                保存草稿
              </Button>
              <Button
                leftSection={<GitBranch size={15} />}
                onClick={() => workbench.preview.mutate()}
                loading={workbench.preview.isPending}
              >
                预览变更
              </Button>
              <Menu>
                <Menu.Target>
                  <ActionIcon variant="default" aria-label="草稿操作">
                    <MoreHorizontal size={18} />
                  </ActionIcon>
                </Menu.Target>
                <Menu.Dropdown>
                  <Menu.Item
                    leftSection={<Save size={15} />}
                    onClick={() => setSavingBlueprint(true)}
                  >
                    保存为环境模板
                  </Menu.Item>
                  <Menu.Item onClick={() => workbench.saveDraft.mutate()}>
                    返回现场
                  </Menu.Item>
                  <Menu.Item
                    color="red"
                    leftSection={<Trash2 size={15} />}
                    onClick={() => workbench.discard.mutate()}
                  >
                    放弃草稿
                  </Menu.Item>
                </Menu.Dropdown>
              </Menu>
            </>
          ) : (
            <>
              {canCompose && (
                <Button
                  variant="default"
                  leftSection={<Pencil size={15} />}
                  onClick={workbench.beginEdit}
                  disabled={busy || status === "destroyed"}
                >
                  {environment.draft ? "继续调整" : "调整环境"}
                </Button>
              )}
              {canOperate && (
                <Button
                  leftSection={
                    activeAction === "start" ? <Play size={15} /> : undefined
                  }
                  loading={workbench.action.isPending}
                  disabled={busy || status === "destroyed"}
                  onClick={() =>
                    workbench.action.mutate({ action: activeAction })
                  }
                >
                  {activeAction === "start"
                    ? "运行"
                    : activeAction === "resume"
                      ? "继续运行"
                      : "停止"}
                </Button>
              )}
              {(canRead || canCompose || canOperate || canManage) && (
                <Menu position="bottom-end">
                  <Menu.Target>
                    <ActionIcon variant="default" aria-label="环境操作">
                      <MoreHorizontal size={18} />
                    </ActionIcon>
                  </Menu.Target>
                  <Menu.Dropdown>
                    {canRead && (
                      <Menu.Item onClick={() => setRecoveryOpened(true)}>
                        恢复点
                      </Menu.Item>
                    )}
                    {canCompose && (
                      <Menu.Item
                        leftSection={<Save size={15} />}
                        onClick={() => setSavingBlueprint(true)}
                      >
                        保存为环境模板
                      </Menu.Item>
                    )}
                    {canOperate && (
                      <>
                        <Menu.Divider />
                        {status === "suspended" && (
                          <Menu.Item
                            onClick={() =>
                              workbench.action.mutate({ action: "stop" })
                            }
                          >
                            停止环境
                          </Menu.Item>
                        )}
                        <Menu.Item
                          disabled={busy || status !== "running"}
                          onClick={() =>
                            workbench.action.mutate({ action: "suspend" })
                          }
                        >
                          暂停环境
                        </Menu.Item>
                        <Menu.Item
                          disabled={busy || status !== "running"}
                          onClick={() =>
                            workbench.action.mutate({ action: "reboot" })
                          }
                        >
                          重启环境
                        </Menu.Item>
                      </>
                    )}
                    {canManage && (
                      <>
                        <Menu.Divider />
                        <Menu.Item
                          color="red"
                          disabled={busy || status === "destroyed"}
                          leftSection={<Trash2 size={15} />}
                          onClick={() => setDestroying(true)}
                        >
                          销毁环境
                        </Menu.Item>
                      </>
                    )}
                  </Menu.Dropdown>
                </Menu>
              )}
            </>
          )}
        </div>
      </header>
      {errors.map((error, index) => (
        <ErrorMessage key={index} error={error} />
      ))}
      {environment.error && (
        <div className="workbench-error" role="alert">
          {environment.error}
        </div>
      )}
      <div className="workspace-body">
        {tree && (
          <aside className="object-tree">
            <TextInput
              aria-label="查找对象"
              placeholder="查找对象"
              leftSection={<Search size={15} />}
              value={query}
              onChange={(event) => setQuery(event.currentTarget.value)}
            />
            <div className="tree-section-heading">
              资产<span>{spec.assets.length}</span>
            </div>
            {spec.assets
              .filter((item) =>
                item.name
                  .toLocaleLowerCase()
                  .includes(query.toLocaleLowerCase()),
              )
              .map((item) => (
                <button
                  className={`tree-item ${selection === item.id ? "selected" : ""}`}
                  key={item.id}
                  onClick={() => setSelection(item.id)}
                >
                  {templatesById.get(item.templateId)?.kind === "vm" ? (
                    <Monitor size={16} />
                  ) : (
                    <Box size={16} />
                  )}
                  <span>{item.name}</span>
                  <span className={`tree-state ${assetStates.get(item.id)}`} />
                </button>
              ))}
            <div className="tree-section-heading">
              网段<span>{spec.networks.length}</span>
            </div>
            {spec.networks
              .filter((item) =>
                item.name
                  .toLocaleLowerCase()
                  .includes(query.toLocaleLowerCase()),
              )
              .map((item, index) => (
                <button
                  className={`tree-item ${selection === item.id ? "selected" : ""}`}
                  key={item.id}
                  onClick={() => setSelection(item.id)}
                >
                  <Network
                    size={16}
                    color={networkColors[index % networkColors.length]}
                  />
                  <span>{item.name}</span>
                </button>
              ))}
          </aside>
        )}
        <section className="canvas-area" aria-label="环境拓扑">
          <div className="canvas-toolbar">
            <div className="canvas-view-controls">
              <ActionIcon
                variant={tree ? "light" : "subtle"}
                color={tree ? "teal" : "gray"}
                aria-label="对象列表"
                aria-pressed={tree}
                onClick={() => setTree(!tree)}
              >
                <Columns3 size={17} />
              </ActionIcon>
              <div className="view-switch">
                <button
                  className={!list ? "selected" : ""}
                  aria-label="拓扑视图"
                  aria-pressed={!list}
                  onClick={() => setList(false)}
                >
                  <LayoutGrid size={15} />
                  <span>拓扑</span>
                </button>
                <button
                  className={list ? "selected" : ""}
                  aria-label="资产视图"
                  aria-pressed={list}
                  onClick={() => setList(true)}
                >
                  <List size={15} />
                  <span>资产</span>
                </button>
              </div>
            </div>
            <div className="canvas-add-controls">
              {workbench.editing && (
                <Menu position="bottom-end">
                  <Menu.Target>
                    <Button
                      variant="default"
                      size="xs"
                      leftSection={<Plus size={14} />}
                      rightSection={<ChevronDown size={13} />}
                    >
                      添加
                    </Button>
                  </Menu.Target>
                  <Menu.Dropdown>
                    <Menu.Item
                      leftSection={<Box size={16} />}
                      onClick={() => adding("asset")}
                    >
                      资产
                    </Menu.Item>
                    <Menu.Item
                      leftSection={<Network size={16} />}
                      onClick={() => adding("network")}
                    >
                      网段
                    </Menu.Item>
                  </Menu.Dropdown>
                </Menu>
              )}
              <span className="canvas-count">
                {spec.assets.length} 资产 · {spec.networks.length} 网段
              </span>
            </div>
          </div>
          {!spec.assets.length && !spec.networks.length ? (
            <Empty
              icon={<Network size={36} />}
              title="从一个网段开始"
              action="添加网段"
              onAction={() => adding("network")}
            />
          ) : list ? (
            <div className="asset-list-area">
              <table className="data-table asset-table">
                <thead>
                  <tr>
                    <th>资产</th>
                    <th>状态</th>
                    <th>连接网段</th>
                    <th>地址</th>
                  </tr>
                </thead>
                <tbody>
                  {spec.assets.map((item) => (
                    <tr
                      key={item.id}
                      className={selection === item.id ? "selected" : ""}
                    >
                      <td>
                        <button
                          className="asset-row-name"
                          onClick={() => setSelection(item.id)}
                        >
                          {templatesById.get(item.templateId)?.kind === "vm" ? (
                            <Monitor size={17} />
                          ) : (
                            <Box size={17} />
                          )}
                          {item.name}
                        </button>
                      </td>
                      <td>
                        <Status value={assetStates.get(item.id) ?? "draft"} />
                      </td>
                      <td>
                        {item.interfaces
                          .map(
                            (iface) =>
                              spec.networks.find(
                                (network) => network.id === iface.networkId,
                              )?.name,
                          )
                          .join("、")}
                      </td>
                      <td className="mono">
                        {item.interfaces
                          .map((iface) => iface.address)
                          .filter(Boolean)
                          .join("、")}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
              {workbench.editing && (
                <Button
                  variant="subtle"
                  leftSection={<Plus size={15} />}
                  onClick={() => adding("asset")}
                >
                  添加资产
                </Button>
              )}
            </div>
          ) : (
            <TopologyCanvas
              nodes={graph.nodes}
              edges={graph.edges}
              selection={selection}
              onSelect={setSelection}
              editing={workbench.editing}
              onPosition={(positions) => {
                if (canCompose)
                  workbench.saveView.mutate({ ...environment.view, positions });
              }}
              onConnect={({ source, target }) => {
                const assetId = spec.assets.some((item) => item.id === source)
                  ? source
                  : target;
                const networkId = spec.networks.some(
                  (item) => item.id === source,
                )
                  ? source
                  : target;
                if (assetId && networkId && assetId !== networkId)
                  workbench.setSpec((value) =>
                    connectAsset(value, assetId, networkId),
                  );
              }}
              onContext={(objectId, x, y) => {
                setSelection(objectId);
                setContext({ id: objectId, x, y });
              }}
            />
          )}
        </section>
        {(asset || network) && (
          <ObjectInspector
            asset={asset}
            network={network}
            spec={spec}
            template={asset && templatesById.get(asset.templateId)}
            state={workbench.state.data}
            editing={workbench.editing}
            busy={busy}
            canOperate={allows(environment, "operate", asset?.id)}
            canManage={allows(environment, "manage", asset?.id)}
            canCapture={Boolean(identity.data?.administrator)}
            onCapture={() => setCapturing(asset)}
            canConnect={allows(environment, "session", asset?.id)}
            canObserve={allows(environment, "observe", asset?.id)}
            canAccess={allows(environment, "access", asset?.id)}
            services={workbench.services.data ?? []}
            onClose={() => setSelection(undefined)}
            onEdit={editObject}
            onRemove={remove}
            onDuplicate={duplicate}
            onAction={(action) =>
              action === "rebuild"
                ? setRebuilding(asset)
                : workbench.action.mutate({ action, assetId: asset?.id })
            }
            onConnect={connect}
            onLogs={() => setLogging(asset)}
            onServices={() => {
              workbench.exposeService.reset();
              workbench.revokeService.reset();
              setServing(asset?.id);
            }}
            onSelect={setSelection}
          />
        )}
      </div>
      {connections.length > 0 && (
        <Suspense fallback={<Loading />}>
          <ConsoleWorkspace
            environmentId={id}
            tabs={connections}
            selected={selectedConnection}
            onSelect={setSelectedConnection}
            onClose={closeConnection}
          />
        </Suspense>
      )}
      {capturing && (
        <CaptureTemplateDialog
          environmentId={id}
          asset={capturing}
          revision={environment.revision}
          defaultInitialization={
            templatesById.get(capturing.templateId)?.initialization
          }
          onClose={() => setCapturing(undefined)}
          onCreated={workbench.refresh}
        />
      )}
      <TaskTray
        operations={workbench.operations.data ?? []}
        operation={operation}
        pagination={workbench.operations}
        spec={spec}
        retrying={workbench.retry.isPending}
        onRetry={(operationId) => workbench.retry.mutate(operationId)}
      />
      {sharing && (
        <SharingDrawer
          environment={environment}
          onClose={() => setSharing(false)}
        />
      )}
      {vpnOpened && environment.permissions?.includes("access") && (
        <VPNDrawer
          vpn={vpn}
          networks={environment.appliedSpec?.networks ?? []}
          busy={
            busy ||
            status === "destroyed" ||
            operation?.state === "queued" ||
            operation?.state === "running"
          }
          onClose={() => setVPNOpened(false)}
        />
      )}
      {logging && (
        <LogDrawer
          environmentId={id}
          asset={logging}
          onClose={() => setLogging(undefined)}
        />
      )}
      {servedAsset && (
        <ServiceDrawer
          key={servedAsset.id}
          asset={servedAsset}
          spec={spec}
          services={workbench.services.data ?? []}
          loading={workbench.services.isLoading}
          pending={
            busy ||
            workbench.exposeService.isPending ||
            workbench.revokeService.isPending
          }
          canManage={allows(environment, "access", servedAsset.id)}
          error={
            workbench.services.error ??
            workbench.exposeService.error ??
            workbench.revokeService.error
          }
          onCreate={(body, onAccepted) =>
            workbench.exposeService.mutate(
              { assetId: servedAsset.id, ...body },
              { onSuccess: onAccepted },
            )
          }
          onRevoke={(serviceId) => workbench.revokeService.mutate(serviceId)}
          onClose={() => setServing(undefined)}
        />
      )}
      {editor === "asset" && (
        <AssetEditor
          asset={editingObject ? asset : undefined}
          templates={templates}
          pagination={workbench.templates}
          search={workbench.templateSearch}
          onSearch={workbench.setTemplateSearch}
          spec={spec}
          onSave={updateAsset}
          onClose={() => setEditor(undefined)}
        />
      )}
      {editor === "network" && (
        <NetworkEditor
          network={editingObject ? network : undefined}
          spec={spec}
          onSave={updateNetwork}
          onClose={() => setEditor(undefined)}
        />
      )}
      {savingBlueprint && (
        <SaveBlueprintDialog
          environmentId={environment.id}
          environmentName={environment.name}
          expectedRevision={environment.revision}
          spec={spec}
          onClose={() => setSavingBlueprint(false)}
        />
      )}
      <Modal
        opened={
          Boolean(workbench.preview.data) || Boolean(workbench.preview.error)
        }
        onClose={() => workbench.preview.reset()}
        title="应用环境变更"
        centered
        size="md"
      >
        <ErrorMessage error={workbench.preview.error} />
        <ErrorMessage error={workbench.apply.error} />
        {workbench.preview.data && (
          <>
            <div className="change-summary">
              {workbench.preview.data.changes.length ? (
                workbench.preview.data.changes.map((item) => (
                  <div key={item.id} className="change-row">
                    <span className={`change-effect ${item.effect}`}>
                      {
                        {
                          add: "新增",
                          update: "修改",
                          replace: "替换",
                          remove: "移除",
                        }[item.effect]
                      }
                    </span>
                    <strong>{item.name}</strong>
                    {item.requiresStop && (
                      <span className="impact-tag">需要停机</span>
                    )}
                    {item.dataEffect && <p>{item.dataEffect}</p>}
                  </div>
                ))
              ) : (
                <div className="task-empty">没有执行配置变更</div>
              )}
            </div>
            <div className="dialog-actions">
              <Button
                variant="default"
                onClick={() => workbench.preview.reset()}
              >
                返回调整
              </Button>
              <Button
                onClick={() => workbench.apply.mutate()}
                loading={workbench.apply.isPending}
                disabled={!workbench.preview.data.changes.length}
              >
                应用变更
              </Button>
            </div>
          </>
        )}
      </Modal>
      <Modal
        opened={Boolean(rebuilding)}
        onClose={() => setRebuilding(undefined)}
        title={`重建 ${rebuilding?.name ?? ""}`}
        centered
        size="sm"
      >
        <p>系统盘上的改动将清除，恢复为模板初始内容。</p>
        <div className="dialog-actions">
          <Button variant="default" onClick={() => setRebuilding(undefined)}>
            取消
          </Button>
          <Button
            color="red"
            loading={workbench.action.isPending}
            disabled={busy}
            onClick={() => {
              workbench.action.mutate({
                action: "rebuild",
                assetId: rebuilding!.id,
              });
              setRebuilding(undefined);
            }}
          >
            重建资产
          </Button>
        </div>
      </Modal>
      <Modal
        opened={destroying}
        onClose={() => setDestroying(false)}
        title={`销毁 ${environment.name}`}
        centered
        size="sm"
      >
        <div className="destroy-summary">
          <strong>{spec.assets.length} 个资产</strong>
          <span>{spec.networks.length} 个网段</span>
        </div>
        <p>运行资产及其系统盘将删除。</p>
        <div className="dialog-actions">
          <Button variant="default" onClick={() => setDestroying(false)}>
            取消
          </Button>
          <Button
            color="red"
            onClick={() => {
              setDestroying(false);
              workbench.action.mutate({ action: "destroy" });
            }}
          >
            销毁环境
          </Button>
        </div>
      </Modal>
      {recoveryOpened && (
        <RecoveryDrawer
          id={id}
          projectId={environment.projectId}
          revision={environment.revision}
          busy={busy}
          canManage={canManage}
          canClone={
            canManage &&
            allowsProject(identity.data, "compose", environment.projectId)
          }
          canCapture={["running", "stopped", "suspended"].includes(status)}
          onClose={() => setRecoveryOpened(false)}
        />
      )}
      <Menu
        opened={Boolean(context)}
        onClose={() => setContext(undefined)}
        position="bottom-start"
      >
        <Menu.Target>
          <span
            className="context-anchor"
            style={{ left: context?.x, top: context?.y }}
          />
        </Menu.Target>
        <Menu.Dropdown>
          {workbench.editing ? (
            <>
              <Menu.Item
                leftSection={<Pencil size={15} />}
                onClick={editObject}
              >
                编辑
              </Menu.Item>
              {asset && <Menu.Item onClick={duplicate}>复制资产</Menu.Item>}
              <Menu.Item
                color="red"
                leftSection={<Trash2 size={15} />}
                onClick={remove}
              >
                移除
              </Menu.Item>
            </>
          ) : (
            canCompose && (
              <Menu.Item
                leftSection={<Pencil size={15} />}
                disabled={busy}
                onClick={workbench.beginEdit}
              >
                调整环境
              </Menu.Item>
            )
          )}
        </Menu.Dropdown>
      </Menu>
    </main>
  );
}
