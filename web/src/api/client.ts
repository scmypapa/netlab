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

export class ApiError extends Error {
  constructor(
    public status: number,
    detail: string,
  ) {
    super(detail);
  }
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
  if (response.status === 204) return undefined as T;
  return response.json() as Promise<T>;
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
  templates: (options?: ListOptions) => list<Template>("/templates", options),
  createTemplate: (body: Template) =>
    request<Template>("/templates", "POST", body),
  nodes: (options?: ListOptions) => list<Node>("/nodes", options),
  registerNode: (body: Schema<"NodeRegistration">) =>
    request<Node>("/nodes", "POST", body),
};
