import { useRef, type ReactNode } from "react";
import { render } from "@testing-library/react";
import { PageNavigationProvider } from "../src/kit/pageNavigation";
import { TrailRow } from "../src/kit/TrailRow";

export function PageFrame({ children }: { children: ReactNode }) {
  const root = useRef<HTMLDivElement>(null);
  return <div ref={root}><PageNavigationProvider root={root} trail={[]}>
    <TrailRow fallback="Page" />{children}
  </PageNavigationProvider></div>;
}
export function renderPage(children: ReactNode) { return render(<PageFrame>{children}</PageFrame>); }
