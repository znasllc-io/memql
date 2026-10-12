// Every string the email pages show, in one place: the New email page (newEmail),
// the email editor (editor) and the messages the host raises for either (host).
//
// Pure: no vscode and no DOM, so the host and the view bundles share it. A
// string with a {placeholder} goes through fill(). The voice follows the UX
// record: sentence case, short, typographic apostrophes.

export const EMAIL_COPY = {
  newEmail: {
    title: "New email", organization: "Organization", layout: "Layout", builtIn: "Built in",
    name: "Name", namePlaceholder: "Autumn welcome", nameInvalid: "Use a name without slashes, up to 150 characters.",
    describe: "Describe it", optional: "Optional",
    describePlaceholder: "What should this email say? Include the audience, the offer and any links.",
    addReferences: "Add references or pictures", dictate: "Dictate the description", stopDictation: "Stop dictation",
    useInEmail: "Use in the email", removeReference: "Remove {name}",
    notConnected: "Not connected to {domain}", noOrganization: "No organization yet",
    noOrganizationLine: "Add an organization in MemQL OS to write emails.",
    chooseOrganization: "Choose an organization", chooseLayout: "Choose a layout", nameIt: "Name the email",
    ready: "Ready to write", writing: "Writing your email", failed: "Couldn’t write the email",
    layoutsFailed: "Couldn’t load the layouts", loading: "Loading layouts",
    connect: "Connect", start: "Start from the layout", write: "Write it", cancel: "Cancel", retry: "Retry",
  },
  editor: {
    design: "Design", source: "Source", desktop: "Desktop width", mobile: "Mobile width", plain: "Plain text",
    email: "Email", subject: "Subject", preheader: "Preheader", organization: "Organization", issues: "Issues", blocks: "Blocks",
    moveUp: "Move up", moveDown: "Move down", addAfter: "Add after", remove: "Remove block",
    words: "Words", label: "Label", link: "Link", description: "Description", picture: "Picture", noPicture: "No picture",
    chooseFromFiles: "Choose from Files", upload: "Upload", removePicture: "Remove",
    colour: "Colour", background: "Background", align: "Align", left: "Left", center: "Center", right: "Right",
    height: "Height", title: "Title", links: "Links", lines: "Lines", html: "HTML", network: "Network", addLink: "Add link",
    brand: "Same in every email", theme: "Theme", headingFont: "Heading font", bodyFont: "Body font",
    themeColours: { page: "Page", canvas: "Canvas", primary: "Primary", secondary: "Secondary", accent: "Accent", tint: "Tint", text: "Text", muted: "Muted", white: "White" },
    previewWith: "Preview with", sampleValue: "Sample value",
    sampleLabels: { displayName: "Name", email: "Email address", accountName: "Organization", campaignName: "Campaign" },
    htmlOnly: "Written in HTML", editHtml: "Edit HTML", plainOnly: "Plain text only",
    blockNames: { image: "Picture", heading: "Heading", text: "Text", highlight: "Highlight", button: "Button", divider: "Divider", spacer: "Space", section: "Section", columns: "Columns", social: "Social links", footer: "Footer", html: "HTML" },
    notConnected: "Not connected", notConnectedTo: "Not connected to {domain}", chooseOrganization: "Choose the organization",
    unreadable: "Can’t read this email", local: "On this computer",
    issuesOne: "Fix 1 issue to publish", issuesMany: "Fix {count} issues to publish",
    unsaved: "Unsaved changes", draft: "Draft", published: "Published to {organization}", changed: "Changes since publishing",
    publishing: "Publishing", savingLayout: "Saving the layout", savingCopy: "Saving to MemQL", layoutSaved: "Saved as a layout",
    connect: "Connect", openSource: "Open Source", saveToMemQL: "Save to MemQL", saveLayout: "Save as layout", publish: "Publish",
    layoutName: "Layout name", saveLayoutConfirm: "Save layout", cancel: "Cancel",
    renderFailed: "Couldn’t show this email.", invalidJson: "This email has invalid JSON. Open Source and correct its formatting.",
    emailPreview: "Email preview", removeLink: "Remove link", colourDefault: "Default",
    emailView: "Email view", previewWidth: "Preview width",
    buttonLabelEmpty: "Enter a label.", buttonLabelInvalid: "Enter a shorter label.", linkLabelInvalid: "Enter a link label up to 80 characters.",
    networkEmpty: "Enter a network name.", networkInvalid: "Enter a shorter network name.", tooManyLines: "Use up to 12 lines.", lineTooLong: "Use up to 300 characters in each line.",
    wordsLinkLabel: "Give each link a label, as in [label](address).",
    wordsLinkAddress: "Give each link a web address, an email address (mailto:), a phone number (tel:) or a link tag.",
  },
  host: {
    connectFirst: "Connect to your cluster in MemQL before writing an email.",
    addOrganization: "Add an organization in MemQL OS before writing an email.",
    addReferences: "Add references", fromComputer: "From this computer", fromFiles: "From Files",
    chooseReferences: "Choose references", addLabel: "Add", recentFiles: "Recent files in MemQL",
    tooMany: "Choose up to 64 references.", tooLarge: "References must total 16 MiB or less.",
    unsupported: "Choose pictures, text, Markdown, HTML, CSS, JSON, CSV, SVG or ZIP files.",
    choosePicture: "Choose a picture", recentPictures: "Recent pictures in Files", noPictures: "No pictures in Files yet. Upload one instead.",
    uploadPicture: "Upload a picture", usePicture: "Use picture", pictureTooLarge: "Choose a picture of 1 MiB or less.",
    saveBeforePublish: "Save the email before publishing.", changedWhilePublishing: "The email changed while publishing. Publish again.",
    fixIssues: "Fix the issues shown in the editor, then publish.", noLayoutForHtml: "An email written in HTML can’t be a layout.",
    layoutNameInvalid: "Enter a layout name up to 200 characters.", editsNeedValidJson: "Fix the email in Source first.",
  },
} as const;

const PLACEHOLDER = /\{([A-Za-z][A-Za-z0-9]*)\}/g;

/**
 * Fills the {name} placeholders of a copy string. A placeholder with no value
 * stays as written, so a missing value is visible rather than blank. One pass:
 * a value is never read as another placeholder.
 */
export function fill(template: string, values: Record<string, string | number>): string {
  return template.replace(PLACEHOLDER, (placeholder, name: string) => Object.hasOwn(values, name) ? String(values[name]) : placeholder);
}
