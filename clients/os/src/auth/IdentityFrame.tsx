import { useId, type ReactNode } from "react";
import { Mark } from "../chrome/Mark";
import "./identity.css";

/** One first-party frame for every authentication step, including failures. */
export function IdentityFrame({ title, lead, children, footer, wide = false }: {
  title: string; lead?: ReactNode; children: ReactNode; footer?: ReactNode; wide?: boolean;
}) {
  const heading = useId();
  return <main className="os-signin" aria-labelledby={heading}>
    <section className="os-signin-content" data-wide={wide || undefined}>
      <div className="os-identity-brand"><Mark size={36} /><span>MemQL</span></div>
      <header className="os-signin-heading"><h1 id={heading}>{title}</h1>{lead && <p>{lead}</p>}</header>
      {children}
      {footer && <footer className="os-identity-footer">{footer}</footer>}
    </section>
  </main>;
}
