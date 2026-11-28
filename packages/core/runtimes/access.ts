import type { RuntimeDevice } from "../types";

/**
 * Runtime details follow the owner's explicit grants or public workspace access.
 * The server authenticates reads and writes while the viewer's session loads.
 */
export function isRuntimeUsableForUser(
  runtime: RuntimeDevice,
  currentUserId: string | null,
): boolean {
  // An ownerless runtime is unusable by anyone, public included: the server
  // needs an owner to mint the agent's task token, so a bind here would only
  // yield agents whose tasks get cancelled at claim time (MUL-3292).
  if (!runtime.owner_id) return false;
  if (!currentUserId) return true;
  if (runtime.owner_id === currentUserId) return true;
  return runtime.visibility === "public" ||
    (runtime.visibility === "private" && (runtime.shared_with ?? []).includes(currentUserId));
}

// Placement and model discovery also honor an operator-configured peer grant.
export function isRuntimePlaceableForUser(
  runtime: RuntimeDevice,
  currentUserId: string | null,
): boolean {
  return isRuntimeUsableForUser(runtime, currentUserId) ||
    (!!runtime.owner_id && !!currentUserId && runtime.visibility === "private" &&
      runtime.policy_peer_id === currentUserId);
}
