import type { ChromeLayout } from "../app/layout";
import { Mark } from "./Mark";

export function Launcher({ layout }: { layout: ChromeLayout }) {
  return (
    <header className="os-launcher" data-launcher data-layout={layout}>
      <span className="os-brand">
        <Mark className="os-mark" />
        <span className="os-wordmark">MemQL</span>
      </span>
    </header>
  );
}
