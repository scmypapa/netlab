import {
  Button,
  Modal,
  NumberInput,
  PasswordInput,
  Select,
  Textarea,
  TextInput,
} from "@mantine/core";
import { useMutation, useQuery } from "@tanstack/react-query";
import { useEffect, useState } from "react";
import { api, ApiError, type Asset, type Schema } from "../../api/client";
import { ErrorMessage, Loading } from "../../foundation/Feedback";
import styles from "./FileWorkspace.module.css";

export function SSHDialog({
  environmentId,
  asset,
  onClose,
  onSaved,
}: {
  environmentId: string;
  asset: Asset;
  onClose: () => void;
  onSaved: () => void;
}) {
  const saved = useQuery({
    queryKey: ["ssh", environmentId, asset.id],
    retry: false,
    queryFn: async () => {
      try {
        return await api.sshSettings(environmentId, asset.id);
      } catch (error) {
        if (error instanceof ApiError && error.status === 404) return null;
        throw error;
      }
    },
  });
  const [settings, setSettings] = useState<Schema<"SSHSettings">>({
    username: asset.guest?.username ?? "",
    port: 22,
    authKind: "password",
    hostKey: "",
  });
  const [password, setPassword] = useState("");
  const [privateKey, setPrivateKey] = useState("");
  const [passphrase, setPassphrase] = useState("");
  useEffect(() => {
    if (saved.data) setSettings(saved.data);
  }, [saved.data]);
  const probe = useMutation({
    mutationFn: () =>
      api.sshHostKey(environmentId, asset.id, {
        port: settings.port,
        interfaceId: settings.interfaceId,
      }),
    onSuccess: (value) =>
      setSettings((current) => ({ ...current, hostKey: value.fingerprint })),
  });
  const save = useMutation({
    mutationFn: () =>
      api.saveSSHSettings(environmentId, asset.id, {
        ...settings,
        password: password || undefined,
        privateKey: privateKey || undefined,
        passphrase: passphrase || undefined,
      }),
    onSuccess: onSaved,
  });
  const change = <K extends keyof Schema<"SSHSettings">>(
    name: K,
    value: Schema<"SSHSettings">[K],
  ) => setSettings((current) => ({ ...current, [name]: value }));
  return (
    <Modal
      opened
      title={`${asset.name} · SSH 连接`}
      onClose={onClose}
      centered
      size="md"
    >
      {saved.isPending ? (
        <Loading />
      ) : (
        <form
          className={styles.form}
          onSubmit={(event) => {
            event.preventDefault();
            save.mutate();
          }}
        >
          <ErrorMessage error={saved.error ?? probe.error ?? save.error} />
          <div className={styles.columns}>
            <TextInput
              label="用户名"
              value={settings.username}
              onChange={(event) =>
                change("username", event.currentTarget.value)
              }
              required
              autoFocus
            />
            <NumberInput
              label="端口"
              value={settings.port}
              min={1}
              max={65535}
              onChange={(value) => {
                change("port", Number(value));
                change("hostKey", "");
              }}
            />
          </div>
          {asset.interfaces.length > 1 && (
            <Select
              label="网络接口"
              value={settings.interfaceId ?? ""}
              data={[
                { value: "", label: "主接口" },
                ...asset.interfaces.map((item) => ({
                  value: item.id,
                  label: item.address || "未分配地址",
                })),
              ]}
              onChange={(value) => {
                change("interfaceId", value || undefined);
                change("hostKey", "");
              }}
            />
          )}
          <Select
            label="认证"
            value={settings.authKind}
            data={[
              { value: "password", label: "密码" },
              { value: "key", label: "私钥" },
            ]}
            onChange={(value) =>
              change("authKind", value as Schema<"SSHSettings">["authKind"])
            }
          />
          {settings.authKind === "password" ? (
            <PasswordInput
              label="密码"
              value={password}
              placeholder={saved.data?.authKind === "password" ? "已保存" : ""}
              onChange={(event) => setPassword(event.currentTarget.value)}
              autoComplete="new-password"
            />
          ) : (
            <>
              <Textarea
                label="私钥"
                value={privateKey}
                onChange={(event) => setPrivateKey(event.currentTarget.value)}
                minRows={4}
                maxRows={8}
                autosize
                placeholder={saved.data?.authKind === "key" ? "已保存" : ""}
              />
              <PasswordInput
                label="私钥口令"
                value={passphrase}
                onChange={(event) => setPassphrase(event.currentTarget.value)}
              />
            </>
          )}
          <div className={styles.key}>
            <Button
              variant="default"
              size="compact-sm"
              onClick={() => probe.mutate()}
              loading={probe.isPending}
            >
              读取主机密钥
            </Button>
            {settings.hostKey && <code>{settings.hostKey}</code>}
          </div>
          <div className={styles.dialogActions}>
            <Button variant="default" onClick={onClose}>
              取消
            </Button>
            <Button
              type="submit"
              loading={save.isPending}
              disabled={!settings.hostKey || probe.isPending}
            >
              {settings.hostKey === saved.data?.hostKey ? "保存" : "信任并保存"}
            </Button>
          </div>
        </form>
      )}
    </Modal>
  );
}
