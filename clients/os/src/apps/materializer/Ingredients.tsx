import { useMemo, useState } from "react";
import { Database, File, X } from "lucide-react";
import { IconButton } from "../../kit/IconButton";
import {
  Button,
  Chip,
  Input,
  Notice,
  RecordList,
  RecordListSkeleton,
  RecordRow,
  Subhead,
} from "../../kit";
import type { Ingredient } from "./make";
import type { FileOption } from "./ingredientFiles";
import type { ComposablesState, ResolveState } from "./useCompose";

export interface IngredientsProps {
  picked: Ingredient[];
  onChange: (picked: Ingredient[]) => void;
  files: FileOption[];
  filesLoading: boolean;
  filesError: string;
  retryFiles: () => void;
  records: ComposablesState;
  resolved: ResolveState;
}
export function Ingredients({
  picked,
  onChange,
  files,
  filesLoading,
  filesError,
  retryFiles,
  records,
  resolved,
}: IngredientsProps) {
  const [search, setSearch] = useState("");
  const query = search.trim().toLowerCase();
  const offeredFiles = useMemo(
    () =>
      query
        ? files.filter((file) => file.name.toLowerCase().includes(query))
        : files.slice(0, 20),
    [files, query],
  );
  const offeredRecords = records.concepts
    .filter(
      (c) =>
        c.list &&
        (!query || `${c.as} ${c.description}`.toLowerCase().includes(query)),
    )
    .sort((a, b) => Number(b.marked) - Number(a.marked));
  const selected = (kind: Ingredient["kind"], ref: string) =>
    picked.some((item) => item.kind === kind && item.ref === ref);
  const toggle = (item: Ingredient) =>
    onChange(
      selected(item.kind, item.ref)
        ? picked.filter((p) => p.kind !== item.kind || p.ref !== item.ref)
        : [...picked, item],
    );
  return (
    <div className="mz-ingredients">
      {picked.length ? (
        <ul className="mz-picked" aria-label="Picked things">
          {picked.map((item) => {
            const count = resolved.sources.find(
              (s) => s.kind === item.kind && s.ref === item.ref,
            );
            const fact =
              item.kind === "library_file"
                ? files.find((f) => f.id === item.ref)?.kind || "File"
                : count
                  ? count.problem ||
                    `${count.count} ${count.count === 1 ? "row" : "rows"}`
                  : "—";
            return (
              <li key={`${item.kind}:${item.ref}`} className="mz-ingredient">
                {item.kind === "library_file" ? (
                  <File size={18} aria-hidden />
                ) : (
                  <Database size={18} aria-hidden />
                )}
                <span>
                  <strong>{item.label || item.ref}</strong>
                  <small>{fact}</small>
                </span>
                <IconButton
                  label={`Remove ${item.label || item.ref}`}
                  onClick={() => toggle(item)}
                >
                  <X size={16} />
                </IconButton>
              </li>
            );
          })}
        </ul>
      ) : null}
      {resolved.error ? (
        <Notice tone="error" sentence={resolved.error} />
      ) : null}
      <Input
        id="mz-ingredient-search"
        label="Search your files and records"
        placeholder="Search"
        value={search}
        onChange={setSearch}
      />
      <Subhead>Files</Subhead>
      {filesLoading && !files.length ? (
        <RecordListSkeleton label="Reading your files" />
      ) : null}
      {filesError ? (
        <>
          <Notice tone="error" sentence={filesError} />
          <Button tone="quiet" onClick={retryFiles}>
            Try again
          </Button>
        </>
      ) : null}
      <RecordList label="Files to pick">
        {offeredFiles.map((file) => (
          <RecordRow
            key={file.id}
            name={file.name}
            secondary={file.kind}
            selected={selected("library_file", file.id)}
            onOpen={() =>
              toggle({ kind: "library_file", ref: file.id, label: file.name })
            }
          />
        ))}
      </RecordList>
      {!query && files.length > 20 ? (
        <p className="os-caption">Search to find older files.</p>
      ) : null}
      <Subhead>Records</Subhead>
      {records.loading && !records.concepts.length ? (
        <RecordListSkeleton label="Reading record types" />
      ) : null}
      {records.error ? (
        <>
          <Notice tone="error" sentence={records.error} />
          <Button tone="quiet" onClick={records.reread}>
            Try again
          </Button>
        </>
      ) : null}
      {!records.loading && !records.registryAvailable ? (
        <Notice
          tone="info"
          sentence="This node cannot read the record types, so none are offered."
        />
      ) : null}
      <RecordList label="Records to pick">
        {offeredRecords.map((record) => (
          <RecordRow
            key={record.id}
            name={record.as || record.id}
            secondary={record.description}
            selected={selected("query", `query ${record.list}()`)}
            onOpen={() =>
              toggle({
                kind: "query",
                ref: `query ${record.list}()`,
                label: record.as || record.id,
              })
            }
          >
            {!record.marked ? <Chip>Unmarked</Chip> : null}
          </RecordRow>
        ))}
      </RecordList>
      {!filesLoading &&
      !records.loading &&
      !filesError &&
      !records.error &&
      !offeredFiles.length &&
      !offeredRecords.length ? (
        <p className="os-caption">
          {query
            ? "No matches. Clear the search to see everything."
            : "Nothing to pick yet. Files you upload in Files appear here."}
        </p>
      ) : null}
    </div>
  );
}
