import { useState } from "react";
import { LayoutTemplate } from "lucide-react";
import {
  Button,
  ChoiceStack,
  Input,
  Notice,
  RecordList,
  RecordListSkeleton,
  RecordRow,
  type Stop,
} from "../../kit";
import { Wizard } from "../../kit/Wizard";
import { AccountPicker } from "../accounts/AccountPicker";
import { accountIsArchived, type AccountRow } from "../accounts/rows";
import type { NewTemplateFacts } from "./actions";
import type { FileOption } from "./ingredientFiles";
import { FORMATS, formatWord } from "./words";

export function AddTemplateWizard({
  files,
  filesLoading,
  filesError,
  accounts,
  busy,
  refusal,
  onAdd,
  onCancel,
  onRetryFiles,
}: {
  files: FileOption[];
  filesLoading: boolean;
  filesError: string;
  accounts: AccountRow[];
  busy: boolean;
  refusal: string;
  onAdd: (facts: NewTemplateFacts) => void;
  onCancel: () => void;
  onRetryFiles: () => void;
}) {
  const [open, setOpen] = useState("file"),
    [fileId, setFileId] = useState(""),
    [format, setFormat] = useState("markdown"),
    [name, setName] = useState(""),
    [description, setDescription] = useState(""),
    [accountId, setAccountId] = useState("");
  const [search, setSearch] = useState("");
  const activeAccounts = accounts.filter((a) => !accountIsArchived(a));
  const selected = files.find((f) => f.id === fileId);
  const ready = !!selected && !!name.trim();
  function pick(file: FileOption) {
    setFileId(file.id);
    setName(file.name.replace(/\.[^.]+$/, ""));
    const ext = file.name.slice(file.name.lastIndexOf(".") + 1).toLowerCase();
    setFormat(
      (
        {
          md: "markdown",
          markdown: "markdown",
          htm: "html",
          html: "html",
          txt: "txt",
          csv: "csv",
          json: "json",
          docx: "docx",
          pdf: "pdf",
        } as Record<string, string>
      )[ext] || "markdown",
    );
    setOpen("kind");
  }
  const steps: Stop[] = [
    {
      id: "file",
      name: "File",
      sentence: "The Library file it is drawn from.",
      state: fileId ? "done" : "waiting",
      answer: selected?.name,
      body: (
        <>
          <Input
            id="mz-template-search"
            label="Search your files"
            value={search}
            onChange={setSearch}
            placeholder="Search"
          />
          {filesLoading && !files.length ? (
            <RecordListSkeleton label="Reading files" />
          ) : null}
          {filesError ? (
            <>
              <Notice
                tone="error"
                sentence="This cluster did not return your files."
                detail={filesError}
              />
              <Button tone="quiet" onClick={onRetryFiles}>
                Try again
              </Button>
            </>
          ) : null}
          <RecordList label="Template files">
            {files
              .filter(
                (f) =>
                  f.kind !== "ZIP" &&
                  f.name.toLowerCase().includes(search.trim().toLowerCase()),
              )
              .map((file) => (
                <RecordRow
                  key={file.id}
                  name={file.name}
                  secondary={file.kind}
                  selected={fileId === file.id}
                  onOpen={() => pick(file)}
                />
              ))}
          </RecordList>
          {!filesLoading && !filesError && !files.length ? (
            <p className="os-caption">Upload the file in Files first.</p>
          ) : null}
        </>
      ),
    },
    {
      id: "kind",
      name: "Kind of file",
      sentence: "What it makes.",
      state: "done",
      answer: formatWord(format),
      body: (
        <ChoiceStack
          name="mz-template-kind"
          label="Kind of file"
          voice="prose"
          value={format}
          onChange={setFormat}
          options={FORMATS.map((value) => ({
            value,
            label: formatWord(value),
          }))}
        />
      ),
    },
    {
      id: "name",
      name: "Name",
      sentence: "What to call it.",
      state: name.trim() ? "done" : "waiting",
      answer: name.trim() || undefined,
      body: (
        <Input
          id="mz-template-name"
          label="Name"
          value={name}
          onChange={setName}
          onEnter={() => {
            if (ready) onAdd({ fileId, format, name, description, accountId });
          }}
        />
      ),
    },
    {
      id: "description",
      name: "Description",
      sentence: "What it is for.",
      state: description.trim() ? "done" : "skipped",
      answer: description || "None",
      body: (
        <Input
          id="mz-template-description"
          label="Description"
          value={description}
          onChange={setDescription}
        />
      ),
    },
    ...(activeAccounts.length
      ? [
          {
            id: "organization",
            name: "Organization",
            sentence: "Who it is for.",
            state: accountId ? ("done" as const) : ("skipped" as const),
            answer:
              activeAccounts.find((a) => a.id === accountId)?.name || "None",
            body: (
              <AccountPicker
                id="mz-template-org"
                label="Organization"
                value={accountId}
                onChange={setAccountId}
                accounts={activeAccounts}
                emptyLabel="No organization"
              />
            ),
          },
        ]
      : []),
  ];
  return (
    <Wizard
      className="mz-make"
      icon={<LayoutTemplate />}
      title="Add a template"
      label="Add a template"
      breadcrumbs={[
        { label: "Templates", onSelect: onCancel },
        { label: "Add" },
      ]}
      steps={steps.map((step) => ({ ...step, openable: !busy }))}
      open={open}
      onOpen={setOpen}
      status={{
        word: busy
          ? "Adding the template"
          : ready
            ? "Ready to add the template"
            : !fileId
              ? "Choose a file"
              : "Name the template",
        tone: busy ? "busy" : "none",
      }}
      notices={
        refusal ? (
          <Notice
            tone="error"
            sentence="The template was not added."
            detail={refusal}
          />
        ) : null
      }
      acts={
        busy
          ? []
          : [
              { label: "Cancel", text: true, onAct: onCancel },
              ...(ready
                ? [
                    {
                      label: "Add template",
                      tone: "primary" as const,
                      onAct: () =>
                        onAdd({ fileId, format, name, description, accountId }),
                    },
                  ]
                : []),
            ]
      }
    />
  );
}
