import type { Schema } from "../../api/client";

export function allows(
  environment: Schema<"Environment"> | undefined,
  permission: Schema<"Permission">,
  assetId?: string,
) {
  return Boolean(
    environment?.permissions?.includes(permission) ||
    (assetId && environment?.assetPermissions?.[assetId]?.includes(permission)),
  );
}
