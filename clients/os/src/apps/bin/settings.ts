// The Bin has no settings store of its own, and that is the design.
//
// The one preference this app reads is "ask before archiving", which belongs
// to the Files app and is stored there (`apps/files/settings.ts`). The issue
// that asked for the Bin said it plainly: the Bin invokes the same mutations
// and consults the same setting, and re-specifies neither. A second copy of
// that checkbox would be a second answer to one question, and the two would
// disagree the first time somebody changed one of them.
//
// So this app's settings section is a DOOR onto that setting plus the one
// standing fact about the Bin somebody looking for a setting is actually
// trying to find out: nothing here expires.

export { BIN_SECTIONS } from "./concepts";
