import type { GuideEntry } from "./types";

// One entry per destination and per tab. The coverage gate walks the nav
// definition against this list, so the set here is not a matter of taste --
// a destination without an entry fails the build.
//
// VOICE (ui/README.md carries the rules): name what the person controls or
// sees; sentence case, plain verbs; no engine vocabulary above the
// "Technical details" line. Everything below that line is FOR engine
// vocabulary -- that is what it is.

export const GUIDE_ENTRIES: readonly GuideEntry[] = [
  {
    id: "console",
    title: "Console",
    body:
      "The landing view for this cluster: how many people, agents, accounts and " +
      "deployables it holds, what has been deployed lately, and what has happened. " +
      "Every tile is a door into the full surface behind it.",
    how: [
      "Each tile counts rows this cluster actually holds -- there is no sampling and no cache.",
      "The counts are what YOU can see. Somebody with a different role can land here and read different numbers, and both are correct.",
      "Clicking a tile opens the surface it counts, already scoped to the same thing.",
    ],
    technical: {
      concepts: [
        "v1:identity:user",
        "v1:agents:agent",
        "v1:identity:account",
        "v1:platform:site",
        "v1:cluster:deployment",
        "v1:identity:auditEvent",
      ],
      notes: ["Every tile reads through the same per-row authorization as the page it opens."],
    },
  },
  {
    id: "nexus",
    title: "Nexus",
    body:
      "One goal of yours, seen whole. Nexus shows a single piece of work -- what is " +
      "running on it, the specialists it raised, what it produced, and how it got " +
      "there -- as a map you can watch build itself and then replay.",
    how: [
      "Start from Goals: each card is one goal you asked for, with its phase and its state.",
      "Map draws the goal's world; Constructs lists what it authored; Replay walks the same world back through its own timestamps.",
      "The replay is built from the rows' recorded times, so a moment with no timestamp produces no event -- nothing here is invented to fill a gap.",
      "The map needs WebGL. Without it the page still reads the goal out in text rather than showing you a blank canvas.",
    ],
    technical: {
      concepts: ["v1:planner:plan", "v1:planner:task", "v1:agents:agent", "v1:authoring:construct"],
      docs: ["docs/public/operate/portal.md"],
      notes: [
        "A goal is filtered to the caller's own requestedBy in the client today; the residual is recorded in docs/public/operate/auth/per-row-authz-audit.md.",
      ],
    },
  },
  {
    id: "views",
    title: "Views",
    body:
      "Screens over this cluster's data. Five ship with the product -- the people, " +
      "the agents, the accounts, what is deployed and what happened -- and the rest " +
      "are ones you composed. A view is a layout over rows, so a new view costs a " +
      "choice of arrangement rather than a release.",
    how: [
      "Open a card to read that population three ways: how many there are, how it divides, and which ones specifically.",
      "New view opens the composer: pick the concepts, accept or rearrange the layout it proposes, name it and save.",
      "Your composed views are yours -- nobody else's list changes when you save one.",
      "Archiving a view takes it off this page without touching a single row of the data it showed.",
    ],
    technical: {
      concepts: ["v1:portalviews:view"],
      notes: [
        "Composed views filter on ownerUserId == actor.userId in the engine; the browser does no owner check of its own.",
      ],
    },
  },
  {
    id: "concepts",
    title: "Concepts",
    body:
      "Every kind of thing this cluster can store, and the rows of each. Whatever " +
      "the cluster declares appears here the moment it is declared -- including " +
      "anything a product bundle brought with it. Nothing on this page is written " +
      "per concept.",
    how: [
      "Search and the domain filter live in the address, so a narrowed registry is a link you can send.",
      "Open one to walk its rows, or switch to Schema to read the fields it declares.",
      "Rows page as you scroll. If paging stops, what is listed is what arrived -- the page says so rather than looking finished.",
    ],
    technical: {
      notes: [
        "The registry arrives as a live delta stream, so a concept declared while you are looking appears without a reload.",
      ],
      docs: ["docs/public/concepts/identifiers.md"],
    },
  },
  {
    id: "concepts.modules",
    title: "Modules",
    body:
      "The packs this cluster has mounted, and whether each one is switched on. A " +
      "pack brings its own concepts, tools and automations; disabling it leaves its " +
      "rows readable and takes its behaviour out of the running system.",
    how: [
      "Open a module to see what it declares and how much of it is being used.",
      "Enable and disable take effect as each node restarts -- a node keeps whatever it loaded at boot.",
      "Disabled is not deleted. Rows stay readable; the tools, queries and automations are simply absent.",
    ],
    technical: {
      docs: ["docs/public/concepts/component-integration-pack.md"],
      notes: ["Owner and admin only -- every read behind this page is refused below that."],
    },
  },
  {
    id: "fleet.machines",
    title: "Machines",
    body:
      "The computers registered to this cluster as workers -- yours, and (if you " +
      "are an operator) everybody's. A machine is somebody's own computer, reached " +
      "over a connection it opened outward, and its owner can revoke it at any time.",
    how: [
      "Add a machine mints a pairing token; run the command it gives you on the computer you want to reach.",
      "Online is derived from the machine's own heartbeat, not from a stored flag -- a laptop that closed its lid goes quiet within about thirty seconds.",
      "Labels the machine reports are overwritten every time it reconnects. Labels YOU set are yours and survive that.",
      "Revoking cuts the machine off immediately. Pair it again to bring it back.",
      "Routing decides which machine a piece of work lands on; a call's record says which ones were considered and why each was passed over.",
    ],
    technical: {
      concepts: ["v1:worker:registration", "v1:worker:routingPolicy", "v1:worker:invocation"],
      env: ["WORKER_INVOCATION_RETENTION_DAYS"],
      docs: ["docs/public/operate/workers-runbook.md"],
      notes: [
        "Online window is 30s -- twice the cockpit's own 15s heartbeat flush.",
        "A machine's stream terminates on one agent replica; dispatches to it are forwarded to that replica.",
      ],
    },
  },
  {
    id: "fleet.apps",
    title: "Local apps",
    body:
      "Handing a task to an app you already pay for, on a machine you own. This " +
      "page decides when the planner is allowed to do that, and shows the " +
      "transcript of every run that resulted. Which apps a machine actually has is " +
      "on its card under Machines.",
    how: [
      "Delegation is a preference with a fallback: if no machine with an allowed, signed-in app is online, the work runs here instead. A plan never waits for a laptop to wake up.",
      "The app reads your data as YOU for the length of the run, so it sees exactly what you would see and nothing more.",
      "Spend on a subscription you already pay for is counted and reported, but it does not come out of a plan's money budget.",
      "Open a run to read its transcript as it happens.",
    ],
    technical: {
      concepts: ["v1:worker:delegationPolicy", "v1:worker:appSession", "v1:router:call"],
      docs: ["docs/public/operate/local-apps.md"],
      notes: [
        "The back-channel credential is minted with tokenClass=\"app_session\", capped at 8h, and is not revocable -- the short lifetime is what stands in for revocation.",
      ],
    },
  },
  {
    id: "fleet.workbenches",
    title: "Workbenches",
    body:
      "The cluster's own sandboxed working directories. When an agent needs to " +
      "write a file or run a command, this is where it happens by default -- inside " +
      "the cluster, on nothing belonging to anybody. Each goal gets its own " +
      "directory and it is thrown away when the goal ends.",
    how: [
      "A workspace is created the first time a goal needs one and released when the goal finishes.",
      "A workspace is a directory on one replica. If that replica goes away the workspace is released and a fresh one is made -- files are not carried across, because there is nothing to carry them from.",
      "Work that a sandbox genuinely cannot do (something needing a screen, a Mac-only tool, or files already on your computer) is offered to your machines instead, and says so.",
    ],
    technical: {
      concepts: ["v1:workbench:workspace"],
      env: ["MEMQL_WORKBENCH_ROOT", "MEMQL_WORKBENCH_REMOTE", "MEMQL_WORKBENCH_LOCAL_FALLBACK"],
      docs: ["docs/public/operate/workbench-runbook.md"],
      notes: [
        "MEMQL_WORKBENCH_REMOTE is an assertion, not a preference: with it set and no reachable workbench peer, a call is refused rather than run on the agent's own disk.",
      ],
    },
  },
  {
    id: "library.artifacts",
    title: "Artifacts",
    body:
      "Everything this cluster has indexed for you: files you uploaded, outputs it " +
      "generated, and the notes, to-dos, calendar events and memories your agents " +
      "made. Label one to say what it was for -- a label is a filter here too.",
    how: [
      "Upload a file and it is indexed automatically; it is searchable by meaning within moments.",
      "Search by meaning matches what a document is about rather than the words in it, so a good match may share no vocabulary with what you typed.",
      "Export downloads the original bytes through the cluster, under your own identity -- there is no shareable link to leak.",
      "Archiving takes an artifact off this list and keeps the row, so the trail of what existed stays intact.",
    ],
    technical: {
      concepts: ["v1:library:artifact", "v1:library:file", "v1:library:fileChunk"],
      env: ["MEMQL_LIBRARY_MAX_UPLOAD_BYTES"],
      docs: ["docs/public/operate/library.md"],
      notes: ["Upload and export are the two HTTP routes the portal uses; everything else rides the stream."],
    },
  },
  {
    id: "library.deployables",
    title: "Deployables",
    body:
      "What you have put on the internet from here. A deployable is a hostname " +
      "plus whatever you last deployed to it; you can roll it back to any earlier " +
      "version, take it offline, or delete it outright.",
    how: [
      "A new deployable starts as a draft: its hostname answers Not Found until you set it live.",
      "Deploy takes a bundle out of your Library and makes it the current version. The switch is atomic -- nobody ever sees a half-deployed site.",
      "Every version stays listed, so rolling back is choosing an earlier one rather than deploying again.",
      "In the cloud a freshly deployed hostname gets its certificate on its own; locally it is covered by the development wildcard.",
    ],
    technical: {
      concepts: ["v1:platform:site"],
      docs: ["docs/public/operate/deployables.md", "docs/public/operate/front-door.md"],
      notes: [
        "The hostname is a single label under the cluster's domain, checked against a reserved set derived from the front door's own roles.",
      ],
    },
  },
  {
    id: "cluster.integrations",
    title: "Integrations",
    body:
      "What this cluster has wired to the outside world, and whether each " +
      "connection is actually working. Registration and health are separate facts " +
      "and this page keeps them separate -- something can be configured and still " +
      "be down.",
    how: [
      "The list is what this node has registered. The live check dials each one and reports what came back.",
      "If the live check cannot run, what you are reading is registration only, and the page says so rather than implying health.",
      "Email settings are editable here; email credentials show only whether they are present -- no secret is ever sent to this page.",
    ],
    technical: {
      env: ["AZURE_TENANT_ID", "AZURE_CLIENT_ID", "MAIL_SENDER", "MAIL_FROM_NAME", "MEMQL_EMAIL_ALLOW_LOG_ONLY"],
      docs: ["docs/public/operate/inbound-delivery.md"],
    },
  },
  {
    id: "cluster.data-origins",
    title: "Data origins",
    body:
      "Which data this cluster owns, which it mirrors from somewhere else, and " +
      "which it pushes back out. A mirror is read-only here by construction: edits " +
      "belong at the system that owns the data, and this cluster keeps a faithful " +
      "copy of it.",
    how: [
      "Each row is one kind of thing plus the system it comes from, with how far behind the copy is.",
      "Backfill reads the whole set again; reconcile repairs what live updates lost. Pause stops both without losing what has been copied.",
      "An attempted write to mirrored data is refused, even for a cluster owner -- the next sync would undo it anyway, so accepting it would be a change that appears to work and does not last.",
    ],
    technical: {
      concepts: ["v1:platform:dataOrigin", "v1:platform:outboundRequest"],
      docs: ["docs/public/concepts/data-origins.md"],
      notes: ["Cluster-owner only: every read and action behind this page is refused below that."],
    },
  },
  {
    id: "cluster.stores",
    title: "Stores",
    body:
      "The Shopify stores this cluster mirrors. Shopify owns the data; this " +
      "cluster holds a generated copy kept current by webhooks and repaired by " +
      "reconciliation. A store's credentials live as references to sealed secrets " +
      "-- the row itself never carries a token.",
    how: [
      "Add a store after creating its app on Shopify's side and sealing the three credentials as secrets.",
      "Open a store for its scopes, its webhook subscriptions, and how far each kind of data has drifted.",
      "Backfill, reconcile and pause act per store and report what they did.",
    ],
    technical: {
      concepts: ["v1:shopify:store"],
      docs: [
        "docs/public/operate/shopify-connector.md",
        "docs/public/operate/shopify-storefront-checklist.md",
      ],
      notes: ["Cluster-owner only."],
    },
  },
  {
    id: "cluster.providers",
    title: "AI providers",
    body:
      "Which models this cluster can call, and how it proves who it is to them. " +
      "Nothing here is needed to install or run the cluster -- configure it when " +
      "you want agents to think.",
    how: [
      "A provider is a vendor plus a model plus a way to authenticate. Installing one spends no inference of its own.",
      "What is listed is what the node that answered this read has resolved, which is the honest answer for a cluster whose replicas can be mid-rollout.",
      "In the cloud the credential can be federated, so no long-lived vendor key sits at rest anywhere.",
    ],
    technical: {
      env: ["MEMQL_AI_OPENAI_API_KEY", "MEMQL_AI_ANTHROPIC_API_KEY"],
      docs: [
        "docs/public/operate/auth/anthropic-federation.md",
        "docs/public/ai/llm-cost-control.md",
      ],
      notes: ["Cluster-owner only -- the engine's own builtins refuse below owner regardless of what this page offers."],
    },
  },
  {
    id: "cluster.tokens",
    title: "Sessions and tokens",
    body:
      "Every long-lived credential issued against this cluster and who holds it. " +
      "Revoke one the moment it is out of its owner's hands.",
    how: [
      "Revoking takes effect on the next request the credential makes -- there is no grace period.",
      "Every revoke is recorded, and the record's id is shown to you so you can quote it later.",
      "Node tokens are listed separately because they belong to the cluster's own machinery rather than to a person.",
    ],
    technical: {
      concepts: ["v1:identity:identity", "v1:identity:authSession", "v1:identity:auditEvent"],
      docs: ["docs/public/operate/auth/access-model.md"],
    },
  },
  {
    id: "cluster.keys",
    title: "Signing keys",
    body:
      "The keys this cluster publishes so its nodes can verify each other's " +
      "access tokens, and when they were last rotated. Every node fetches this set " +
      "rather than holding the private half.",
    how: [
      "The published set is what every node verifies against. Replicas that disagree about it fail roughly half of all sign-ins, so this page is the first place to look when sign-in is intermittent.",
      "Rotation is not a browser action here -- the cluster publishes no call for it. The page says where it lives instead of hiding a capability you have.",
    ],
    technical: {
      env: ["MEMQL_IDENTITY_BASE_URL", "MEMQL_IDENTITY_VERIFIER_BASE_URL"],
      docs: ["docs/public/operate/auth/identity-service.md"],
      notes: ["The published set is served at /.well-known/jwks.json by the identity node."],
    },
  },
  {
    id: "cluster.settings",
    title: "Cluster settings",
    body:
      "The settings in force right now: who may register, how long a credential " +
      "lives, and how this cluster names itself.",
    how: [
      "A change here applies to the whole cluster and is recorded with who made it.",
      "Settings that are fixed at boot are not editable from a browser; they are shown so you can see what is in force.",
    ],
    technical: {
      env: ["MEMQL_DOMAIN", "MEMQL_IDENTITY_ENABLED"],
      docs: ["docs/public/operate/env-vars.md", "docs/public/operate/auth/user-provisioning.md"],
    },
  },
];
