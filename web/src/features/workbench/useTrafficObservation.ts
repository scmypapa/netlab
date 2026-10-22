import {
  useInfiniteQuery,
  useMutation,
  useQueries,
  useQuery,
  useQueryClient,
} from "@tanstack/react-query";
import { api, type Environment, type Schema } from "../../api/client";

export type Flow = Schema<"CaptureFlow">;
export type Alert = Schema<"SecurityAlert">;
export type Segment = Schema<"CaptureSegment">;
export const activeCapture = (segment: Segment) =>
  ["starting", "running"].includes(segment.status);
export const endpoint = (flow: Flow | Alert, side: "source" | "destination") =>
  flow[side === "source" ? "sourceAssetId" : "destinationAssetId"] ||
  flow[side];

export function useTrafficObservation(
  environment: Environment,
  captureId: string,
  setCaptureId: (id: string) => void,
  filters: {
    range: number;
    level?: string;
    assetId?: string;
    search?: string;
    protocol?: string;
  },
) {
  const id = environment.id;
  const client = useQueryClient();
  const live = captureId === "live";
  const captures = useQuery({
    queryKey: ["captures", id],
    queryFn: ({ signal }) => api.captures(id, signal),
    refetchInterval: (query) =>
      query.state.data?.segments.some(activeCapture) ? 3000 : false,
  });
  const traffic = useQuery({
    queryKey: ["traffic", id],
    queryFn: ({ signal }) => api.traffic(id, signal),
    enabled: live,
    refetchInterval: 5000,
  });
  const history = useQuery({
    queryKey: ["traffic-history", id, filters.range, filters.assetId],
    queryFn: ({ signal }) =>
      api.environmentMetrics(id, filters.range, signal, filters.assetId),
    enabled: live,
    refetchInterval: 10_000,
  });
  const current = useQuery({
    queryKey: ["traffic-current", id, filters.assetId],
    queryFn: ({ signal }) =>
      api.environmentMetrics(id, 60, signal, filters.assetId),
    enabled: live,
    refetchInterval: 10_000,
  });
  const security = useInfiniteQuery({
    queryKey: ["security", id, filters],
    initialPageParam: undefined as number | undefined,
    queryFn: ({ signal, pageParam }) =>
      api.security(id, { ...filters, before: pageParam }, signal),
    getNextPageParam: (last) => last.nextCursor,
    enabled: live,
    refetchInterval: 5000,
  });
  const segments =
    captures.data?.segments.filter((segment) => segment.id === captureId) ?? [];
  const details = useQueries({
    queries: segments.map((segment) => ({
      queryKey: ["capture", id, segment.nodeId, segment.id],
      queryFn: ({ signal }: { signal: AbortSignal }) =>
        api.captureDetail(id, segment.nodeId, segment.id, signal),
      enabled: !live && (segment.status !== "failed" || segment.bytes > 0),
      refetchInterval: activeCapture(segment) ? 2000 : false,
    })),
  });
  const refresh = () => {
    void client.invalidateQueries({ queryKey: ["captures", id] });
    void client.invalidateQueries({ queryKey: ["capture", id] });
  };
  const start = useMutation({
    mutationFn: (body: Schema<"CreateCapture">) => api.startCapture(id, body),
    onSuccess: (result) => {
      if (result.segments.length) setCaptureId(result.segments[0].id);
      refresh();
    },
  });
  const action = useMutation({
    mutationFn: async ({
      segments,
      remove,
    }: {
      segments: Segment[];
      remove: boolean;
    }) => {
      const results = await Promise.allSettled(
        segments.map((segment) =>
          remove
            ? api.deleteCapture(id, segment.nodeId, segment.id)
            : api.stopCapture(id, segment.nodeId, segment.id),
        ),
      );
      const failures = results.flatMap((item) =>
        item.status === "rejected" ? [String(item.reason)] : [],
      );
      if (failures.length) throw new Error(failures.join("；"));
    },
    onSuccess: (_, input) => {
      if (
        input.remove &&
        segments.length &&
        segments.every((segment) =>
          input.segments.some(
            (removed) =>
              removed.id === segment.id && removed.nodeId === segment.nodeId,
          ),
        )
      )
        setCaptureId("live");
    },
    onSettled: refresh,
  });
  const flows = [
    ...(live
      ? traffic.isError
        ? []
        : (traffic.data?.flows ?? [])
      : details.flatMap((item) => item.data?.flows ?? [])),
  ].sort((a, b) => b.bytes - a.bytes);
  const alerts = security.isError
    ? []
    : [
        ...new Map(
          [...(security.data?.pages ?? [])]
            .reverse()
            .flatMap((page) => page.alerts)
            .map((alert) => [alert.id, alert]),
        ).values(),
      ].sort((a, b) => Date.parse(b.lastSeen) - Date.parse(a.lastSeen));
  const errors = [
    captures,
    ...(live ? [traffic, history, current, security] : details),
    action,
    start,
  ].flatMap((item) => (item.error ? [item.error.message] : []));
  return {
    live,
    captures,
    traffic,
    history,
    current,
    security,
    segments,
    details,
    flows,
    alerts,
    errors: [...new Set(errors)],
    start,
    action,
    summary: security.data?.pages[0],
  };
}
