// The two addresses this feature owns. A detail screen is a CHILD ADDRESS
// rather than a modal -- the standard the whole portal is held to (#3316): a
// store an operator is about to back-fill is a link they can send a
// colleague, and a page that survives a refresh.
export function storesPath(): string {
  return "/stores";
}

export function storePath(storeId: string): string {
  return `/stores/${encodeURIComponent(storeId)}`;
}
