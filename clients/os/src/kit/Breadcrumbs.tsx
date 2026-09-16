import { ChevronRight } from "lucide-react";

export interface Breadcrumb { label: string; onSelect?: () => void }
export function Breadcrumbs({ items }: { items: readonly Breadcrumb[] }) {
  return <nav className="os-page-breadcrumbs" aria-label="Breadcrumbs"><ol>{items.map((item, index) => <li key={`${index}:${item.label}`}>
    {index > 0 ? <ChevronRight size={12} aria-hidden /> : null}
    {item.onSelect ? <button type="button" onClick={item.onSelect}>{item.label}</button> : <span aria-current={index === items.length - 1 ? "page" : undefined}>{item.label}</span>}
  </li>)}</ol></nav>;
}
