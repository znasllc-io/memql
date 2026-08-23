// The nav-row recipe, in one place (memql#4316 / #4317).
//
// It was a private function inside AppShell until the profile row needed it
// too. A copy would have been the obvious move and the wrong one: the row at
// the top of the rail has to be indistinguishable from the rows below it --
// same hover wash, same 2px accent edge, same transparent border at rest so
// activation never shifts the text -- and two copies of that recipe drift the
// moment either one is tuned.
//
// It lives HERE rather than being exported from AppShell because AppShell
// imports the profile row: exporting it from there would make the two modules
// import each other.

export function navClass(isActive: boolean, collapsed: boolean): string {
  return (
    "flex items-center gap-2.5 rounded py-1.5 text-sm " +
    (collapsed ? "justify-center px-0 " : "px-2.5 ") +
    // The active edge: a 2px accent bar on the left plus a soft fill. The
    // border is always present (transparent at rest) so activation never
    // shifts the text.
    "border-l-2 " +
    (isActive
      ? "border-accent bg-accent-subtle font-medium text-fg"
      : "border-transparent text-muted hover:bg-raised hover:text-fg")
  );
}
