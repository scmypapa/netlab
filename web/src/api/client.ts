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
  return new Promise((resolve, reject) => {
    const xhr = new XMLHttpRequest();
    const abort = () => xhr.abort();
    xhr.open("POST", "/api/v1/templates");
    xhr.upload.onprogress = (event) => {
      if (event.lengthComputable)
        progress(Math.round((event.loaded / event.total) * 100));
    };
    xhr.onloadend = () => signal.removeEventListener("abort", abort);
    xhr.onerror = () => reject(new Error("镜像上传连接中断"));
    xhr.onabort = () => reject(new DOMException("上传已取消", "AbortError"));
    xhr.onload = async () => {
      try {
        const response = new Response(xhr.responseText, { status: xhr.status });
        await checkResponse(response);
        resolve((await response.json()) as Template);
      } catch (error) {
        reject(error);
      }
    };
    signal.addEventListener("abort", abort, { once: true });
    if (signal.aborted) reject(new DOMException("上传已取消", "AbortError"));
    else xhr.send(form);
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
  createTemplate: (body: Schema<"TemplateImport">) =>
    request<Template>("/templates", "POST", body),
  nodes: (options?: ListOptions) => list<Node>("/nodes", options),
  nodeInterfaces: (id: string) =>
    request<Schema<"ExternalInterface">[]>(`/nodes/${id}/interfaces`),
  registerNode: (body: Schema<"NodeRegistration">) =>
    request<Node>("/nodes", "POST", body),
};
