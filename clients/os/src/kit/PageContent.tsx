import type { ComponentPropsWithRef } from "react";
import "./pageContent.css";

/** Shared page bounds for windows and retained app pages. Constrain fields or
 * prose inside the page, never the page itself. Scrolling stays with the host. */
type PageContentProps =
  | (ComponentPropsWithRef<"div"> & { as?: "div" })
  | (ComponentPropsWithRef<"main"> & { as: "main" });

export function PageContent(props: PageContentProps) {
  if (props.as === "main") {
    const { as: _tag, className = "", ...rest } = props;
    return <main {...rest} className={`os-page-content ${className}`.trim()} />;
  }
  const { as: _tag, className = "", ...rest } = props;
  return <div {...rest} className={`os-page-content ${className}`.trim()} />;
}
