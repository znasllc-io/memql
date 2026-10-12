import { Notice } from "../../../kit";
import type { ImportReport as Report } from "../actions";

export function ImportReport({ report }: { report: Report }) {
  const parts = [`${report.added} added`, ...(report.duplicates ? [`${report.duplicates} already here`] : []), ...(report.invalid ? [`${report.invalid} could not be read`] : [])];
  return <Notice tone={report.invalid ? "warn" : "info"} sentence={`${parts.join(", ")}${report.total ? ` from ${report.total} rows` : ""}.`} next={report.invalid ? "Fix those lines and import the file again. Addresses already here are not added twice." : undefined}>
    {report.samples.length ? <ul aria-label="Lines that could not be read">{report.samples.map((sample, i) => <li key={i}>Line {sample.line}: <span className="os-mono">{sample.text || "(empty)"}</span> — {sample.reason}</li>)}</ul> : null}
  </Notice>;
}
