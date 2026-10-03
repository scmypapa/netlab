import { stateLabel } from "./format";

export function Status({ value }: { value: string }) {
  const tone = ["running", "ready", "succeeded", "online"].includes(value)
    ? "positive"
    : ["failed", "offline", "partially_applied"].includes(value)
      ? "negative"
      : [
            "deploying",
            "changing",
            "destroying",
            "queued",
            "importing",
            "deleting",
          ].includes(value)
        ? "active"
        : "neutral";
  return (
    <span className={`status status-${tone}`}>
      <span className="status-dot" />
      {stateLabel(value)}
    </span>
  );
}
