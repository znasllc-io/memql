import { useRef, type ComponentProps } from "react";
import { DialogFrame } from "./DialogFrame";
import { PageHelpProvider } from "./pageHelp";
import { PageHelpButton } from "./PageHelpButton";

/** A modal owns its documentation: the page behind it is inert. The help
 * dialog uses DialogFrame directly, so it cannot grow another help entry. */
export function Dialog(props: Omit<ComponentProps<typeof DialogFrame>, "dialogRef" | "headerActions">) {
  const root = useRef<HTMLDialogElement>(null);
  return <PageHelpProvider root={root}>
    <DialogFrame {...props} dialogRef={root} headerActions={<PageHelpButton title={props.title} />} />
  </PageHelpProvider>;
}
