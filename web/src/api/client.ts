import type { components } from "./types.gen";

export type Schema<K extends keyof components["schemas"]> =
  components["schemas"][K];
export type Environment = Schema<"Environment">;
export type EnvironmentSummary = Schema<"EnvironmentSummary">;
export type EnvironmentSpec = Schema<"EnvironmentSpec">;
export type Asset = Schema<"Asset">;
export type Network = Schema<"Network">;
export type Template = Schema<"Template">;
export type Blueprint = Schema<"Blueprint">;
export type BlueprintVersion = Schema<"BlueprintVersion">;
export type Node = Schema<"Node">;
export type Operation = Schema<"Operation">;
export type EnvironmentState = Schema<"EnvironmentState">;
export type Identity = Schema<"Identity">;
export type Principal = Schema<"Principal">;
export type Permission = Schema<"Permission">;
export type RolePreset = Schema<"RolePreset">;
export type EnvironmentGrant = Schema<"EnvironmentGrant">;

export class ApiError extends Error {
  constructor(
    public status: number,
    detail: string,
  ) {
    super(detail);
  }
}

async function uploadTemplate(
  body: Schema<"TemplateImport">,
  files: File[],
  progress: (percent: number) => void,
  signal: AbortSignal,
): Promise<Template> {
  const form = new FormData();
  form.append(
    "template",
    new Blob([JSON.stringify(body)], { type: "application/json" }),
  );
  for (const file of files)
    form.append("files", file, file.webkitRelativePath || file.name);
  return uploadRequest<Template>("/templates", "POST", form, progress, signal);
}

function uploadRequest<T>(
  path: string,
  method: string,
  body: XMLHttpRequestBodyInit,
  progress: (percent: number) => void,
  signal: AbortSignal,
): Promise<T> {
  return new Promise((resolve, reject) => {
    const xhr = new XMLHttpRequest();
    const abort = () => xhr.abort();
    xhr.open(method, "/api/v1" + path);
    if (body instanceof File)
      xhr.setRequestHeader("Content-Type", "application/octet-stream");
    xhr.upload.onprogress = (event) => {
      if (event.lengthComputable)
        progress(Math.round((event.loaded / event.total) * 100));
    };
    xhr.onloadend = () => signal.removeEventListener("abort", abort);
    xhr.onerror = () => reject(new Error("上传连接中断"));
    xhr.onabort = () => reject(new DOMException("上传已取消", "AbortError"));
    xhr.onload = async () => {
      try {
        const response = new Response(
          xhr.status === 204 ? null : xhr.responseText,
          { status: xhr.status },
        );
        await checkResponse(response);
        resolve(
          response.status === 204
            ? (undefined as T)
            : ((await response.json()) as T),
        );
      } catch (error) {
        reject(error);
      }
    };
    signal.addEventListener("abort", abort, { once: true });
    if (signal.aborted) reject(new DOMException("上传已取消", "AbortError"));
    else xhr.send(body);
  });
}

async function request<T>(
  path: string,
  method = "GET",
  body?: unknown,
  signal?: AbortSignal,
): Promise<T> {
  const response = await fetch(`/api/v1${path}`, {
    method,
    credentials: "same-origin",
    signal,
    headers:
      body === undefined ? undefined : { "Content-Type": "application/json" },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  await checkResponse(response);
  if (response.status === 204) return undefined as T;
  return response.json() as Promise<T>;
}

async function checkResponse(response: Response) {
  if (!response.ok) {
    const problem = (await response
      .json()
      .catch(() => ({ detail: response.statusText }))) as {
      detail?: string;
      title?: string;
    };
    throw new ApiError(
      response.status,
      problem.detail || problem.title || response.statusText,
    );
  }
}

async function assetLogs(
  id: string,
  assetId: string,
  options: { stream: string; tail: number; follow: boolean },
  signal: AbortSignal,
  receive: (chunk: Schema<"LogChunk">) => void,
  connected: () => void,
) {
  const params = new URLSearchParams({
    stream: options.stream,
    tail: String(options.tail),
    follow: String(options.follow),
  });
  const response = await fetch(
    `/api/v1/environments/${encodeURIComponent(id)}/assets/${encodeURIComponent(assetId)}/logs?${params}`,
    { credentials: "same-origin", signal },
  );
  await checkResponse(response);
  connected();
  const reader = response.body!.getReader();
  const decoder = new TextDecoder();
  let pending = "";
  try {
    for (;;) {
      const { done, value } = await reader.read();
      if (done) break;
      pending += decoder.decode(value, { stream: true });
      let boundary: number;
      while ((boundary = pending.indexOf("\n\n")) >= 0) {
        const event = pending.slice(0, boundary);
        pending = pending.slice(boundary + 2);
        const data = event
          .split("\n")
          .find((line) => line.startsWith("data: "));
        if (!data) continue;
        if (event.startsWith("event: stream-error"))
          throw new Error(
            (JSON.parse(data.slice(6)) as Schema<"Problem">).detail,
          );
        if (event.startsWith("event: output"))
          receive(JSON.parse(data.slice(6)) as Schema<"LogChunk">);
      }
    }
  } finally {
    reader.releaseLock();
  }
}

export type ListOptions = {
  cursor?: string;
  limit?: number;
  search?: string;
  status?: string;
  kind?: string;
  ids?: string[];
  signal?: AbortSignal;
};

function list<T>(
  path: string,
  options: ListOptions = {},
  extra?: Record<string, string>,
) {
  const { signal, ...filters } = options;
  const params = new URLSearchParams(extra);
  for (const [name, value] of Object.entries(filters)) {
    if (value !== undefined && String(value) !== "")
      params.set(name, String(value));
  }
  return request<T[]>(`${path}?${params}`, "GET", undefined, signal);
}

export const api = {
  nodeStorageDevices: (id: string) =>
    request<Schema<"NodeStorageDevices">>(`/nodes/${id}/storage-device`),
  configureNodeStorage: (id: string, device: string) =>
    request<Operation>(`/nodes/${id}/storage-device`, "PUT", { device }),
  cephStatus: (id: string) =>
    request<Schema<"CephStatus">>(`/storage-pools/${id}/ceph`),
  configureCephPool: (id: string, replicas: number) =>
    request<Operation>(`/storage-pools/${id}/ceph`, "PUT", { replicas }),
  storagePoolAssets: (id: string) =>
    request<Schema<"StoragePoolAsset">[]>(`/storage-pools/${id}/assets`),
  volumes: () => request<Schema<"PersistentVolume">[]>("/volumes"),
  createVolume: (body: Schema<"CreateVolume">) =>
    request<Operation>("/volumes", "POST", body),
  resizeVolume: (id: string, sizeGiB: number) =>
    request<Operation>(`/volumes/${id}`, "PUT", { sizeGiB }),
  deleteVolume: (id: string) => request<Operation>(`/volumes/${id}`, "DELETE"),
  traffic: (id: string, signal?: AbortSignal) =>
    request<Schema<"TrafficObservation">>(
      `/environments/${id}/traffic`,
      "GET",
      undefined,
      signal,
    ),
  captures: (id: string, signal?: AbortSignal) =>
    request<Schema<"CaptureList">>(
      "/environments/" + id + "/captures",
      "GET",
      undefined,
      signal,
    ),
  startCapture: (id: string, body: Schema<"CreateCapture">) =>
    request<Schema<"CaptureList">>(
      "/environments/" + id + "/captures",
      "POST",
      body,
    ),
  captureDetail: (
    id: string,
    node: string,
    capture: string,
    signal?: AbortSignal,
  ) =>
    request<Schema<"CaptureDetail">>(
      "/environments/" + id + "/captures/" + node + "/" + capture,
      "GET",
      undefined,
      signal,
    ),
  stopCapture: (id: string, node: string, capture: string) =>
    request<Schema<"CaptureSegment">>(
      "/environments/" + id + "/captures/" + node + "/" + capture,
      "POST",
    ),
  deleteCapture: (id: string, node: string, capture: string) =>
    request<void>(
      "/environments/" + id + "/captures/" + node + "/" + capture,
      "DELETE",
    ),
  captureDownload: (id: string, node: string, capture: string) =>
    "/api/v1/environments/" +
    id +
    "/captures/" +
    node +
    "/" +
    capture +
    "/file",
  updateStatus: () => request<Schema<"SystemUpdate">>("/system/update"),
  checkUpdate: () =>
    request<Schema<"SystemUpdate">>("/system/update/check", "POST"),
  applyUpdate: (version: string) =>
    request<Schema<"SystemUpdate">>("/system/update", "POST", { version }),
  metrics: (id: string, assetId: string, range: number, signal?: AbortSignal) =>
    request<Schema<"MetricHistory">>(
      "/environments/" +
        id +
        "/metrics?" +
        new URLSearchParams({ assetId, range: String(range) }),
      "GET",
      undefined,
      signal,
    ),
  rdpSettings: (id: string, assetId: string) =>
    request<Schema<"RDPSettings">>(`/environments/${id}/assets/${assetId}/rdp`),
  saveRDPSettings: (
    id: string,
    assetId: string,
    settings: Schema<"RDPSettings">,
  ) =>
    request<void>(`/environments/${id}/assets/${assetId}/rdp`, "PUT", settings),
  rdpCertificate: (id: string, assetId: string, probe: Schema<"SSHProbe">) =>
    request<{ fingerprint: string }>(
      `/environments/${id}/assets/${assetId}/rdp/certificate`,
      "POST",
      probe,
    ),
  sshSettings: (id: string, assetId: string) =>
    request<Schema<"SSHSettings">>(`/environments/${id}/assets/${assetId}/ssh`),
  saveSSHSettings: (
    id: string,
    assetId: string,
    settings: Schema<"SSHSettings">,
  ) =>
    request<void>(`/environments/${id}/assets/${assetId}/ssh`, "PUT", settings),
  sshHostKey: (id: string, assetId: string, probe: Schema<"SSHProbe">) =>
    request<{ fingerprint: string }>(
      `/environments/${id}/assets/${assetId}/ssh/host-key`,
      "POST",
      probe,
    ),
  files: (id: string, assetId: string, path: string, signal?: AbortSignal) =>
    request<Schema<"FileEntry">[]>(
      `/environments/${id}/assets/${assetId}/files?path=${encodeURIComponent(path)}`,
      "GET",
      undefined,
      signal,
    ),
  fileCommand: (
    id: string,
    assetId: string,
    path: string,
    command: Schema<"FileCommand">,
  ) =>
    request<void>(
      `/environments/${id}/assets/${assetId}/files?path=${encodeURIComponent(path)}`,
      "POST",
      command,
    ),
  uploadFile: (
    id: string,
    assetId: string,
    path: string,
    file: File,
    progress: (value: number) => void,
    signal: AbortSignal,
  ) =>
    uploadRequest<void>(
      `/environments/${id}/assets/${assetId}/files/content?path=${encodeURIComponent(path)}`,
      "PUT",
      file,
      progress,
      signal,
    ),
  fileURL: (id: string, assetId: string, path: string) =>
    `/api/v1/environments/${id}/assets/${assetId}/files/content?path=${encodeURIComponent(path)}`,
  backupRepositories: () =>
    request<Schema<"BackupRepository">[]>("/backup-repositories"),
  createBackupRepository: (body: Schema<"CreateBackupRepository">) =>
    request<Schema<"BackupRepository">>("/backup-repositories", "POST", body),
  deleteBackupRepository: (id: string) =>
    request<void>(`/backup-repositories/${id}`, "DELETE"),
  refreshBackupRepository: (id: string) =>
    request<Operation>(`/backup-repositories/${id}/refresh`, "POST"),
  repositoryBackups: (id: string, options?: ListOptions) =>
    list<Schema<"BackupSummary">>(
      `/backup-repositories/${id}/backups`,
      options,
    ),
  deleteRepositoryBackup: (id: string, backupId: string) =>
    request<Operation>(
      `/backup-repositories/${id}/backups/${backupId}`,
      "DELETE",
    ),
  backupRepositoryCredentials: (id: string) =>
    request<Schema<"BackupCredentials">>(
      `/backup-repositories/${id}/credentials`,
    ),
  backups: (id: string, options?: ListOptions) =>
    list<Schema<"BackupSummary">>(`/environments/${id}/backups`, options),
  createBackup: (id: string, body: Schema<"CreateBackup">) =>
    request<Schema<"BackupSummary">>(
      `/environments/${id}/backups`,
      "POST",
      body,
    ),
  deleteBackup: (id: string, backupId: string) =>
    request<Operation>(`/environments/${id}/backups/${backupId}`, "DELETE"),
  restoreBackup: (id: string, backupId: string, expectedRevision: number) =>
    request<Operation>(
      `/environments/${id}/backups/${backupId}/restore`,
      "POST",
      { expectedRevision },
    ),
  captureTemplate: (
    id: string,
    assetId: string,
    body: Schema<"CaptureTemplate">,
  ) =>
    request<Template>(
      `/environments/${id}/assets/${assetId}/templates`,
      "POST",
      body,
    ),
  uploadTemplate,
  assetLogs,
  principals: (options?: ListOptions) =>
    list<Principal>("/principals", options),
  createUser: (body: Schema<"CreateUser">) =>
    request<Principal>("/principals", "POST", body),
  updateUser: (id: string, body: Schema<"UpdateUser">) =>
    request<void>(`/principals/${id}`, "PUT", body),
  createToken: (body: Schema<"CreateServiceToken">) =>
    request<Schema<"IssuedServiceToken">>("/service-tokens", "POST", body),
  revokeToken: (id: string) => request<void>(`/service-tokens/${id}`, "DELETE"),
  sharing: (id: string) =>
    request<Schema<"EnvironmentSharing">>(`/environments/${id}/grants`),
  replaceSharing: (id: string, body: EnvironmentGrant[]) =>
    request<void>(`/environments/${id}/grants`, "PUT", body),
  identity: () => request<Identity>("/identity"),
  login: (name: string, password: string) =>
    request<Identity>("/sessions/login", "POST", { name, password }),
  logout: () => request<void>("/sessions/logout", "POST"),
  environments: (options?: ListOptions) =>
    list<EnvironmentSummary>("/environments", options),
  environment: (id: string) => request<Environment>(`/environments/${id}`),
  createEnvironment: (body: Schema<"CreateEnvironment">) =>
    request<Environment>("/environments", "POST", body),
  blueprints: (options?: ListOptions) =>
    list<Blueprint>("/blueprints", options),
  deleteBlueprint: (id: string) => request<void>(`/blueprints/${id}`, "DELETE"),
  blueprintVersions: (id: string, options?: ListOptions) =>
    list<Schema<"BlueprintVersionSummary">>(
      `/blueprints/${id}/versions`,
      options,
    ),
  blueprintVersion: (id: string) =>
    request<BlueprintVersion>(`/blueprint-versions/${id}`),
  saveBlueprint: (id: string, body: Schema<"SaveBlueprint">) =>
    request<Blueprint>(`/environments/${id}/blueprints`, "POST", body),
  saveBlueprintVersion: (id: string, body: Schema<"SaveBlueprintVersion">) =>
    request<BlueprintVersion>(`/blueprints/${id}/versions`, "POST", body),
  state: (id: string) => request<EnvironmentState>(`/environments/${id}/state`),
  recoveryPoints: (id: string, options?: ListOptions) =>
    list<Schema<"RecoveryPointSummary">>(
      `/environments/${id}/recovery-points`,
      options,
    ),
  captureRecoveryPoint: (id: string, body: Schema<"CaptureRecoveryPoint">) =>
    request<Schema<"RecoveryPointSummary">>(
      `/environments/${id}/recovery-points`,
      "POST",
      body,
    ),
  deleteRecoveryPoint: (id: string, pointId: string) =>
    request<Operation>(
      `/environments/${id}/recovery-points/${pointId}`,
      "DELETE",
    ),
  restoreRecoveryPoint: (
    id: string,
    pointId: string,
    expectedRevision: number,
  ) =>
    request<Operation>(
      `/environments/${id}/recovery-points/${pointId}/restore`,
      "POST",
      { expectedRevision },
    ),
  vpnAccess: (id: string) =>
    request<Schema<"VPNAccess">[]>(`/environments/${id}/vpn-access`),
  createVPNAccess: (id: string, body: Schema<"CreateVPNAccess">) =>
    request<Operation>(`/environments/${id}/vpn-access`, "POST", body),
  revokeVPNAccess: (
    id: string,
    accessId: string,
    expectedRevision: number,
    clientRequestId: string,
  ) =>
    request<Operation>(
      `/environments/${id}/vpn-access/${accessId}?${new URLSearchParams({ expectedRevision: String(expectedRevision), clientRequestId })}`,
      "DELETE",
    ),
  vpnConnection: (id: string, accessId: string) =>
    request<Schema<"VPNConnection">>(
      `/environments/${id}/vpn-access/${accessId}/connection`,
    ),
  services: (id: string) =>
    request<Schema<"ServiceEndpoint">[]>(`/environments/${id}/services`),
  exposeService: (id: string, assetId: string, body: Schema<"CreateService">) =>
    request<Operation>(
      `/environments/${id}/assets/${assetId}/services`,
      "POST",
      body,
    ),
  revokeService: (
    id: string,
    serviceId: string,
    expectedRevision: number,
    clientRequestId: string,
  ) =>
    request<Operation>(
      `/environments/${id}/services/${serviceId}?${new URLSearchParams({ expectedRevision: String(expectedRevision), clientRequestId })}`,
      "DELETE",
    ),
  events: (id: string) => new EventSource(`/api/v1/environments/${id}/events`),
  action: (id: string, body: Schema<"ActionRequest">) =>
    request<Operation>(`/environments/${id}/actions`, "POST", body),
  assetAction: (id: string, assetId: string, body: Schema<"ActionRequest">) =>
    request<Operation>(
      `/environments/${id}/assets/${assetId}/actions`,
      "POST",
      body,
    ),
  migrationDestinations: (id: string, assetId: string) =>
    request<Schema<"MigrationDestination">[]>(
      `/environments/${id}/assets/${assetId}/migrations`,
    ),
  migrateAsset: (
    id: string,
    assetId: string,
    body: Schema<"MigrationRequest">,
  ) =>
    request<Operation>(
      `/environments/${id}/assets/${assetId}/migrations`,
      "POST",
      body,
    ),
  saveDraft: (id: string, body: Schema<"Draft">) =>
    request<void>(`/environments/${id}/draft`, "PUT", body),
  discardDraft: (id: string) =>
    request<void>(`/environments/${id}/draft`, "DELETE"),
  saveView: (id: string, body: Schema<"CanvasView">) =>
    request<void>(`/environments/${id}/view`, "PUT", body),
  preview: (id: string, body: Schema<"ChangeRequest">) =>
    request<Schema<"ChangePreview">>(
      `/environments/${id}/changes`,
      "POST",
      body,
    ),
  apply: (id: string, body: Schema<"ChangeRequest">) =>
    request<Operation>(`/environments/${id}/changes`, "POST", body),
  operations: (environmentId: string, options?: ListOptions) =>
    list<Operation>("/operations", options, { environmentId }),
  retryOperation: (id: string) =>
    request<Operation>(`/operations/${id}/retry`, "POST"),
  templates: (options?: ListOptions) => list<Template>("/templates", options),
  deleteTemplate: (id: string) =>
    request<Operation>(`/templates/${id}`, "DELETE"),
  createTemplate: (body: Schema<"TemplateImport">) =>
    request<Template>("/templates", "POST", body),
  nodes: (options?: ListOptions) => list<Node>("/nodes", options),
  storagePools: () => request<Schema<"StoragePool">[]>("/storage-pools"),
  createStoragePool: (input: Schema<"CreateStoragePool">) =>
    request<Schema<"StoragePool">>("/storage-pools", "POST", input),
  deleteStoragePool: (id: string) =>
    request<Operation>(`/storage-pools/${id}`, "DELETE"),
  nodeInterfaces: (id: string) =>
    request<Schema<"ExternalInterface">[]>(`/nodes/${id}/interfaces`),
  registerNode: (body: Schema<"NodeRegistration">) =>
    request<Node>("/nodes", "POST", body),
};
