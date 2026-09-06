export { h, text, renderToHtml, escapeHtml, type VNode } from "./vnode.js";
export { renderRowList, rowDisplayId } from "./rowList.js";
export {
  renderValueView,
  valueTypeName,
  joinPath,
  VALUE_VIEW_ATTRS,
  DEFAULT_EXPAND_DEPTH,
  DEFAULT_MAX_STRING_LENGTH,
  DEFAULT_NODE_BUDGET,
  DEFAULT_PAGE_SIZE,
  type ValueTypeName,
  type ValueViewOptions,
} from "./valueView.js";
export {
  inferDisplayCard,
  resolveDisplayCard,
  statusFieldLabel,
  statusText,
  statusValue,
  PRIMARY_NAME_FIELDS,
  SECONDARY_NAME_FIELDS,
  STATUS_NAME_FIELDS,
} from "./displayCard.js";

export {
  renderTopologyGrid,
  renderTally,
  renderDeploymentHistory,
  type TopologyNodeView,
  type TallyRowView,
  type DeploymentHistoryView,
} from "./cluster.js";
// Install run + uninstall preview (memql#3474, #3476). Like the cluster
// renderers above, these take projected view objects rather than concept rows:
// an install step is not a graph row. See install.ts for why they are not
// renderChecklist.
export {
  renderInstallSteps,
  renderRemovalPreview,
  type InstallStepState,
  type InstallStepView,
  type RemovalElevation,
  type RemovalItemKind,
  type RemovalItemView,
} from "./install.js";
// Element personality: what one value looks like, and which fields a table
// or a card shows. Inside view-kit so every consumer improves at once.
export {
  displayColumns,
  cellKind,
  cellAttrs,
  cellContent,
  DISPLAY_COLUMN_CAP,
  type CellKind,
  type RefResolver,
} from "./cell.js";
export type {
  ConceptFieldLike,
  ConceptLike,
  ConceptRelationshipLike,
  DeclaredFieldKind,
  DisplayCardHints,
  RowLike,
} from "./types.js";
export { viewKitStyles, VIEW_KIT_CSS_VARIABLES } from "./styles.js";


// Element fitness -- the contract a view system matches elements against.
export {
  profileConcept,
  fitElement,
  fitElements,
  explainFit,
  renderElement,
  boundField,
  boundFields,
  elementBand,
  isIsoDateString,
  BAND_ROLES,
  BAND_QUESTIONS,
  FIELD_KINDS,
  NON_DISPLAY_FIELDS,
  CATEGORICAL_MAX_DISTINCT,
  type BandRole,
  type FieldKind,
  type FieldProfile,
  type ConceptProfile,
  type DisplaySlotName,
  type ElementRequirement,
  type ElementSpec,
  type ElementOptions,
  type ElementRenderInput,
  type ElementRenderer,
  type ElementFit,
  type FitVerdict,
  type UnmetRequirement,
} from "./fitness.js";

// THE ARRANGEMENT AND LAYOUT MODULES ARE GONE (epic memql#5009, memql#5020).
//
// They were the composition layer a user-composed portal view was built out
// of. The portal was retired in epic memql#4984 and took the only consumer
// with it -- `v1:portalviews:view`, the concept the arrangements were stored
// in, was deleted with it.
//
// REMOVED RATHER THAN DEPRECATED, and the reason is worth keeping because
// the issue that filed this got it backwards. It looked like a breaking
// change to a published package surface: `publishConfig` in package.json, an
// `sdk/` path, an index that re-exports, and a genuinely-published sibling.
// Every signal of a public surface was present except the one that decides
// it. The org registry holds exactly one npm package (`memql-sdk-core`), the
// only publish workflow is `publish-sdk-core.yml` with
// working-directory `sdk/ts`, and nothing has ever published this package --
// its only consumers were two in-repo `file:` dependencies. **Whether
// something is a published surface is answered by the release pipeline, not
// by the package's own metadata.**
//
// With no external consumer possible, keeping it marked would be a
// deprecation window, which this pre-release repo does not do.
//
// ONE RULE FROM IT OUTLIVES IT, and any future grammar with optional layout
// fields needs it: **absent means stack, absent means standard.** An absent
// `layout` or `role` had exactly one reading, decided in one function, so an
// arrangement stored before layouts existed rendered exactly as it always
// had. If "absent" ever came to mean something else, the release that
// changed it would silently re-lay-out every stored arrangement, with no
// migration and nothing in the row to say what it used to look like.

// The element library.
export {
  VIEW_KIT_ELEMENTS,
  elementById,
  ROW_LIST_ELEMENT,
  DETAIL_ELEMENT,
  SCENE_ELEMENT,
  SCENE_ELEMENT_ID,
  WIDGET_ELEMENT,
  WIDGET_ELEMENT_ID,
} from "./elements.js";
export { renderTable, TABLE_ELEMENT, isLongTableField } from "./table.js";
export { renderCalendar, CALENDAR_ELEMENT } from "./calendar.js";
export { renderChecklist, CHECKLIST_ELEMENT } from "./checklist.js";
export { renderTimeline, TIMELINE_ELEMENT } from "./timeline.js";
export { renderStatTiles, STAT_TILE_ELEMENT } from "./statTile.js";
export { renderKanban, KANBAN_ELEMENT } from "./kanban.js";
export { renderMap, MAP_ELEMENT } from "./map.js";
export {
  renderBarChart,
  renderLineChart,
  renderPieChart,
  renderProportionBar,
  axisScale,
  BAR_CHART_ELEMENT,
  LINE_CHART_ELEMENT,
  PIE_CHART_ELEMENT,
  PROPORTION_BAR_ELEMENT,
  CHART_SERIES_SLOTS,
  type AxisScale,
} from "./chart.js";

// Value formatting, exported so a host rendering its own chrome beside an
// element formats a number or an instant the same way the element does.
export {
  scalarText,
  numberValue,
  booleanValue,
  instantValue,
  formatDate,
  formatTime,
  formatDateTime,
  formatNumber,
  formatCompact,
  formatRelative,
  compareScalars,
  isMissing,
  MONTH_NAMES,
  WEEKDAY_NAMES,
} from "./format.js";
