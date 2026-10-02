import { ActionIcon, Button, Drawer, MultiSelect, Select } from "@mantine/core";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { KeyRound, Plus, Trash2, UserRound } from "lucide-react";
import { useState } from "react";
import {
  api,
  type Environment,
  type EnvironmentGrant,
  type Permission,
  type RolePreset,
  type Schema,
} from "../../api/client";
import { ErrorMessage, Loading } from "../../foundation/Feedback";
import { PermissionEditor } from "./PermissionEditor";

type Member = {
  principalId: string;
  permissions: Permission[];
  assetIds: string[];
  original: EnvironmentGrant[];
  changed: boolean;
};

function members(grants: EnvironmentGrant[]): Member[] {
  const result = new Map<string, Member>();
  for (const grant of grants) {
    const member = result.get(grant.principalId) ?? {
      principalId: grant.principalId,
      permissions: [],
      assetIds: [],
      original: [],
      changed: false,
    };
    member.permissions = [
      ...new Set([...member.permissions, ...grant.permissions]),
    ];
    member.assetIds = [
      ...new Set([...member.assetIds, ...(grant.assetIds ?? [])]),
    ];
    member.original.push(grant);
    result.set(grant.principalId, member);
  }
  return [...result.values()];
}

function grantsFor(member: Member): EnvironmentGrant[] {
  if (!member.changed) return member.original;
  if (!member.assetIds.length)
    return [
      { principalId: member.principalId, permissions: member.permissions },
    ];
  return [
    {
      principalId: member.principalId,
      permissions: member.permissions,
      assetIds: member.assetIds,
    },
  ];
}

export function SharingDrawer({
  environment,
  onClose,
}: {
  environment: Environment;
  onClose: () => void;
}) {
  const sharing = useQuery({
    queryKey: ["sharing", environment.id],
    queryFn: () => api.sharing(environment.id),
  });
  const identity = useQuery({ queryKey: ["identity"], queryFn: api.identity });
  return (
    <Drawer
      opened
      onClose={onClose}
      title="共享环境"
      position="right"
      size={448}
    >
      <ErrorMessage error={sharing.error} />
      {sharing.isPending ? (
        <Loading />
      ) : (
        sharing.data && (
          <SharingEditor
            environment={environment}
            sharing={sharing.data}
            roles={identity.data?.roles ?? []}
            onClose={onClose}
          />
        )
      )}
    </Drawer>
  );
}

function SharingEditor({
  environment,
  sharing,
  roles,
  onClose,
}: {
  environment: Environment;
  sharing: Schema<"EnvironmentSharing">;
  roles: RolePreset[];
  onClose: () => void;
}) {
  const [items, setItems] = useState(() => members(sharing.grants));
  const [adding, setAdding] = useState<string | null>(null);
  const [selected, setSelected] = useState<string>();
  const client = useQueryClient();
  const save = useMutation({
    mutationFn: () =>
      api.replaceSharing(environment.id, items.flatMap(grantsFor)),
    onSuccess: () => {
      void client.invalidateQueries({
        queryKey: ["environment", environment.id],
      });
      void client.invalidateQueries({ queryKey: ["environments"] });
      void client.invalidateQueries({ queryKey: ["sharing", environment.id] });
      void client.invalidateQueries({ queryKey: ["identity"] });
      onClose();
    },
  });
  const subjects = new Map(
    sharing.subjects.map((subject) => [subject.id, subject]),
  );
  const roleName = (permissions: Permission[]) =>
    roles.find(
      (role) =>
        role.permissions.length === permissions.length &&
        role.permissions.every((permission) =>
          permissions.includes(permission),
        ),
    )?.name ?? "自定义";
  const edit = (principalId: string, changes: Partial<Member>) =>
    setItems(
      items.map((member) =>
        member.principalId === principalId
          ? { ...member, ...changes, changed: true }
          : member,
      ),
    );
  const current = items.find((member) => member.principalId === selected);
  return (
    <div className="form-stack">
      {sharing.ownerId && subjects.has(sharing.ownerId) && (
        <div className="share-owner">
          <UserRound size={17} />
          <strong>{subjects.get(sharing.ownerId)!.name}</strong>
          <span>所有者</span>
        </div>
      )}
      <div className="sharing-add">
        <Select
          aria-label="添加成员"
          placeholder="选择用户或 Token"
          searchable
          value={adding}
          onChange={setAdding}
          data={sharing.subjects
            .filter(
              (subject) =>
                !subject.disabled &&
                subject.id !== sharing.ownerId &&
                !items.some((member) => member.principalId === subject.id),
            )
            .map((subject) => ({ value: subject.id, label: subject.name }))}
        />
        <ActionIcon
          variant="filled"
          size={36}
          aria-label="添加成员"
          disabled={!adding}
          onClick={() => {
            setItems([
              ...items,
              {
                principalId: adding!,
                permissions: ["read"],
                assetIds: [],
                original: [],
                changed: true,
              },
            ]);
            setSelected(adding!);
            setAdding(null);
          }}
        >
          <Plus size={17} />
        </ActionIcon>
      </div>
      <div className="sharing-members">
        {items.map((member) => {
          const subject = subjects.get(member.principalId);
          return (
            <div
              className={`sharing-member ${selected === member.principalId ? "selected" : ""}`}
              key={member.principalId}
            >
              <button
                type="button"
                onClick={() =>
                  setSelected(
                    selected === member.principalId
                      ? undefined
                      : member.principalId,
                  )
                }
              >
                {subject?.kind === "token" ? (
                  <KeyRound size={17} />
                ) : (
                  <UserRound size={17} />
                )}
                <strong>{subject?.name ?? "已移除账号"}</strong>
                <span>{roleName(member.permissions)}</span>
              </button>
              <ActionIcon
                variant="subtle"
                color="gray"
                aria-label={`移除${subject?.name ?? "成员"}`}
                onClick={() =>
                  setItems(
                    items.filter(
                      (item) => item.principalId !== member.principalId,
                    ),
                  )
                }
              >
                <Trash2 size={15} />
              </ActionIcon>
            </div>
          );
        })}
      </div>
      {current && (
        <section className="sharing-edit">
          <PermissionEditor
            roles={roles}
            value={current.permissions}
            onChange={(permissions) =>
              edit(current.principalId, { permissions })
            }
          />
          <MultiSelect
            label="授权资产"
            placeholder="整个环境"
            searchable
            value={current.assetIds}
            data={(environment.appliedSpec ?? environment.spec).assets.map(
              (asset) => ({ value: asset.id, label: asset.name }),
            )}
            onChange={(assetIds) => edit(current.principalId, { assetIds })}
          />
        </section>
      )}
      {sharing.inherited.length > 0 && (
        <section className="sharing-inherited">
          <h3>项目授权</h3>
          {members(sharing.inherited).map((member) => (
            <div key={member.principalId}>
              <strong>
                {subjects.get(member.principalId)?.name ?? "已移除账号"}
              </strong>
              <span>{roleName(member.permissions)}</span>
            </div>
          ))}
        </section>
      )}
      <ErrorMessage error={save.error} />
      <div className="drawer-footer">
        <Button
          fullWidth
          onClick={() => save.mutate()}
          loading={save.isPending}
        >
          保存授权
        </Button>
      </div>
    </div>
  );
}
