import { ActionIcon, Button, Select, SegmentedControl } from "@mantine/core";
import { useQueries, useQuery } from "@tanstack/react-query";
import { ArrowLeft, ChevronDown, Network } from "lucide-react";
import { useState } from "react";
import { api, type Schema } from "../../api/client";
import type { Flow, Segment } from "./useTrafficObservation";
import styles from "./PacketWorkspace.module.css";

type Field = Schema<"PacketField">;

function ProtocolField({
  field,
  onSelect,
  top = false,
}: {
  field: Field;
  onSelect: (field: Field) => void;
  top?: boolean;
}) {
  if (!field.children.length)
    return (
      <button
        className={styles.field}
        onClick={() => onSelect(field)}
        title={field.name}
      >
        {field.label}
      </button>
    );
  return (
    <details className={styles.protocol} open={top}>
      <summary onClick={() => onSelect(field)}>{field.label}</summary>
      <div>
        {field.children.map((child, index) => (
          <ProtocolField
            key={child.name + index}
            field={child}
            onSelect={onSelect}
          />
        ))}
      </div>
    </details>
  );
}

export default function PacketWorkspace({
  environmentId,
  captureId,
  segments,
  flow,
  onClose,
}: {
  environmentId: string;
  captureId: string;
  segments: Segment[];
  flow: Flow;
  onClose: () => void;
}) {
  const [node, setNode] = useState<string | null>(null);
  const [packetNumber, setPacketNumber] = useState<number>();
  const [field, setField] = useState<Field>();
  const [pane, setPane] = useState("protocol");
  const [cursors, setCursors] = useState<(number | undefined)[]>([undefined]);
  const chosen =
    segments.find((item) => item.nodeId === node) ??
    segments.find((item) => item.nodeId === flow.nodeId) ??
    segments.find((item) => item.assetIds.includes(flow.sourceAssetId ?? "")) ??
    segments.find((item) =>
      item.assetIds.includes(flow.destinationAssetId ?? ""),
    ) ??
    segments[0];
  const live = chosen?.status === "running" || chosen?.status === "starting";
  const filters = {
    source: flow.source,
    destination: flow.destination,
    sourcePort: flow.sourcePort,
    destinationPort: flow.destinationPort,
    interfaceIndex:
      chosen?.nodeId === flow.nodeId ? flow.interfaceIndex : undefined,
    protocol: ["TCP", "UDP", "ICMPv4", "ICMPv6", "ARP"].includes(flow.protocol)
      ? flow.protocol
      : undefined,
  };
  const pages = useQueries({
    queries: cursors.map((after, index) => ({
      queryKey: [
        "capture-packets",
        environmentId,
        captureId,
        chosen?.nodeId,
        filters,
        after,
      ],
      queryFn: ({ signal }: { signal: AbortSignal }) =>
        api.capturePackets(
          environmentId,
          chosen!.nodeId,
          captureId,
          { ...filters, after },
          signal,
        ),
      enabled: !!chosen,
      refetchInterval:
        live && index === cursors.length - 1 ? 2000 : (false as const),
      staleTime: Infinity,
    })),
  });
  const packets = pages[pages.length - 1];
  const nextCursor = packets.data?.nextCursor;
  const rows = pages.flatMap((page) => page.data?.packets ?? []);
  const selected = packetNumber ?? rows[0]?.number;
  const detail = useQuery({
    queryKey: [
      "capture-packet",
      environmentId,
      captureId,
      chosen?.nodeId,
      selected,
    ],
    queryFn: ({ signal }) =>
      api.capturePacket(
        environmentId,
        chosen!.nodeId,
        captureId,
        selected!,
        signal,
      ),
    enabled: !!chosen && selected !== undefined,
    staleTime: Infinity,
  });
  const bytes = detail.data?.hex.match(/.{2}/g) ?? [];
  const highlighted = (index: number) =>
    !!field &&
    field.length > 0 &&
    index >= field.offset &&
    index < field.offset + field.length;
  const errors = [...pages.map((page) => page.error), detail.error].filter(
    Boolean,
  );
  return (
    <section className={styles.workspace} aria-label="连接报文">
      <header className={styles.heading}>
        <ActionIcon aria-label="返回通信图" variant="subtle" onClick={onClose}>
          <ArrowLeft size={17} />
        </ActionIcon>
        <strong>报文分析</strong>
        <span>{flow.appProtocol || flow.protocol}</span>
        {segments.length > 1 && (
          <Select
            aria-label="报文节点"
            size="xs"
            w={160}
            value={chosen?.nodeId}
            data={segments.map((item) => ({
              value: item.nodeId,
              label: item.nodeName ?? item.nodeId,
            }))}
            onChange={(value) => {
              setNode(value);
              setPacketNumber(undefined);
              setField(undefined);
              setCursors([undefined]);
            }}
          />
        )}
        <span className={styles.count}>
          {rows.length} 帧{live && <i title="持续读取" />}
        </span>
      </header>
      {errors.length > 0 && (
        <div role="alert" className={styles.error}>
          {errors.map((error) => (
            <div key={error!.message}>{error!.message}</div>
          ))}
        </div>
      )}
      <div className={styles.packetList}>
        <table aria-label="报文列表">
          <thead>
            <tr>
              <th>帧</th>
              <th>时间</th>
              <th>来源</th>
              <th>目标</th>
              <th>协议</th>
              <th>字节</th>
              <th>内容摘要</th>
            </tr>
          </thead>
          <tbody>
            {rows.map((packet) => (
              <tr
                key={packet.number}
                data-selected={selected === packet.number}
                onClick={() => {
                  setPacketNumber(packet.number);
                  setField(undefined);
                }}
              >
                <td>
                  <button aria-label={`查看第 ${packet.number} 帧`}>
                    {packet.number}
                  </button>
                </td>
                <td>
                  {new Date(packet.time).toLocaleTimeString("zh-CN", {
                    hour12: false,
                  })}
                  .
                  {new Date(packet.time)
                    .getMilliseconds()
                    .toString()
                    .padStart(3, "0")}
                </td>
                <td>
                  {packet.source}
                  {packet.sourcePort ? `:${packet.sourcePort}` : ""}
                </td>
                <td>
                  {packet.destination}
                  {packet.destinationPort ? `:${packet.destinationPort}` : ""}
                </td>
                <td>{packet.protocol}</td>
                <td>{packet.length}</td>
                <td title={packet.summary}>{packet.summary}</td>
              </tr>
            ))}
          </tbody>
        </table>
        {!rows.length && (
          <div className={styles.empty}>
            <Network size={25} />
            {packets.isLoading
              ? "读取报文…"
              : live
                ? "等待匹配报文"
                : "没有匹配报文"}
          </div>
        )}
        {nextCursor !== undefined && (
          <Button
            variant="subtle"
            size="xs"
            rightSection={<ChevronDown size={13} />}
            loading={packets.isFetching}
            onClick={() => setCursors((current) => [...current, nextCursor])}
          >
            更多报文
          </Button>
        )}
      </div>
      <div className={styles.paneTabs}>
        <SegmentedControl
          aria-label="报文内容"
          size="xs"
          data={[
            { value: "protocol", label: "协议层" },
            { value: "bytes", label: "字节" },
          ]}
          value={pane}
          onChange={setPane}
        />
      </div>
      <div className={styles.analysis} data-pane={pane}>
        <div className={styles.protocols} aria-label="报文协议层">
          {detail.isLoading && <div className={styles.empty}>解析报文…</div>}
          {detail.data?.layers.map((layer, index) => (
            <ProtocolField
              key={layer.name + index}
              field={layer}
              onSelect={setField}
              top
            />
          ))}
        </div>
        <div className={styles.hex} aria-label="报文字节">
          {Array.from({ length: Math.ceil(bytes.length / 16) }, (_, row) => (
            <div className={styles.hexRow} key={row}>
              <span>{(row * 16).toString(16).padStart(4, "0")}</span>
              <code>
                {bytes.slice(row * 16, row * 16 + 16).map((byte, column) => (
                  <span
                    key={column}
                    data-selected={highlighted(row * 16 + column)}
                  >
                    {byte}{" "}
                  </span>
                ))}
              </code>
              <code className={styles.ascii}>
                {bytes.slice(row * 16, row * 16 + 16).map((byte, column) => {
                  const value = parseInt(byte, 16);
                  return (
                    <span
                      key={column}
                      data-selected={highlighted(row * 16 + column)}
                    >
                      {value >= 32 && value < 127
                        ? String.fromCharCode(value)
                        : "."}
                    </span>
                  );
                })}
              </code>
            </div>
          ))}
        </div>
      </div>
    </section>
  );
}
