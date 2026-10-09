import { randomUUID } from "../../foundation/id";
import { useMutation, useQuery } from "@tanstack/react-query";
import { useEffect, useRef } from "react";
import {
  api,
  type Environment,
  type Operation,
  type Schema,
} from "../../api/client";
import { downloadWireguard, wireguardConfig, wireguardKeys } from "./wireguard";

export type VPNInput = Pick<
  Schema<"CreateVPNAccess">,
  "name" | "networkIds" | "mode"
>;

export function useVPNAccess(
  id: string,
  opened: boolean,
  environment: Environment | undefined,
  operations: Operation[],
  currentOperation: Operation | undefined,
  refresh: () => void,
) {
  const vault = useRef({
    environmentId: id,
    keys: new Map<string, string>(),
  });
  if (vault.current.environmentId !== id)
    vault.current = { environmentId: id, keys: new Map() };
  const keys = vault.current.keys;
  useEffect(() => () => keys.clear(), [keys]);
  const access = useQuery({
    queryKey: ["vpn-access", id],
    queryFn: () => api.vpnAccess(id),
    enabled: opened && Boolean(environment?.permissions?.includes("access")),
  });
  const create = useMutation({
    gcTime: 0,
    mutationFn: async (input: VPNInput) => {
      const pair = await wireguardKeys();
      const operation = await api.createVPNAccess(id, {
        ...input,
        publicKey: pair.publicKey,
        expectedRevision: environment!.revision,
        clientRequestId: randomUUID(),
      });
      keys.set(pair.publicKey, pair.privateKey);
      return operation;
    },
    onSuccess: refresh,
  });
  const revoke = useMutation({
    gcTime: 0,
    mutationFn: async (item: Schema<"VPNAccess">) => {
      const operation = await api.revokeVPNAccess(
        id,
        item.id,
        environment!.revision,
        randomUUID(),
      );
      keys.delete(item.publicKey);
      return operation;
    },
    onSuccess: refresh,
  });
  const canDownload = (item: Schema<"VPNAccess">) => {
    return Boolean(
      keys.has(item.publicKey) &&
      item.state === "active" &&
      [...operations, currentOperation].some(
        (operation) =>
          operation?.id === item.operationId && operation.state === "succeeded",
      ),
    );
  };
  const download = useMutation({
    gcTime: 0,
    mutationFn: async (item: Schema<"VPNAccess">) => {
      const privateKey = keys.get(item.publicKey)!;
      const connection = await api.vpnConnection(id, item.id);
      downloadWireguard(wireguardConfig(connection, privateKey));
      keys.delete(item.publicKey);
    },
  });
  const copy = useMutation({
    mutationFn: async (item: Schema<"VPNAccess">) => {
      const connection = await api.vpnConnection(id, item.id);
      await navigator.clipboard.writeText(wireguardConfig(connection));
    },
  });
  return { access, create, revoke, canDownload, download, copy };
}
