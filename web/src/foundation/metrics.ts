import type { Schema } from "../api/client";

export type History = Schema<"MetricHistory">;
export type Metric = Schema<"MetricSeries">["metric"];

export function latestMetric(
  history: History | undefined,
  metric: Metric,
  nodeId?: string,
): number | undefined {
  const points = history?.series
    .filter(
      (series) =>
        series.metric === metric &&
        (nodeId === undefined || series.nodeId === nodeId),
    )
    .map((series) => series.points.at(-1))
    .filter(
      (point) =>
        point && Date.parse(history!.end) - Date.parse(point.time) <= 20_000,
    );
  if (!points?.length) return undefined;
  const time = Math.max(...points.map((point) => Date.parse(point!.time)));
  return points.reduce(
    (total, point) =>
      total + (Date.parse(point!.time) === time ? point!.value : 0),
    0,
  );
}
