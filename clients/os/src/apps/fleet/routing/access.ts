// The capabilities that gate Fleet > Routing's tabs.
//
// THE NAMES ARE SETTINGS', ON PURPOSE. Rules and Decisions lived in Settings
// until the routing redesign moved them here, and their role gates moved WITH
// them: the seeded `app:settings/rules` and `app:settings/decisions` rows are
// what the engine and dsl/rbac/seeds.memql already answer for, so re-homing a
// screen is not a reason to mint new capability names nobody holds. Settings
// keeps the two sections as drill-down signposts (registry.tsx), which is also
// what keeps these names declared by the OS registry.

/**
 * Owner or developer, as a set -- explicitly not admin. Writing a route or a
 * rule is configuration; an admin's concern is user administration, and the
 * ladder puts admin below developer so a minimum cannot express it. Routes and
 * Rules both sit behind it.
 */
export const RULES_SECTION_RESOURCE = "app:settings/rules";

/**
 * Owner, developer AND admin. A decision record carries neither the prompt nor
 * the error message, so an admin answering "why did this go to a vendor" can
 * read History.
 */
export const DECISIONS_SECTION_RESOURCE = "app:settings/decisions";
