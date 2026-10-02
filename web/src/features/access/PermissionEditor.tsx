import { Checkbox, Collapse, Select } from "@mantine/core";
import { ChevronDown } from "lucide-react";
import { useState } from "react";
import type { Permission, RolePreset } from "../../api/client";

const labels: Record<Permission, string> = {
  read: "查看环境",
  operate: "启停与重启",
  session: "远程连接",
  file: "文件传输",
  observe: "流量与抓包",
  network: "网络策略",
  access: "服务开放",
  compose: "环境编排",
  manage: "重建、销毁与共享",
};

export function PermissionEditor({
  value,
  onChange,
  roles,
}: {
  value: Permission[];
  onChange: (value: Permission[]) => void;
  roles: RolePreset[];
}) {
  const [details, setDetails] = useState(false);
  const role = roles.find(
    (role) =>
      role.permissions.length === value.length &&
      role.permissions.every((permission) => value.includes(permission)),
  );
  return (
    <div className="permission-editor">
      <Select
        label="角色"
        allowDeselect={false}
        value={role?.key ?? "custom"}
        data={[
          ...roles.map((role) => ({ value: role.key, label: role.name })),
          { value: "custom", label: "自定义" },
        ]}
        onChange={(key) => {
          const selected = roles.find((role) => role.key === key);
          if (selected) onChange(selected.permissions);
          else setDetails(true);
        }}
      />
      <button
        type="button"
        className="disclosure"
        aria-expanded={details}
        onClick={() => setDetails(!details)}
      >
        细项权限
        <ChevronDown size={15} className={details ? "rotated" : ""} />
      </button>
      <Collapse in={details}>
        <div className="permission-grid">
          {Object.entries(labels).map(([key, label]) => (
            <Checkbox
              key={key}
              label={label}
              checked={value.includes(key as Permission)}
              onChange={(event) =>
                onChange(
                  event.currentTarget.checked
                    ? [...value, key as Permission]
                    : value.filter((permission) => permission !== key),
                )
              }
            />
          ))}
        </div>
      </Collapse>
    </div>
  );
}
