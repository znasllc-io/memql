export { h, text, renderToHtml, escapeHtml, type VNode } from "./vnode.js";
export { renderRowList, rowDisplayId } from "./rowList.js";
export { renderDetail } from "./detail.js";
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
export type { ConceptLike, DisplayCardHints, RowLike } from "./types.js";
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

// Arrangements -- the composition layer a user-composed view is built out of
// (memql#3320). Deterministic first: everything here works with no model, no
// network and no provider, and the AI path is one more producer of the same
// value.
export {
  elementCandidates,
  proposeArrangement,
  explainArrangement,
  arrangementProblems,
  sanitizeArrangement,
  readArrangement,
  arrangementRequest,
  elementOptions,
  EMPTY_ARRANGEMENT,
  ARRANGEMENT_PROPOSAL_SCHEMA,
  type ArrangedElement,
  type Arrangement,
  type ArrangementFault,
  type ArrangementProblem,
  type ArrangementProposal,
  type ArrangementRequest,
  type ElementCandidate,
} from "./arrangement.js";

// The element library.
export {
  VIEW_KIT_ELEMENTS,
  elementById,
  ROW_LIST_ELEMENT,
  DETAIL_ELEMENT,
} from "./elements.js";
export { renderTable, TABLE_ELEMENT } from "./table.js";
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
  formatCompact,
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
  compareScalars,
  isMissing,
  MONTH_NAMES,
  WEEKDAY_NAMES,
} from "./format.js";
