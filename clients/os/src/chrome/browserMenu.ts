// The shell's right-click rule.
//
// ===========================================================================
// THE BROWSER'S MENU IS OFF BY DEFAULT, NOT ON
// ===========================================================================
// This is a desktop, and a desktop's right-click belongs to the desktop. Every
// surface that has something to offer on right-click already offers it -- the
// desk background, a desk item, a dock pin. Everywhere else the browser's own
// menu appeared instead: Back, Reload, View Page Source, over a window that is
// pretending not to be a web page. It is the single loudest tell that the
// shell is running in a tab.
//
// So the default is INVERTED at the shell root rather than audited element by
// element. Suppressing per element means every new control is one someone
// forgot; suppressing at the root means a new control is silent unless it
// asks to speak. A surface with its own menu still calls preventDefault and
// shows it -- this handler running afterwards is a no-op.
//
// ===========================================================================
// THREE PLACES WHERE THE BROWSER'S MENU IS THE FEATURE
// ===========================================================================
// The rule is "nothing happens where nothing is offered", NOT "nothing ever
// happens". In three cases the browser's menu is the only thing offering the
// action, and taking it away removes a capability rather than a distraction:
//
//   - an EDITABLE FIELD, where the menu is cut / copy / paste. A desktop OS
//     shows a menu on a text field; removing it here would mean a person with
//     no keyboard shortcuts cannot paste a worker token.
//   - a LIVE TEXT SELECTION, where it is copy. Someone who has just selected
//     an error message or a registration id is reaching for exactly that.
//   - a LINK, where it is copy-address and open-in-new-tab.
//
// Each of these is a control the browser owns and the shell has no
// replacement for. When the shell grows its own clipboard menu, this list
// shrinks.

/** The three cases above, asked of one event target. */
export function browserMenuIsTheFeature(
  target: EventTarget | null,
  selection: { isCollapsed: boolean } | null,
): boolean {
  const element = target instanceof Element ? target : null;
  if (element !== null) {
    // `closest` rather than a tag check: the click usually lands on a child --
    // the text inside a link, a span inside a contenteditable.
    if (element.closest("input, textarea")) return true;
    if (element.closest('[contenteditable]:not([contenteditable="false"])')) return true;
    if (element.closest("a[href]")) return true;
  }
  // A selection can span nodes far from the target, so it is asked LAST and
  // independently of where the pointer happens to be.
  return selection !== null && !selection.isCollapsed;
}

/**
 * The root handler. Attach to every shell root; it needs nothing else.
 *
 * `selection` is injected so the rule is testable without a live document,
 * and defaults to the real one.
 */
export function suppressBrowserMenu(
  event: Pick<MouseEvent, "target"> & { preventDefault: () => void },
  selection: { isCollapsed: boolean } | null = readSelection(),
): void {
  if (browserMenuIsTheFeature(event.target, selection)) return;
  event.preventDefault();
}

function readSelection(): { isCollapsed: boolean } | null {
  // getSelection is absent in some embeddings and throws in none; a missing
  // one means "no selection", which is the suppressing answer.
  const get = globalThis.getSelection;
  return typeof get === "function" ? get.call(globalThis) : null;
}
