import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useState } from "react";
import { api, type EnvironmentSpec, type Schema } from "../../api/client";

export function useWorkbench(id: string) {
  const client = useQueryClient();
  const environment = useQuery({
    queryKey: ["environment", id],
    queryFn: () => api.environment(id),
  });
  const state = useQuery({
    queryKey: ["state", id],
    queryFn: () => api.state(id),
    refetchInterval: (query) =>
      ["queued", "running"].includes(query.state.data?.operation?.state ?? "")
        ? 1500
        : 6000,
  });
  const templates = useQuery({
    queryKey: ["templates"],
    queryFn: api.templates,
    refetchInterval: (query) =>
      query.state.data?.some((template) => template.state === "importing")
        ? 2500
        : false,
  });
  const operations = useQuery({
    queryKey: ["operations", id],
    queryFn: () => api.operations(id),
    refetchInterval:
      state.data?.operation &&
      ["queued", "running"].includes(state.data.operation.state)
        ? 1500
        : false,
  });
  const [editing, setEditing] = useState(false);
  const [spec, setSpec] = useState<EnvironmentSpec>({
    assets: [],
    networks: [],
  });
  const [baseRevision, setBaseRevision] = useState(0);
  const refresh = () => {
    void client.invalidateQueries({ queryKey: ["environment", id] });
    void client.invalidateQueries({ queryKey: ["state", id] });
    void client.invalidateQueries({ queryKey: ["operations", id] });
    void client.invalidateQueries({ queryKey: ["environments"] });
  };
  useEffect(() => {
    if (
      state.data &&
      environment.data &&
      (state.data.revision !== environment.data.revision ||
        state.data.status !== environment.data.status)
    )
      void client.invalidateQueries({ queryKey: ["environment", id] });
  }, [state.data, environment.data, client, id]);
  const beginEdit = () => {
    if (environment.data) {
      setSpec(
        structuredClone(environment.data.draft?.spec ?? environment.data.spec),
      );
      setBaseRevision(
        environment.data.draft?.baseRevision ?? environment.data.revision,
      );
      setEditing(true);
    }
  };
  const saveDraft = useMutation({
    mutationFn: () => api.saveDraft(id, { baseRevision, spec }),
    onSuccess: () => {
      setEditing(false);
      refresh();
    },
  });
  const discard = useMutation({
    mutationFn: () => api.discardDraft(id),
    onSuccess: () => {
      setEditing(false);
      refresh();
    },
  });
  const preview = useMutation({
    mutationFn: () =>
      api.preview(id, { expectedRevision: baseRevision, spec, apply: false }),
  });
  const apply = useMutation({
    mutationFn: () =>
      api.apply(id, {
        expectedRevision: preview.data!.revision,
        spec,
        apply: true,
        clientRequestId: crypto.randomUUID(),
      }),
    onSuccess: () => {
      setEditing(false);
      preview.reset();
      refresh();
    },
  });
  const action = useMutation({
    mutationFn: ({
      action: value,
      assetId,
    }: {
      action: Schema<"ActionRequest">["action"];
      assetId?: string;
    }) => {
      const body = {
        action: value,
        expectedRevision: environment.data!.revision,
        clientRequestId: crypto.randomUUID(),
      };
      return assetId
        ? api.assetAction(id, assetId, body)
        : api.action(id, body);
    },
    onSuccess: refresh,
  });
  const saveView = useMutation({
    scope: { id: `view-${id}` },
    mutationFn: (view: Schema<"CanvasView">) => api.saveView(id, view),
    onMutate: (view) =>
      client.setQueryData(
        ["environment", id],
        (value: typeof environment.data) => value && { ...value, view },
      ),
  });
  return {
    environment,
    state,
    templates,
    operations,
    editing,
    setEditing,
    beginEdit,
    spec,
    setSpec,
    saveDraft,
    discard,
    preview,
    apply,
    action,
    saveView,
  };
}
