// The language reference page: what it shows, what it refuses to invent, and
// what it says when it cannot ask a cluster.
//
// THE PAGE'S JOB IS TO BE CHECKABLE. Everything on it comes from one of two
// places -- a cluster's own `memqlGrammar()` / `memqlVocabulary()` reply, or
// this extension's own pin -- and the page has to say which, for every fact.
// The cases below are that claim, one fact at a time: a grammar that renders
// as productions rather than as a wall of text, a vocabulary grouped by the
// kinds the cluster named, an edition status that is printed when this
// extension can vouch for it and WITHHELD when it cannot, and a
// no-connection state that says what the extension knows instead of an empty
// pane.
//
// AND THE ESCAPING, which matters more here than anywhere else in this tree:
// half the words on this page are `<production>`, `@annotation` and `&&`. A
// vocabulary entry named `<b>` must arrive as text.
//
// Refs: memql#5388 (the two builtins), memql#5362 (the pin)

import test from "node:test";
import assert from "node:assert/strict";

import type { Row } from "@znasllc-io/memql-sdk-core/client";

import {
  GRAMMAR_CALL,
  VOCABULARY_CALL,
  clusterLanguage,
  grammarFromRows,
  grammarSections,
  languageIdentity,
  languagePin,
  matchesSearch,
  productionCount,
  vocabularyByKind,
  vocabularyFromRows,
  vocabularyText,
  type LanguagePin,
  type VocabularyArtifact,
} from "../src/state/languageReference.js";
import {
  identityMeta,
  renderLanguageReferencePage,
  type LanguageReferenceInput,
} from "../src/webview/languageReferenceScreens.js";

// A slice of a real grammar: the header comment, two banners, a wrapped
// production and a plain one. Every shape the block parser has to handle.
const GRAMMAR = `(* MemQL authoring grammar. Edition 2026, grammar version 2026.09-example-0123abcd.
   GENERATED from the parser -- do not edit. *)

(* ---- A file ---- *)
<file>                ::= <use>* <declaration>*
<declaration>         ::= <automation> | <concept> | <logic>
                        | <mutation> | <query>

(* ---- Expressions ---- *)
<expr-5>              ::= <expr-4> { "??" <expr-4> }
<expr-2>              ::= ( "!" | "-" ) <expr-2> | <expr-1>
`;

const PIN: LanguagePin = {
  edition: "2026",
  status: "frozen",
  grammarVersion: "2026.09-example-0123abcd",
  version: "0.6.0",
};

function grammarArtifact(): NonNullable<ReturnType<typeof grammarFromRows>> {
  const artifact = grammarFromRows([
    {
      id: "memql:grammar",
      concept: "memql:grammar",
      format: "ebnf",
      edition: "2026",
      grammarVersion: "2026.09-example-0123abcd",
      content: GRAMMAR,
    },
  ]);
  assert.ok(artifact !== undefined);
  return artifact;
}

function vocabularyArtifact(entries?: Record<string, unknown>[]): VocabularyArtifact {
  const artifact = vocabularyFromRows([
    {
      id: "memql:vocabulary",
      concept: "memql:vocabulary",
      edition: "2026",
      grammarVersion: "2026.09-example-0123abcd",
      kinds: ["construct", "operator", "function"],
      entries: entries ?? [
        {
          kind: "construct",
          name: "query",
          signature: "query <concept-name> <name> { ... }",
          description: "Read function: a bound concept, a filter and a projection.",
        },
        {
          kind: "operator",
          name: "??",
          signature: "a ?? b",
          description: "Blank-coalescing: falls through on an absent OR whitespace-only value.",
        },
        {
          kind: "function",
          name: "lower",
          signature: "lower(s)",
          description: "Lowercases a string.",
          tier: "P",
        },
      ],
    },
  ]);
  assert.ok(artifact !== undefined);
  return artifact;
}

function page(over: Partial<LanguageReferenceInput> = {}): string {
  return renderLanguageReferencePage({
    identity: languageIdentity({
      pin: PIN,
      cluster: clusterLanguage("local", { edition: "2026", grammarVersion: PIN.grammarVersion }),
    }),
    grammar: grammarArtifact(),
    vocabulary: vocabularyArtifact(),
    loading: false,
    error: "",
    search: "",
    offerSelectCluster: true,
    ...over,
  });
}

// -----------------------------------------------------------------------------
// The two artifacts render
// -----------------------------------------------------------------------------

test("the grammar renders as the cluster's own sections and productions", () => {
  const html = page();

  // The banners become the grouping, so the page's structure is the
  // artifact's structure rather than one invented here.
  assert.ok(html.includes(">A file<"), "the `(* ---- A file ---- *)` banner is not a heading");
  assert.ok(html.includes(">Expressions<"), "the second banner is not a heading");

  // A WRAPPED production stays whole. Filtering by line would show
  // `<declaration> ::= <automation> | <concept> | <logic>` and hide the
  // continuation, which is a grammar that is wrong rather than short.
  const sections = grammarSections(GRAMMAR);
  const declaration = sections
    .flatMap((s) => s.blocks)
    .find((b) => b.name === "declaration");
  assert.equal(declaration?.lines.length, 2, "the wrapped alternation was split across blocks");

  assert.equal(productionCount(sections), 4, "four productions, counted for the page's header");
  assert.ok(html.includes("4 productions"), "the grammar's own count is not on the page");
});

test("the vocabulary renders grouped by the kinds the cluster named", () => {
  const html = page();
  for (const kind of ["construct", "operator", "function"]) {
    assert.ok(html.includes(`>${kind} <`), `no ${kind} group`);
  }
  assert.ok(html.includes("3 entries"), "the vocabulary's own count is not on the page");
  assert.ok(html.includes("Lowercases a string."), "a description did not render");
  // The tier is a word, with its meaning in the tooltip -- not "P -- lowers
  // to SQL" in a badge.
  assert.ok(html.includes('title="Lowers to SQL in the database">SQL<'), "a catalog function's tier did not render");
  assert.doesNotMatch(html, /P -- lowers/);
});

test("a kind the cluster did not name in `kinds` is still rendered", () => {
  // A client that dropped an entry because it had not heard of its kind would
  // silently under-report the language, which is the one thing this page
  // exists to be trusted about.
  const artifact = vocabularyArtifact([
    { kind: "somethingNew", name: "whatsit", signature: "whatsit()", description: "New." },
  ]);
  const groups = vocabularyByKind(artifact);
  assert.deepEqual(
    groups.map((g) => g.kind),
    ["somethingNew"],
  );
  assert.ok(page({ vocabulary: artifact }).includes("whatsit"));
});

test("both copy acts are offered, as the head's quiet acts, and say they copy all of it", () => {
  const html = page();
  assert.ok(html.includes('data-act="copyGrammar"'), "no grammar copy act");
  assert.ok(html.includes('data-act="copyVocabulary"'), "no vocabulary copy act");
  // Copy takes the WHOLE artifact, and the act's tooltip says so -- not a
  // standing paragraph over the lists.
  assert.match(html, /title="Copies the whole grammar, whatever the search shows"/);
  assert.doesNotMatch(html, /Copy takes the whole artifact/);
  // An artifact that is not on the page has no copy act: absent, not refused.
  assert.equal(page({ grammar: undefined }).includes('data-act="copyGrammar"'), false);
});

test("the copied vocabulary names the language it describes, and separates on tabs", () => {
  const text = vocabularyText(vocabularyArtifact());
  const lines = text.trimEnd().split("\n");
  assert.match(lines[0] ?? "", /Edition 2026, grammar version 2026\.09-example-0123abcd/);
  assert.match(lines[0] ?? "", /3 entries/);
  // Descriptions carry `|`, `,` and `:`; a tab is the one separator none of
  // them holds, so a reader can split a line back into its fields.
  const entry = lines.find((line) => line.startsWith("operator\t"));
  assert.ok(entry !== undefined, "the `??` entry is not in the copied text");
  assert.equal(entry.split("\t").length, 5, "an entry is five tab-separated fields");
  assert.ok(entry.includes("??"), "the operator's own name is missing");
});

// -----------------------------------------------------------------------------
// Search
// -----------------------------------------------------------------------------

test("only a production is counted against a count that says productions", () => {
  // THE DEFECT THIS FAILS AGAINST: the counter counted every searchable item,
  // and the grammar's own comment blocks are searchable without being
  // productions -- so a term matching only the header read "1 of 126
  // productions" with no production on the screen at all.
  const html = page();
  assert.equal((html.match(/data-search="/g) ?? []).length, 8, "searchable items");
  assert.equal(
    (html.match(/data-tally/g) ?? []).length,
    7,
    "the grammar's header comment is being counted as a production",
  );
  // A term matching ONLY the header comment shows it and counts no production.
  const header = page({ search: "GENERATED from the parser" });
  assert.match(header, /0 of 4 productions/);
  assert.doesNotMatch(header, /Nothing in the grammar matches/, "the page says nothing matches over a visible match");
});

/**
 * Every searchable item's index, as the BROWSER sees it.
 *
 * The attribute is HTML-escaped on the way out -- half these indexes are full
 * of `<`, `>` and `&` -- and `dataset.search` hands the page back the decoded
 * string. Decoding here is what makes these cases exercise the value the
 * script's `includes` actually runs against, rather than its wire form.
 */
function indexes(html: string): string[] {
  return [...html.matchAll(/data-search="([^"]*)"/g)].map((m) =>
    (m[1] ?? "")
      .replace(/&lt;/g, "<")
      .replace(/&gt;/g, ">")
      .replace(/&quot;/g, '"')
      .replace(/&#39;/g, "'")
      .replace(/&amp;/g, "&"),
  );
}

test("every item carries the text a search matches it by", () => {
  const html = page();
  const all = indexes(html);
  // Four productions, the grammar's own header comment, and three vocabulary
  // entries. The two `(* ---- … ---- *)` banners are the SECTIONS rather than
  // items in them, so they are not counted here.
  assert.equal(all.length, 8, `unexpected searchable item count: ${all.length}`);

  // A vocabulary entry is searchable by its name, how it is written, what it
  // means AND its kind -- four different ways a reader might come at it.
  const coalesce = all.find((index) => index.includes("blank-coalescing"));
  assert.ok(coalesce !== undefined, "no entry is searchable by its description");
  for (const term of ["??", "a ?? b", "operator", "whitespace-only"]) {
    assert.ok(matchesSearch(coalesce, term), `"${term}" does not reach the ?? entry`);
  }
});

test("a search filters, and an empty one shows everything", () => {
  // The rule, over the indexes the page emitted.
  const all = indexes(page());
  const hits = (term: string): number => all.filter((index) => matchesSearch(index, term)).length;

  assert.equal(hits(""), all.length, "an empty term must show everything");
  assert.equal(hits("   "), all.length, "a term of spaces is an empty term");
  assert.ok(hits("expr-5") > 0 && hits("expr-5") < all.length, "a term must actually narrow");
  assert.equal(hits("QUERY"), hits("query"), "the search is case-insensitive");
  // A production WRAPS, so its index collapses whitespace: a reader searching
  // for two adjacent words of a line the page is showing them must not be told
  // the grammar does not contain it.
  assert.equal(hits("<logic> | <mutation>"), 1, "a term spanning a wrapped line found nothing");
  assert.equal(hits("no-such-thing-anywhere"), 0, "a nonsense term must match nothing");
});

test("the page the host draws for a term hides exactly what the rule rejects", () => {
  // THE HOST DRAWS THE SEARCH now (patched in place, so the box keeps its
  // caret), with the same rule the cases above exercise -- so the two cannot
  // disagree about what a term shows.
  const html = page({ search: "expr-5" });
  const visible = [...html.matchAll(/data-search="([^"]*)"(?: data-tally)?(\s+hidden)?>/g)].filter((m) => m[2] === undefined);
  assert.equal(visible.length, 1, "the page shows a different set from the rule");
  assert.match(html, /1 of 4 productions/);
  assert.match(html, /0 of 3 entries/);
  // A group with nothing left in it is hidden with its heading.
  assert.match(html, /<section class="lr-group" hidden><h3 class="lr-group-head">A file<\/h3>/);
  // Every group's own count follows the search too.
  assert.match(html, />construct <span class="mq-head-meta">0 of 1</);
});

test("a term that matches nothing says so, naming the term, as text", () => {
  const html = page({ search: '<nope> "x"' });
  assert.match(html, /Nothing in the grammar matches &quot;&lt;nope&gt; &quot;x&quot;&quot;\./);
  assert.match(html, /Nothing in the vocabulary matches/);
  // And without a term there is no such line at all.
  assert.doesNotMatch(page(), /Nothing in the/);
});

test("the search box sits above both lists, and keeps its term across a repaint", () => {
  const html = page({ search: "query" });
  const search = html.indexOf('id="lr-search"');
  const grammar = html.indexOf('data-scope="grammar"');
  const vocabulary = html.indexOf('data-scope="vocabulary"');
  assert.ok(search > -1 && search < grammar && grammar < vocabulary, "the page order moved");
  assert.match(html, /data-field="search" value="query"/);
});

// -----------------------------------------------------------------------------
// Escaping
// -----------------------------------------------------------------------------

test("a name that looks like markup renders as text, everywhere it appears", () => {
  const html = page({
    vocabulary: vocabularyArtifact([
      {
        kind: "<kind>",
        name: "<b>",
        signature: '<img src=x onerror="1">',
        description: "</p><script>alert(1)</script>",
      },
    ]),
  });
  // Both directions, so the case cannot pass vacuously: the raw markup must
  // not survive, and the escaped form must be what renders.
  assert.equal(html.includes("<b>"), false, "a raw <b> reached the page");
  assert.equal(html.includes("<img"), false, "a raw <img> reached the page");
  assert.equal(html.includes("<script>alert"), false, "a raw <script> reached the page");
  assert.ok(html.includes("&lt;b&gt;"), "the escaped name did not render");
  assert.ok(html.includes("&lt;script&gt;alert(1)&lt;/script&gt;"), "the escaped description did not render");
  // And in the ATTRIBUTE the search reads, where an unescaped quote would end
  // the attribute and start whatever came next.
  assert.equal(html.includes('onerror="1"'), false, "a raw quote survived into data-search");
});

test("an ampersand survives as an ampersand, and cannot become a tag by a second pass", () => {
  // `&` IS THE CASE THE FIRST ROUND MISSED, and the language is full of it:
  // `<expr-7>` is written `<expr-6> { "&&" <expr-6> }`. Two claims here, and
  // the second is the one that bites: an entity is escaped as text, so a
  // description that literally reads `&lt;b&gt;` renders those nine characters
  // rather than a bold tag somebody typed their way into.
  const html = page({
    vocabulary: vocabularyArtifact([
      {
        kind: "operator",
        name: "&&",
        signature: "a && b",
        description: "Both. Written &lt;b&gt; by nobody, and A&B by somebody.",
      },
    ]),
  });
  assert.ok(html.includes("&amp;&amp;"), "the operator's name did not render escaped");
  assert.ok(html.includes("a &amp;&amp; b"), "the signature did not render escaped");
  assert.ok(html.includes("A&amp;B"), "a bare ampersand in prose did not render escaped");
  assert.ok(
    html.includes("&amp;lt;b&amp;gt;"),
    "an entity in the text was passed through, so a description can smuggle a tag",
  );
  assert.equal(html.includes("&lt;b&gt;"), false, "the text's own entity reached the page live");
  // And in the search index, which is the same string a second time.
  const index = indexes(html).find((candidate) => candidate.includes("a && b"));
  assert.ok(index !== undefined, "the index lost the ampersands on the way through the attribute");
});

test("the grammar's angle brackets survive as text", () => {
  const html = page();
  assert.ok(html.includes("&lt;file&gt;"), "a production name did not render escaped");
  assert.equal(
    /<file>/.test(html),
    false,
    "a production name reached the page as a tag",
  );
});

// -----------------------------------------------------------------------------
// Which language, and on whose authority
// -----------------------------------------------------------------------------

test("the page names the edition, its status and the grammar version, in one line", () => {
  const html = page();
  assert.equal(
    identityMeta(languageIdentity({ pin: PIN, cluster: clusterLanguage("local", { edition: "2026", grammarVersion: PIN.grammarVersion }) })),
    "Edition 2026 · Frozen · Grammar 2026.09-example-0123abcd · from local",
  );
  assert.ok(html.includes("Edition 2026 · Frozen · Grammar 2026.09-example-0123abcd · from local"));
  // No notes beside every fact.
  assert.doesNotMatch(html, /Read from the cluster|Recorded by this extension/);
});

test("with no cluster the line says the language is the editor's own", () => {
  assert.equal(identityMeta(languageIdentity({ pin: PIN })), "Edition 2026 · Frozen · Grammar 2026.09-example-0123abcd · built in");
});

test("the status is WITHHELD for an edition this extension does not record", () => {
  // The one fact on the page a reader cannot check against the artifacts
  // below it, so it is stated only where this extension can vouch for it.
  const identity = languageIdentity({
    pin: PIN,
    cluster: clusterLanguage("prod", { edition: "2027", grammarVersion: "2027.01-next-89abcdef" }),
  });
  assert.equal(identity.status, "");
  assert.match(identity.statusNote, /records the status of edition 2026 only/);
  assert.match(identity.statusNote, /this cluster speaks edition 2027/);

  const html = page({ identity, pin: PIN });
  assert.ok(html.includes("Edition 2027 · Grammar 2027.01-next-89abcdef · from prod"), "the status was not withheld");
  // The difference is said once, when there is one.
  assert.match(html, /prod speaks edition 2027; this editor speaks 2026\./);
  assert.doesNotMatch(page({ pin: PIN }), /speaks edition/);
});

test("a pin with no status leaves it out of the line rather than printing a blank", () => {
  const identity = languageIdentity({ pin: { ...PIN, status: "" } });
  assert.equal(identity.status, "");
  assert.match(identity.statusNote, /records no status for edition 2026/);
  assert.equal(identityMeta(identity), "Edition 2026 · Grammar 2026.09-example-0123abcd · built in");
});

test("the artifacts' own edition wins over the handshake's", () => {
  // The header labels what is ON THE PAGE. A grammar rendered from one version
  // under a header naming another is labelling the wrong thing.
  const resolved = clusterLanguage(
    "local",
    { edition: "2026", grammarVersion: "2026.08-older-00000000" },
    grammarArtifact(),
  );
  assert.equal(resolved.grammarVersion, "2026.09-example-0123abcd");
  assert.equal(resolved.name, "local");
});

// -----------------------------------------------------------------------------
// No cluster
// -----------------------------------------------------------------------------

test("with no connection the page says so in one line, and offers the act", () => {
  const identity = languageIdentity({ pin: PIN });
  const html = renderLanguageReferencePage({
    identity,
    loading: false,
    error: "",
    search: "",
    offerSelectCluster: true,
  });

  assert.match(html, /Connect to a cluster to see its grammar and vocabulary\./);
  // No doctrine paragraphs.
  assert.doesNotMatch(html, /generates both from its own parser/);
  // What the extension DOES know is in the head's line.
  assert.ok(html.includes("Edition 2026 · Frozen · Grammar 2026.09-example-0123abcd · built in"));
  // And a way out -- not a spinner, and not an empty pane.
  assert.ok(html.includes('data-act="selectCluster"'), "no way to connect from here");
  assert.equal(html.includes("lr-search"), false, "a search box over nothing");
  assert.equal(html.includes("data-search"), false, "searchable items with no artifacts");
});

test("an untrusted window is offered workspace trust instead of a button that cannot work", () => {
  // The command is contributed but never registered in a restricted folder, so
  // Select a cluster would promise a click that fails with "command not found".
  const html = renderLanguageReferencePage({
    identity: languageIdentity({ pin: PIN }),
    loading: false,
    error: "",
    search: "",
    offerSelectCluster: false,
  });
  assert.equal(html.includes('data-act="selectCluster"'), false, "a button that cannot work");
  assert.match(html, /Trust this workspace to connect to a cluster\./);
  assert.ok(html.includes('data-act="manageTrust"'));
});

test("reading is the shape of the content, never a sentence", () => {
  const html = page({ grammar: undefined, vocabulary: undefined, loading: true });
  assert.match(html, /mq-skeleton/);
  const visible = html.replace(/<span class="mq-sr">[^<]*<\/span>/g, "");
  assert.doesNotMatch(visible, /Reading/);
});

test("a failed read says what did not come back, why, and offers the retry", () => {
  const html = page({
    grammar: undefined,
    vocabulary: undefined,
    error: "It didn't answer within 20s.",
    loading: false,
    pin: PIN,
  });
  assert.match(html, /local didn&#39;t return a grammar or a vocabulary\./);
  assert.match(html, /It didn&#39;t answer within 20s\./);
  assert.equal(html.match(/data-act="reload"/g)?.length, 1, "exactly one Try again");
  // The head's meta already names this language, and the editor speaks the
  // same one: a second line saying so is the same fact twice.
  assert.equal(html.includes("This editor speaks"), false, "the editor's language is said twice");
  // And not on a page that has the artifacts: there it would be noise.
  assert.equal(page({ pin: PIN }).includes("This editor speaks"), false);
});

test("a failed read names the editor's language only when it is not the cluster's", () => {
  const failed = { grammar: undefined, vocabulary: undefined, error: "It didn't answer within 20s.", pin: PIN };
  // Same edition, another grammar: the one difference the meta cannot show.
  const otherGrammar = page({
    ...failed,
    identity: languageIdentity({
      pin: PIN,
      cluster: clusterLanguage("local", { edition: "2026", grammarVersion: "2026.10-other-89abcdef" }),
    }),
  });
  assert.match(otherGrammar, /This editor speaks edition 2026 \(frozen\), grammar 2026\.09-example-0123abcd\./);
  // A cluster that stated no edition: the editor's is all there is to go on.
  const unstated = page({
    ...failed,
    identity: languageIdentity({ pin: PIN, cluster: clusterLanguage("local", { edition: "", grammarVersion: "" }) }),
  });
  assert.match(unstated, /This editor speaks edition 2026/);
});

test("a successful read offers no retry, and a disconnected page offers none either", () => {
  assert.equal(page().includes('data-act="reload"'), false, "a retry over a page that worked");
  const offline = renderLanguageReferencePage({
    identity: languageIdentity({ pin: PIN }),
    loading: false,
    error: "",
    search: "",
    offerSelectCluster: true,
  });
  // There is nothing to retry: no cluster was asked. Select Cluster is the
  // action that applies, and it is the one drawn.
  assert.equal(offline.includes('data-act="reload"'), false);
  assert.ok(offline.includes('data-act="selectCluster"'));
});

test("one artifact arriving without the other is reported, not hidden", () => {
  const html = page({ vocabulary: undefined, error: "unknown builtin" });
  assert.match(html, /local didn&#39;t return a vocabulary\./);
  assert.match(html, /unknown builtin/);
  assert.ok(html.includes("4 productions"), "the grammar that DID arrive is not rendered");
});

// -----------------------------------------------------------------------------
// The wire
// -----------------------------------------------------------------------------

test("the two calls are the top-level builtin form", () => {
  // `builtin <name>()` is what the SDK's generated methods build; neither
  // builtin carries @sdk, so there is no generated method and the string is
  // written out. A different spelling reaches the engine and is refused.
  assert.equal(GRAMMAR_CALL, "builtin memqlGrammar()");
  assert.equal(VOCABULARY_CALL, "builtin memqlVocabulary()");
});

test("an empty or absent reply is no artifact, never an empty one", () => {
  // "This cluster has no grammar" is not a thing a MemQL cluster can be, so a
  // blank answer means the call did not answer what it was meant to -- and an
  // empty grammar pane would report that as a language with no productions.
  assert.equal(grammarFromRows([]), undefined);
  assert.equal(grammarFromRows([{ content: "   " } as Row]), undefined);
  assert.equal(vocabularyFromRows([]), undefined);
  assert.equal(vocabularyFromRows([{ entries: [] } as Row]), undefined);
  assert.equal(vocabularyFromRows([{ entries: "not a list" } as Row]), undefined);
  // An entry with no NAME is the one a reader could not act on, so it is
  // dropped; one with a name and nothing else survives, because an entry the
  // cluster describes badly is still an entry it has.
  const partial = vocabularyFromRows([
    { entries: [{ kind: "construct" }, { name: "query" }] } as Row,
  ]);
  assert.equal(partial?.entries.length, 1);
  assert.equal(partial?.entries[0]?.name, "query");
});

test("the pin reads what the manifest carries, and nothing it does not", () => {
  assert.deepEqual(languagePin(undefined), {
    edition: "",
    status: "",
    grammarVersion: "",
    version: "",
  });
  assert.deepEqual(languagePin({ memql: { edition: 2026 }, version: null }), {
    edition: "",
    status: "",
    grammarVersion: "",
    version: "",
  });
  assert.deepEqual(
    languagePin({ memql: { edition: "2026", status: "frozen", grammarVersion: "g" }, version: "1.2.3" }),
    { edition: "2026", status: "frozen", grammarVersion: "g", version: "1.2.3" },
  );
});
