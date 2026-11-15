import { useMemo } from "react";
import { ListPagination } from "../../foundation/ListPagination";
import { useLocalPage } from "../../foundation/useLocalPage";
import { Network, Router } from "lucide-react";
import type { Environment } from "../../api/client";
import styles from "./WorkbenchPage.module.css";

export function NetworkList({
  spec,
  query,
  selection,
  onSelect,
}: {
  spec: Environment["spec"];
  query: string;
  selection?: string;
  onSelect: (id: string) => void;
}) {
  const rows = useMemo(() => {
    const names = new Map(
      spec.networks.map((network) => [network.id, network.name]),
    );
    const connected = new Map<string, string[]>();
    for (const object of [...spec.assets, ...(spec.routers ?? [])]) {
      for (const iface of object.interfaces) {
        const items = connected.get(iface.networkId) ?? [];
        items.push(object.name);
        connected.set(iface.networkId, items);
      }
    }
    return [
      ...spec.networks.map((network) => ({
        id: network.id,
        name: network.name,
        kind: network.external ? "外部网段" : "网段",
        address: network.cidr,
        connected: [
          ...(connected.get(network.id) ?? []),
          ...(network.devices ?? []).map((device) => device.name),
        ].join("、"),
        icon: Network,
      })),
      ...(spec.routers ?? []).map((router) => ({
        id: router.id,
        name: router.name,
        kind: "路由器",
        address: router.interfaces
          .map((iface) => iface.address)
          .filter(Boolean)
          .join("、"),
        connected: router.interfaces
          .map((iface) => names.get(iface.networkId))
          .filter(Boolean)
          .join("、"),
        icon: Router,
      })),
    ];
  }, [spec]);
  const filtered = rows.filter((row) =>
    `${row.name} ${row.address} ${row.connected}`
      .toLocaleLowerCase()
      .includes(query.toLocaleLowerCase()),
  );
  const page = useLocalPage(filtered, query);
  return (
    <div className={styles.tableScroll}>
      <table className={styles.assetTable} aria-label="网段与路由器">
        <thead>
          <tr>
            <th>名称</th>
            <th>类型</th>
            <th>地址</th>
            <th>连接对象</th>
          </tr>
        </thead>
        <tbody>
          {page.data.map((row) => (
            <tr
              key={row.id}
              aria-selected={selection === row.id}
              onClick={() => onSelect(row.id)}
            >
              <td>
                <button
                  className={styles.assetName}
                  onClick={() => onSelect(row.id)}
                >
                  <row.icon size={17} />
                  {row.name}
                </button>
              </td>
              <td>{row.kind}</td>
              <td>{row.address || "—"}</td>
              <td>{row.connected || "—"}</td>
            </tr>
          ))}
        </tbody>
      </table>
      <ListPagination list={page} />
      {!filtered.length && (
        <div className={styles.empty}>没有匹配的网段或路由器</div>
      )}
    </div>
  );
}
