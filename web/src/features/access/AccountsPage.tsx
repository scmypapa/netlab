import {
  ActionIcon,
  Button,
  Drawer,
  Menu,
  Modal,
  PasswordInput,
  TextInput,
  Checkbox,
} from "@mantine/core";
import { useDebouncedValue } from "@mantine/hooks";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  Check,
  Copy,
  KeyRound,
  MoreHorizontal,
  Plus,
  Search,
  UserRound,
} from "lucide-react";
import { useState } from "react";
import {
  api,
  type Principal,
  type RolePreset,
  type Schema,
} from "../../api/client";
import { Empty, ErrorMessage, Loading } from "../../foundation/Feedback";
import { dateTime } from "../../foundation/format";
import { LoadMore } from "../../foundation/LoadMore";
import { useCursorList } from "../../foundation/useCursorList";
import { GrantEditor } from "./GrantEditor";

export function AccountsPage() {
  const identity = useQuery({ queryKey: ["identity"], queryFn: api.identity });
  const [kind, setKind] = useState("user");
  const [query, setQuery] = useState("");
  const [search] = useDebouncedValue(query, 250);
  const accounts = useCursorList(
    ["principals", { kind, search }],
    (page) => api.principals({ ...page, kind, search }),
    { enabled: identity.data?.administrator },
  );
  const [editing, setEditing] = useState<Principal | "new">();
  const [issuing, setIssuing] = useState(false);
  const [revoking, setRevoking] = useState<Principal>();
  const client = useQueryClient();
  const refresh = () =>
    void client.invalidateQueries({ queryKey: ["principals"] });
  const revoke = useMutation({
    mutationFn: () => api.revokeToken(revoking!.id),
    onSuccess: () => {
      setRevoking(undefined);
      refresh();
    },
  });
  const toggle = useMutation({
    mutationFn: (principal: Principal) =>
      api.updateUser(principal.id, {
        name: principal.name,
        disabled: !principal.disabled,
      }),
    onSuccess: refresh,
  });
  if (identity.isPending) return <Loading />;
  if (!identity.data?.administrator)
    return (
      <main className="collection-page">
        <Empty icon={<UserRound size={28} />} title="无权管理账号" />
      </main>
    );
  return (
    <main className="collection-page">
      <div className="page-heading">
        <h1>访问管理</h1>
        <Button
          leftSection={<Plus size={16} />}
          onClick={() =>
            kind === "user" ? setEditing("new") : setIssuing(true)
          }
        >
          {kind === "user" ? "新建用户" : "签发 Token"}
        </Button>
      </div>
      <div className="collection-toolbar">
        <div className="filter-tabs" role="group" aria-label="账号分类">
          {[
            ["user", "用户"],
            ["token", "服务 Token"],
          ].map(([value, label]) => (
            <button
              key={value}
              className={kind === value ? "selected" : ""}
              onClick={() => setKind(value)}
            >
              {label}
            </button>
          ))}
        </div>
        <TextInput
          aria-label="搜索账号"
          placeholder="搜索账号"
          leftSection={<Search size={16} />}
          value={query}
          onChange={(event) => setQuery(event.currentTarget.value)}
        />
      </div>
      <ErrorMessage error={accounts.error ?? toggle.error} />
      {accounts.isPending ? (
        <Loading />
      ) : accounts.data?.length ? (
        <div className="table-surface">
          <table className="data-table">
            <thead>
              <tr>
                <th>名称</th>
                <th>状态</th>
                <th>{kind === "user" ? "创建时间" : "有效期"}</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {accounts.data.map((principal) => (
                <tr key={principal.id}>
                  <td>
                    <div className="object-link">
                      <span className="object-symbol">
                        {kind === "user" ? (
                          <UserRound size={19} />
                        ) : (
                          <KeyRound size={19} />
                        )}
                      </span>
                      <strong>{principal.name}</strong>
                      {principal.administrator && (
                        <span className="access-badge">系统管理员</span>
                      )}
                    </div>
                  </td>
                  <td>
                    {principal.disabled
                      ? kind === "token"
                        ? "已撤销"
                        : "已停用"
                      : principal.expiresAt &&
                          new Date(principal.expiresAt) < new Date()
                        ? "已过期"
                        : "正常"}
                  </td>
                  <td className="muted">
                    {kind === "user"
                      ? dateTime(principal.createdAt)
                      : principal.expiresAt
                        ? dateTime(principal.expiresAt)
                        : "长期有效"}
                  </td>
                  <td className="table-action">
                    {
                      <Menu position="bottom-end">
                        <Menu.Target>
                          <ActionIcon
                            variant="subtle"
                            aria-label={`管理${principal.name}`}
                          >
                            <MoreHorizontal size={18} />
                          </ActionIcon>
                        </Menu.Target>
                        <Menu.Dropdown>
                          {kind === "user" ? (
                            <>
                              <Menu.Item onClick={() => setEditing(principal)}>
                                编辑账号
                              </Menu.Item>
                              <Menu.Item
                                onClick={() => toggle.mutate(principal)}
                              >
                                {principal.disabled ? "启用" : "停用"}
                              </Menu.Item>
                            </>
                          ) : (
                            <Menu.Item
                              color="red"
                              onClick={() => setRevoking(principal)}
                            >
                              撤销 Token
                            </Menu.Item>
                          )}
                        </Menu.Dropdown>
                      </Menu>
                    }
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      ) : (
        !accounts.error && (
          <Empty icon={<UserRound size={28} />} title="暂无账号" />
        )
      )}
      <LoadMore list={accounts} />
      {editing && (
        <UserEditor
          principal={editing === "new" ? undefined : editing}
          onClose={() => setEditing(undefined)}
          onSaved={refresh}
          roles={identity.data.roles ?? []}
        />
      )}
      {issuing && (
        <TokenEditor
          roles={identity.data.roles ?? []}
          onClose={() => setIssuing(false)}
          onSaved={refresh}
        />
      )}
      {revoking && (
        <Modal
          opened
          onClose={() => setRevoking(undefined)}
          title="撤销 Token"
          centered
          size="sm"
        >
          <div className="form-stack">
            <strong>{revoking.name}</strong>
            <ErrorMessage error={revoke.error} />
            <div className="dialog-actions">
              <Button variant="default" onClick={() => setRevoking(undefined)}>
                取消
              </Button>
              <Button
                color="red"
                loading={revoke.isPending}
                onClick={() => revoke.mutate()}
              >
                撤销
              </Button>
            </div>
          </div>
        </Modal>
      )}
    </main>
  );
}

function UserEditor({
  principal,
  onClose,
  onSaved,
  roles,
}: {
  principal?: Principal;
  onClose: () => void;
  onSaved: () => void;
  roles: RolePreset[];
}) {
  const [name, setName] = useState(principal?.name ?? "");
  const [password, setPassword] = useState("");
  const [administrator, setAdministrator] = useState(
    principal?.administrator ?? false,
  );
  const [editedGrants, setGrants] = useState<Schema<"ScopeGrant">[]>();
  const grants = useQuery({
    queryKey: ["principal-grants", principal?.id],
    queryFn: () => api.principalGrants(principal!.id),
    enabled: Boolean(principal),
  });
  const value = editedGrants ?? grants.data ?? [];
  const save = useMutation({
    mutationFn: async () => {
      await (principal
        ? api.updateUser(principal.id, {
            name,
            disabled: principal.disabled,
            administrator,
            ...(editedGrants ? { grants: editedGrants } : {}),
            ...(password ? { password } : {}),
          })
        : api.createUser({ name, password, administrator, grants: value }));
    },
    onSuccess: () => {
      onSaved();
      onClose();
    },
  });
  return (
    <Drawer
      opened
      onClose={onClose}
      title={principal ? "编辑账号" : "新建用户"}
      position="right"
      size={400}
    >
      <form
        className="form-stack"
        onSubmit={(event) => {
          event.preventDefault();
          save.mutate();
        }}
      >
        <TextInput
          label="账号名称"
          required
          autoFocus
          value={name}
          onChange={(event) => setName(event.currentTarget.value)}
        />
        <PasswordInput
          label={principal ? "重置密码" : "密码"}
          required={!principal}
          minLength={8}
          maxLength={72}
          value={password}
          onChange={(event) => setPassword(event.currentTarget.value)}
        />
        <ErrorMessage error={save.error} />
        <Checkbox
          label="系统管理员"
          checked={administrator}
          onChange={(event) => setAdministrator(event.currentTarget.checked)}
        />
        {!administrator && (
          <GrantEditor value={value} onChange={setGrants} roles={roles} />
        )}
        <ErrorMessage error={grants.error} />
        <div className="drawer-footer">
          <Button fullWidth type="submit" loading={save.isPending}>
            保存
          </Button>
        </div>
      </form>
    </Drawer>
  );
}

function TokenEditor({
  roles,
  onClose,
  onSaved,
}: {
  roles: RolePreset[];
  onClose: () => void;
  onSaved: () => void;
}) {
  const [name, setName] = useState("");
  const [grants, setGrants] = useState<Schema<"ScopeGrant">[]>([]);
  const [expires, setExpires] = useState("");
  const [copied, setCopied] = useState(false);
  const issue = useMutation({
    mutationFn: () =>
      api.createToken({
        name,
        grants,
        ...(expires ? { expiresAt: new Date(expires).toISOString() } : {}),
      }),
    onSuccess: onSaved,
  });
  return (
    <Drawer
      opened
      onClose={onClose}
      title={issue.data ? "Token 已签发" : "签发服务 Token"}
      position="right"
      size={420}
    >
      {issue.data ? (
        <div className="form-stack">
          <div className="token-secret">
            <code>{issue.data.token}</code>
            <ActionIcon
              aria-label="复制 Token"
              variant="default"
              onClick={async () => {
                await navigator.clipboard.writeText(issue.data!.token);
                setCopied(true);
              }}
            >
              {copied ? <Check size={17} /> : <Copy size={17} />}
            </ActionIcon>
          </div>
          <div className="drawer-footer">
            <Button fullWidth onClick={onClose}>
              完成
            </Button>
          </div>
        </div>
      ) : (
        <form
          className="form-stack"
          onSubmit={(event) => {
            event.preventDefault();
            issue.mutate();
          }}
        >
          <TextInput
            label="Token 名称"
            required
            autoFocus
            value={name}
            onChange={(event) => setName(event.currentTarget.value)}
          />
          <GrantEditor roles={roles} value={grants} onChange={setGrants} />
          <TextInput
            label="有效期"
            type="datetime-local"
            value={expires}
            onChange={(event) => setExpires(event.currentTarget.value)}
          />
          <ErrorMessage error={issue.error} />
          <div className="drawer-footer">
            <Button
              fullWidth
              type="submit"
              disabled={!grants.length}
              loading={issue.isPending}
            >
              签发
            </Button>
          </div>
        </form>
      )}
    </Drawer>
  );
}
