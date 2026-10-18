import type { ReactNode } from "react";
import { Subhead } from "./controls";
import "./details.css";

/** Fleet's settings rhythm, shared with Cluster: heading, aligned content. */
export function DetailsSection({ title, label = title, children }: { title: string; label?: string; children: ReactNode }) {
  return <section className="os-details-section" aria-label={label}>
    <Subhead>{title}</Subhead>
    <div className="os-details-section-body">{children}</div>
  </section>;
}

export function Details({ children }: { children: ReactNode }) {
  return <div className="os-details">{children}</div>;
}

export function SettingRow({ label, children }: { label: string; children: ReactNode }) {
  return <div className="os-setting-row">
    <div className="os-setting-name">{label}</div>
    <div className="os-setting-value">{children}</div>
  </div>;
}
