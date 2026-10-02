import type { Schema } from "../../api/client";

export async function wireguardKeys() {
  const pair = (await crypto.subtle.generateKey({ name: "X25519" }, true, [
    "deriveBits",
  ])) as CryptoKeyPair;
  const [secret, publicKey] = await Promise.all([
    crypto.subtle.exportKey("jwk", pair.privateKey),
    crypto.subtle.exportKey("raw", pair.publicKey),
  ]);
  return {
    privateKey: secret.d!.replace(/-/g, "+").replace(/_/g, "/") + "=",
    publicKey: btoa(String.fromCharCode(...new Uint8Array(publicKey))),
  };
}

export function wireguardConfig(
  connection: Schema<"VPNConnection">,
  privateKey?: string,
) {
  return [
    "[Interface]",
    ...(privateKey ? [`PrivateKey = ${privateKey}`] : []),
    `Address = ${connection.addresses.join(", ")}`,
    `MTU = ${connection.mtu}`,
    "",
    "[Peer]",
    `PublicKey = ${connection.publicKey}`,
    `AllowedIPs = ${connection.allowedIPs.join(", ")}`,
    `Endpoint = ${connection.endpoint}`,
    "PersistentKeepalive = 25",
    "",
  ].join("\n");
}

export function downloadWireguard(config: string) {
  const url = URL.createObjectURL(new Blob([config], { type: "text/plain" }));
  const link = document.createElement("a");
  link.href = url;
  link.download = "WireGuard.conf";
  link.click();
  setTimeout(() => URL.revokeObjectURL(url), 0);
}
