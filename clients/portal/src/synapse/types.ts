// Synapse's vocabulary: what a section tells the model about itself.
//
// A SCOPE is a piece of the interface that can be filled in -- a form, an
// editor, a composer. It says what it is in words a person would recognise,
// lists the fields it will accept a value for, and hands back a function that
// applies them. Nothing else. It does not say what page it is on, it does not
// pass a submit handler, and it cannot be asked to.

export type SynapseFieldType = "text" | "number" | "boolean" | "enum";

export interface SynapseField {
  // The key a patch names. Must match what `apply` expects.
  readonly name: string;
  readonly type: SynapseFieldType;
  // What the field is called on screen. The model matches a person's words
  // against this far more often than against `name`.
  readonly label?: string;
  // What is in the field RIGHT NOW, as text. Present so an empty prompt is a
  // no-op rather than a rewrite, and so "make it shorter" has something to be
  // shorter than.
  readonly value?: string;
  // The allowed members, for `enum`. A patch naming anything else is dropped.
  readonly options?: readonly string[];
  // Anything else worth knowing in one clause: "comma separated", "a
  // hostname", "hours". Free text, shown to the model, never parsed.
  readonly constraints?: string;
}

export interface SynapsePatch {
  readonly field: string;
  // Coerced to the field's declared type by the time a scope sees it.
  readonly value: string | number | boolean;
}

export interface SynapseScope {
  // Stable, and the key the token average is kept under. A form that changes
  // its id between renders loses its own spend history.
  readonly id: string;
  readonly label: string;
  readonly fields: readonly SynapseField[];
  // APPLIES the patches to the caller's own draft state. It may not submit,
  // navigate, or write to the cluster -- that is the hard rule of the whole
  // affordance, and it is expressed here as the absence of anything that
  // could: the scope hands over a setter, not a form.
  readonly apply: (patches: readonly SynapsePatch[]) => void;
}
