import type { ReactNode } from "react";

// The container system: exactly two widths, applied consistently, both
// centered -- so the column stops shifting as an operator navigates between
// sections that each picked their own max-w-*.
//
//   data     full width. Row lists, concept browsers, views -- surfaces whose
//            content is enumerable and wants every pixel.
//   reading  a constrained measure for prose and forms, where line length is
//            legibility.

export function Container({
  variant,
  children,
}: {
  variant: "data" | "reading";
  children: ReactNode;
}): ReactNode {
  return (
    <div className={variant === "reading" ? "mx-auto w-full max-w-3xl" : "w-full"}>
      {children}
    </div>
  );
}
