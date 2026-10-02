import type { components } from "./types.gen";

export type Schema<K extends keyof components["schemas"]> =
  components["schemas"][K];
export type Environment = Schema<"Environment">;
export type EnvironmentSpec = Schema<"EnvironmentSpec">;
export type Asset = Schema<"Asset">;
export type Network = Schema<"Network">;
export type Template = Schema<"Template">;
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
): Promise<T> {
  const response = await fetch(`/api/v1${path}`, {
    method,
    credentials: "same-origin",
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

export const api = {
  identity: () => request<Identity>("/identity"),
  login: (name: string, password: string) =>
    request<Identity>("/sessions/login", "POST", { name, password }),
  logout: () => request<void>("/sessions/logout", "POST"),
  environments: () => request<Environment[]>("/environments"),
  environment: (id: string) => request<Environment>(`/environments/${id}`),
  createEnvironment: (body: Schema<"CreateEnvironment">) =>
    request<Environment>("/environments", "POST", body),
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
  operations: (environmentId: string) =>
    request<Operation[]>(
      `/operations?environmentId=${encodeURIComponent(environmentId)}`,
    ),
  templates: () => request<Template[]>("/templates"),
  createTemplate: (body: Template) =>
    request<Template>("/templates", "POST", body),
  nodes: () => request<Node[]>("/nodes"),
  registerNode: (body: Schema<"NodeRegistration">) =>
    request<Node>("/nodes", "POST", body),
};
