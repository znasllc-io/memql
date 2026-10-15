/**
 * Only messages authored by this extension may be displayed verbatim.
 *
 * This lives apart from problems.ts, which reaches the SDK, so the bundles that
 * run inside a webview can refuse input in words the person reads without
 * carrying the SDK with them.
 */
export class UserInputError extends Error {}
