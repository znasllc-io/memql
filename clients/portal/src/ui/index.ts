// The portal's component vocabulary. Every screen composes these; a raw
// button/input class string outside src/ui/ is a defect the page-by-page
// pass sweeps for. See README.md in this directory for the type scale and
// the composition rules.

export { Avatar, initialsFrom, type AvatarSize } from "./Avatar";
export { Badge, StatusDot, type StatusTone } from "./Badge";
export { Band, Panel } from "./Band";
export { Breadcrumbs, type Crumb } from "./Breadcrumbs";
export { Button, ButtonLink, type ButtonSize, type ButtonTone } from "./Button";
export { Callout } from "./Callout";
export { Constellation, type ConstellationSize } from "./Constellation";
export { Checkbox, RadioGroup, type RadioOption } from "./Choice";
export { Container } from "./Container";
export { DataText, type DataKind } from "./DataText";
export { ConfirmDialog, Dialog, type DialogSize } from "./Dialog";
export { EmptyState } from "./EmptyState";
export { Field, Select, TextInput, Textarea } from "./Field";
export { FormActions, FormRow } from "./FormRow";
export { LabelChips } from "./LabelChips";
export {
  MARK_EDGES,
  MARK_NODES,
  MARK_NODE_R,
  MARK_STROKE_W,
  MARK_VIEWBOX,
  type MarkEdge,
  type MarkNode,
} from "./markGeometry";
export { prefersReducedMotion, useReducedMotion } from "./motion";
export { PageHeader } from "./PageHeader";
export { PopulationMeta } from "./PopulationMeta";
export { Skeleton } from "./Skeleton";
export { Tabs, type TabItem } from "./Tabs";
