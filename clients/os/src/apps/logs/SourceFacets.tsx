import { Input, Select } from "../../kit";
import type { LogFilters } from "../../logs/filters";
import type { LogSources, SourceOption } from "../../logs/useLogSearch";

// The facets that come from the CLUSTER rather than from a fixed list
// (spec D, `logsSources`): the components, nodes and apps that actually
// logged inside the window, each with its count in the option label, so a
// value that never logged is never offered -- and the portal, which is not
// instrumented and has no component name, is absent by construction rather
// than by a list.
//
// Each select holds ONE value: the shell's Select is single-choice, and a
// person narrowing a stream picks a source, reads, and picks another. The
// filter state is a list so the engine's `in` semantics are kept for the day
// a multi-select earns its place in the kit.

const ANY = "";

function optionLabel(option: SourceOption): string {
  return `${option.value}${option.nodeType === "" ? "" : ` · ${option.nodeType}`} (${option.count.toLocaleString()})`;
}

export function SourceFacets({
  sources,
  filters,
  patch,
  idPrefix,
}: {
  sources: LogSources;
  filters: LogFilters;
  patch: (next: Partial<LogFilters>) => void;
  /** Two sections mount these; their control ids must not collide. */
  idPrefix: string;
}) {
  const one = (list: string[]): string => list[0] ?? ANY;
  const asList = (value: string): string[] => (value === ANY ? [] : [value]);
  return (
    <>
      <Select
        id={`${idPrefix}-component`}
        label="Component"
        value={one(filters.components)}
        onChange={(value) => patch({ components: asList(value) })}
      >
        <option value={ANY}>Any component</option>
        {sources.components.map((option) => (
          <option key={option.value} value={option.value}>
            {optionLabel(option)}
          </option>
        ))}
      </Select>
      <Select
        id={`${idPrefix}-node`}
        label="Node"
        value={one(filters.nodes)}
        onChange={(value) => patch({ nodes: asList(value) })}
      >
        <option value={ANY}>Any node</option>
        {sources.nodes.map((option) => (
          <option key={option.value} value={option.value}>
            {optionLabel(option)}
          </option>
        ))}
      </Select>
      <Select
        id={`${idPrefix}-app`}
        label="App"
        value={one(filters.apps)}
        onChange={(value) => patch({ apps: asList(value) })}
      >
        <option value={ANY}>Any app</option>
        {sources.apps.map((option) => (
          <option key={option.value} value={option.value}>
            {optionLabel(option)}
          </option>
        ))}
      </Select>
      <Input
        id={`${idPrefix}-subject`}
        label="Subject"
        placeholder="Subject id"
        value={filters.subject}
        onChange={(subject) => patch({ subject, subjectConcept: subject === "" ? "" : filters.subjectConcept })}
      />
    </>
  );
}
