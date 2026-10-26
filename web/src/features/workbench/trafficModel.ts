import type { Environment, Schema } from "../../api/client";
import type {
  EndpointKind,
  TrafficSelection,
  TrafficLink,
} from "./TrafficGraph";
import { networkContains as contains } from "./networking";

type Spec = Environment["spec"];
type Conversation = Pick<
  Schema<"CaptureFlow">,
  "source" | "destination" | "sourceAssetId" | "destinationAssetId"
> & { networkId?: string };
const ipv4 = (address: string) =>
  address.split(".").reduce((value, part) => value * 256 + Number(part), 0);
export function describeEndpoint(
  spec: Spec,
  item: Conversation,
  side: "source" | "destination",
) {
  const address = item[side],
    assetId = item[`${side}AssetId`];
  const asset =
    spec.assets.find((asset) => asset.id === assetId) ??
    (item.networkId
      ? spec.assets.find((asset) =>
          asset.interfaces.some(
            (iface) =>
              iface.networkId === item.networkId &&
              (iface.mac === address ||
                iface.address.split("/")[0] === address),
          ),
        )
      : undefined);
  if (asset)
    return {
      id: asset.id,
      name: asset.name,
      kind: "asset" as EndpointKind,
      members: [asset.id],
    };
  const peerId =
    side === "source" ? item.destinationAssetId : item.sourceAssetId;
  const peer =
    !item.networkId && peerId
      ? spec.assets.find((asset) => asset.id === peerId)
      : undefined;
  const peerAddress = side === "source" ? item.destination : item.source;
  const peerInterfaces = peer?.interfaces.filter(
    (iface) =>
      (!item.networkId || iface.networkId === item.networkId) &&
      (iface.address.split("/")[0] === peerAddress ||
        iface.mac === peerAddress),
  );
  const peerInterface =
    peerInterfaces?.length === 1 ? peerInterfaces[0] : undefined;
  const inferredNetwork = () => {
    const containing = spec.networks.filter((network) =>
      contains(network.cidr, address),
    );
    return containing.length === 1 ? containing[0] : undefined;
  };
  const network =
    spec.networks.find((network) => network.id === item.networkId) ??
    spec.networks.find((network) => network.id === peerInterface?.networkId) ??
    inferredNetwork();
  const router = spec.routers?.find((router) =>
    router.interfaces.some(
      (iface) =>
        iface.networkId === network?.id &&
        iface.address.split("/")[0] === address,
    ),
  );
  const gateway = router || network?.gateway === address;
  let kind: EndpointKind = gateway ? "gateway" : "external";
  const mac = /^(?:[\da-f]{2}:){5}[\da-f]{2}$/i.test(address);
  if (address === "ff:ff:ff:ff:ff:ff" || address === "255.255.255.255")
    kind = "broadcast";
  else if (network && contains(network.cidr, address)) {
    const prefix = Number(network.cidr.split("/")[1]);
    if (prefix < 31 && (ipv4(address) + 1) % 2 ** (32 - prefix) === 0)
      kind = "broadcast";
  }
  if (
    /^ff[\da-f]{2}:/i.test(address) ||
    /^2(?:2[4-9]|3\d)\./.test(address) ||
    (mac &&
      (parseInt(address.slice(0, 2), 16) & 1) === 1 &&
      kind !== "broadcast")
  )
    kind = "multicast";
  else if (mac && kind !== "broadcast") kind = "ethernet";
  const grouped = kind === "broadcast" || kind === "multicast";
  return {
    id: grouped
      ? `group:${network?.id ?? "unbound"}:${kind}`
      : `endpoint:${JSON.stringify([network?.id ?? "", address])}`,
    name: grouped
      ? `${network?.name ?? "链路"} · ${kind === "broadcast" ? "广播" : "组播"}`
      : router
        ? router.name
        : gateway
          ? `${network!.name} · 网关`
          : address,
    kind,
    members: [address],
  };
}

function scopedEndpoint(id: string): [string, string] | undefined {
  return id.startsWith("endpoint:")
    ? (JSON.parse(id.slice(9)) as [string, string])
    : undefined;
}

export function selectionMatches(
  spec: Spec,
  selection: TrafficSelection | undefined,
  item: Conversation,
) {
  if (!selection) return true;
  const source = describeEndpoint(spec, item, "source").id,
    destination = describeEndpoint(spec, item, "destination").id;
  return "asset" in selection
    ? [source, destination].includes(selection.asset)
    : (source === selection.source && destination === selection.destination) ||
        (source === selection.destination && destination === selection.source);
}

export function addressFilter(address: string) {
  if (/^(?:[\da-f]{2}:){5}[\da-f]{2}$/i.test(address))
    return `eth.addr == ${address}`;
  return `${address.includes(":") ? "ipv6" : "ip"}.addr == ${address.split("/")[0]}`;
}
export function endpointFilter(spec: Spec, members: string[]) {
  const expressions = members.flatMap((member) => {
    if (member.startsWith("group:")) {
      const [, networkId, kind] = member.split(":"),
        network = spec.networks.find((item) => item.id === networkId);
      if (kind === "multicast")
        return ["(eth.dst.ig == 1 && eth.dst != ff:ff:ff:ff:ff:ff)"];
      const [base, prefix] = network?.cidr.split("/") ?? [];
      const broadcast = base?.includes(".")
        ? (Math.floor(ipv4(base) / 2 ** (32 - Number(prefix))) + 1) *
            2 ** (32 - Number(prefix)) -
          1
        : undefined;
      return [
        `(eth.addr == ff:ff:ff:ff:ff:ff || ip.addr == 255.255.255.255${broadcast !== undefined ? ` || ip.addr == ${[24, 16, 8, 0].map((shift) => Math.floor(broadcast / 2 ** shift) % 256).join(".")}` : ""})`,
      ];
    }
    const asset = spec.assets.find((asset) => asset.id === member);
    return asset
      ? asset.interfaces.flatMap((iface) => [
          addressFilter(iface.address),
          `eth.addr == ${iface.mac}`,
        ])
      : [addressFilter(scopedEndpoint(member)?.[1] ?? member)];
  });
  return `(${[...new Set(expressions)].join(" || ")})`;
}
export function selectionFilter(
  spec: Spec,
  selection: TrafficSelection | undefined,
) {
  if (!selection) return "";
  const managed = (id: string) => spec.assets.some((asset) => asset.id === id);
  return "asset" in selection
    ? managed(selection.asset)
      ? ""
      : endpointFilter(spec, [selection.asset])
    : managed(selection.source)
      ? endpointFilter(spec, [selection.destination])
      : managed(selection.destination)
        ? endpointFilter(spec, [selection.source])
        : `(${endpointFilter(spec, [selection.source])} && ${endpointFilter(spec, [selection.destination])})`;
}
export function selectionAsset(
  spec: Spec,
  selection: TrafficSelection | undefined,
) {
  const ids = selection
    ? "asset" in selection
      ? [selection.asset]
      : [selection.source, selection.destination]
    : [];
  return ids.find((id) => spec.assets.some((asset) => asset.id === id));
}
export function selectionNetwork(
  spec: Spec,
  selection: TrafficSelection | undefined,
) {
  const ids = selection
    ? "asset" in selection
      ? [selection.asset]
      : [selection.source, selection.destination]
    : [];
  const network = ids.find((id) => id.startsWith("group:"))?.split(":")[1];
  if (network && network !== "unbound") return network;
  const endpointNetworks = [
    ...new Set(ids.flatMap((id) => scopedEndpoint(id)?.[0] || [])),
  ];
  if (endpointNetworks.length === 1) return endpointNetworks[0];
  const assets = ids.flatMap(
    (id) => spec.assets.find((asset) => asset.id === id) ?? [],
  );
  const shared =
    assets[0]?.interfaces
      .map((iface) => iface.networkId)
      .filter((id) =>
        assets.every((asset) =>
          asset.interfaces.some((iface) => iface.networkId === id),
        ),
      ) ?? [];
  const unique = [...new Set(shared)];
  return unique.length === 1 ? unique[0] : undefined;
}

export function packetAlerts(
  packet: Schema<"CapturePacket">,
  alerts: Schema<"SecurityAlert">[],
) {
  const timestamp = Date.parse(packet.time);
  return alerts.filter((alert) => {
    const forward =
      packet.source === alert.source &&
      packet.destination === alert.destination &&
      packet.sourcePort === alert.sourcePort &&
      packet.destinationPort === alert.destinationPort;
    const reverse =
      packet.source === alert.destination &&
      packet.destination === alert.source &&
      packet.sourcePort === alert.destinationPort &&
      packet.destinationPort === alert.sourcePort;
    return (
      (!packet.networkId ||
        !alert.networkId ||
        packet.networkId === alert.networkId) &&
      (forward || reverse) &&
      (packet.transportProtocol ?? packet.protocol)
        .toLowerCase()
        .replace(/icmpv[46]|ipv6-icmp/g, "icmp") ===
        alert.protocol.toLowerCase().replace(/icmpv[46]|ipv6-icmp/g, "icmp") &&
      timestamp >= Date.parse(alert.firstSeen) - 1000 &&
      timestamp <= Date.parse(alert.lastSeen) + 1000
    );
  });
}

const pairKey = (source: string, target: string) =>
  JSON.stringify([source, target].sort());
export function buildTrafficLinks(
  spec: Spec,
  flows: Schema<"CaptureFlow">[],
  alerts: Schema<"SecurityAlert">[],
  packets?: Schema<"CapturePacket">[],
  riskOnly = false,
): TrafficLink[] {
  const detected = new Map<
    string,
    {
      source: ReturnType<typeof describeEndpoint>;
      target: ReturnType<typeof describeEndpoint>;
      severity: number;
      count: number;
    }
  >();
  for (const alert of alerts) {
    const source = describeEndpoint(spec, alert, "source"),
      target = describeEndpoint(spec, alert, "destination"),
      key = pairKey(source.id, target.id),
      previous = detected.get(key);
    if (previous) {
      previous.severity = Math.min(previous.severity, alert.severity);
      previous.count += alert.count;
    } else
      detected.set(key, {
        source,
        target,
        severity: alert.severity,
        count: alert.count,
      });
  }
  const make = (item: Conversation, value: number, rate?: number) => {
    const source = describeEndpoint(spec, item, "source"),
      target = describeEndpoint(spec, item, "destination");
    return {
      source: source.id,
      target: target.id,
      sourceName: source.name,
      targetName: target.name,
      sourceKind: source.kind,
      targetKind: target.kind,
      value,
      rate,
      severity: detected.get(pairKey(source.id, target.id))?.severity,
    };
  };
  const links: TrafficLink[] = packets
    ? packets.map((packet) =>
        make(
          {
            ...packet,
          },
          packet.length,
        ),
      )
    : flows.map((flow) => make(flow, flow.bytes, flow.bytesPerSecond));
  for (const { source, target, severity, count } of detected.values())
    links.push({
      source: source.id,
      target: target.id,
      sourceName: source.name,
      targetName: target.name,
      sourceKind: source.kind,
      targetKind: target.kind,
      value: 0,
      alerts: count,
      severity,
    });
  return riskOnly ? links.filter((link) => link.severity !== undefined) : links;
}
export function selectionLabel(
  spec: Spec,
  selection: TrafficSelection | undefined,
  links: TrafficLink[],
) {
  const name = (id: string) =>
    spec.assets.find((asset) => asset.id === id)?.name ??
    links.find((link) => link.source === id)?.sourceName ??
    links.find((link) => link.target === id)?.targetName ??
    id;
  return selection
    ? "asset" in selection
      ? name(selection.asset)
      : `${name(selection.source)} ↔ ${name(selection.destination)}`
    : undefined;
}
export function alertFilter(alert: Schema<"SecurityAlert">) {
  const protocol = alert.protocol.toLowerCase(),
    portProtocol =
      protocol === "tcp" || protocol === "udp" ? protocol : undefined;
  return [
    addressFilter(alert.source),
    addressFilter(alert.destination),
    portProtocol
      ? `${portProtocol}.port == ${alert.sourcePort} && ${portProtocol}.port == ${alert.destinationPort}`
      : protocol === "icmpv4"
        ? "icmp"
        : protocol === "icmpv6"
          ? "icmpv6"
          : protocol,
  ].join(" && ");
}
