package knowledge

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// StandardDomain is a seed record for a knowledge domain shipped with
// memQL. Seeding is idempotent: the seedStandardDomains capability only
// creates domains that don't already exist, so re-running on every
// startup is safe and manual overrides (admin-edited domain rows) are
// preserved.
type StandardDomain struct {
	ID                  string
	Name                string
	Description         string
	Category            string
	RelevantForRoles    []string
	RequiredByToolSlugs []string
	// Tier drives the seeder's content strategy per
	// docs/planning/knowledge-seeder.md:
	//   "A" -- general knowledge, LLM-generated chunks ship as-is.
	//   "B" -- safety-relevant; LLM-generated + a disclaimer chunk
	//          ("general info, not professional advice") prepended.
	//   "C" -- high-stakes specialist (clinical medicine, surgical
	//          technique, securities advice, legal practice). Don't
	//          auto-seed; the seeder writes a single placeholder
	//          chunk telling the user to upload their own
	//          authoritative content. Empty string defaults to "A"
	//          for backwards-compat with domains added before this
	//          field landed.
	Tier string
	// BroadSurvey marks a domain whose scope spans many sub-areas
	// (multi-millennium history, multi-civilization cultural studies,
	// multi-discipline philosophy, etc.). The default 30-chunk target
	// produces ~5 chunks per major sub-area for these, which is too
	// thin to surface specific named events / works / figures by
	// retrieval. Survey domains get a 60-chunk target plus a tighter
	// prompt that requires named-anchor coverage. Narrow domains
	// (e.g. "Heisenberg uncertainty principle" lives inside
	// quantum-mechanics) keep the 30 default.
	BroadSurvey bool
}

// DomainTier exposes the tier discriminator for non-Go consumers
// (kept as a string column on the concept payload so SQL can filter).
type DomainTier string

const (
	TierA DomainTier = "A"
	TierB DomainTier = "B"
	TierC DomainTier = "C"
)

// WikipediaArticles is the optional set of Wikipedia article titles
// to fetch + chunk + embed when the seeder runs against a Tier C
// domain. Set on Tier C entries so the seeder produces real
// authoritative content (with attribution) instead of the
// placeholder chunk. Tier A + B domains ignore this field.
//
// Lives as a separate map (not on StandardDomain itself) so the
// expansion entries above can stay terse -- WikipediaArticles
// only exists for Tier C entries that have a curated mapping.
// Empty mapping (or domain absent from the map) => Tier C falls
// back to the placeholder chunk.
var tierCWikipediaArticles = map[string][]string{
	// Surgical specialties -- linking to Wikipedia overview articles
	// that summarise the field at a high level. Per-procedure detail
	// would need a much larger curated mapping; keeping this lean
	// for v1.
	"med-surgery-general":    {"General surgery", "Surgery", "Surgical specialty"},
	"med-surgery-orthopedic": {"Orthopedic surgery", "Joint replacement", "Fracture"},
	"med-surgery-cardiac":    {"Cardiac surgery", "Coronary artery bypass surgery", "Heart valve repair and replacement"},
	"med-surgery-neuro":      {"Neurosurgery", "Craniotomy", "Spinal surgery"},

	// Clinical specialties -- overview + a couple anchors.
	"med-internal-medicine": {"Internal medicine", "Diagnosis", "Clinical reasoning"},
	"med-cardiology":        {"Cardiology", "Cardiovascular disease", "Echocardiography"},
	"med-neurology":         {"Neurology", "Neurological examination", "Stroke"},
	"med-pediatrics":        {"Pediatrics", "Child development", "Vaccination schedule"},
	"med-geriatrics":        {"Geriatrics", "Frailty syndrome", "Polypharmacy"},
	"med-psychiatry":        {"Psychiatry", "Diagnostic and Statistical Manual of Mental Disorders", "Cognitive behavioral therapy"},
	"med-dentistry":         {"Dentistry", "Oral hygiene", "Periodontology"},
	"med-ophthalmology":     {"Ophthalmology", "Cataract surgery", "Glaucoma"},
	"med-dermatology":       {"Dermatology", "Skin cancer", "Eczema"},
	"med-radiology":         {"Radiology", "Medical imaging", "Computed tomography"},
	"med-anesthesiology":    {"Anesthesiology", "General anaesthesia", "Pain management"},
	"med-veterinary":        {"Veterinary medicine", "Veterinary surgery", "Animal welfare"},
	"med-nursing":           {"Nursing", "Nursing process", "Nursing diagnosis"},
}

// wikipediaArticlesFor returns the Tier C Wikipedia mapping for a
// domain id, or nil if none configured. Used by runSeederForDomain
// to decide whether to fetch real content or write the placeholder.
func wikipediaArticlesFor(domainId string) []string {
	return tierCWikipediaArticles[domainId]
}

// standardDomains mirrors the former hardcoded KNOWLEDGE_DOMAINS list
// that used to live on the CoPresent frontend in agentDefaults.ts. We
// seed it into the database on startup so the frontend (and any other
// client) can query v1:common:knowledgeDomain instead of carrying the
// list in-bundle. The RelevantForRoles slice maps the old
// ROLE_DOMAIN_MAP: a domain whose RelevantForRoles contains role "X"
// will surface in role X's picker.
//
// copresent-ui (bottom of this list) is new -- it's the app-knowledge
// domain required by the operator tools (copresent-takeover /
// copresent-guide) so agents given either automatically pick up
// CoPresent UI knowledge for walkthroughs.
var standardDomains = []StandardDomain{
	// --- Core --------------------------------------------------------------
	// business-administration was previously called general_business AND
	// auto-attached + locked to every agent in the picker. Now it's a
	// regular trainable catalog domain that any agent can opt into; the
	// only special case is the Assistant, which still gets it
	// auto-attached + locked-on by the provisionAssistant
	// automation. Specialists pick it up from the picker like any other
	// domain.
	{ID: "business-administration", Name: "Business Administration", Category: "core",
		Description:      "Core business literacy: org structure, workflows, everyday terminology, basic financial concepts.",
		RelevantForRoles: []string{"assistant", "accounting-finance", "human-resources", "customer-service", "quality-assurance", "sales-marketing", "it-support", "legal-compliance", "operations", "project-management", "research-development", "training-education"},
	},

	// --- Original pre-existing domains ------------------------------------
	{ID: "inventory-supply-chain", Name: "Inventory & Supply Chain", Description: "Stock levels, supplier management, procurement, logistics."},
	{ID: "financial-data", Name: "Financial Data", Description: "Financial statements, ledgers, transactions, accounts."},
	{ID: "employee-records", Name: "Employee Records", Description: "HR files, roles, compensation, organisational directory."},
	{ID: "customer-relations", Name: "Customer Relations", Description: "Customer accounts, contact history, engagement records."},
	{ID: "product-catalog", Name: "Product Catalog", Description: "Product SKUs, specifications, lifecycle, pricing tiers."},
	{ID: "quality-metrics", Name: "Quality Metrics", Description: "Quality KPIs, defect tracking, inspection data, compliance audits."},
	{ID: "legal-documents", Name: "Legal Documents", Description: "Contracts, policies, agreements, legal correspondence."},
	{ID: "project-data", Name: "Project Data", Description: "Project plans, milestones, deliverables, resourcing."},
	{ID: "technical-documentation", Name: "Technical Documentation", Category: "technical", Description: "System architecture, APIs, runbooks, engineering references."},

	// --- Accounting & Finance ---------------------------------------------
	{ID: "accounting-principles", Name: "Accounting Principles", Description: "GAAP/IFRS fundamentals, journal entries, closing processes."},
	{ID: "tax-regulations", Name: "Tax Regulations", Description: "Federal, state, and international tax codes and filings."},
	{ID: "budgeting-forecasting", Name: "Budgeting & Forecasting", Description: "Budget cycles, variance analysis, rolling forecasts."},
	{ID: "payroll-benefits", Name: "Payroll & Benefits", Description: "Payroll processing, benefits administration, compensation rules."},

	// --- Human Resources --------------------------------------------------
	{ID: "talent-acquisition", Name: "Talent Acquisition", Description: "Recruiting pipelines, interviewing, sourcing, onboarding."},
	{ID: "labor-law", Name: "Labor Law", Description: "Employment regulations, workplace compliance, labor relations."},
	{ID: "training-development", Name: "Training & Development", Description: "Training programs, skill development, continuing education."},
	{ID: "organizational-design", Name: "Organizational Design", Description: "Org structure, team topology, reporting lines, role design."},

	// --- Sales & Marketing ------------------------------------------------
	{ID: "sales-pipeline", Name: "Sales Pipeline", Description: "Leads, opportunities, pipeline stages, forecasting."},
	{ID: "marketing-analytics", Name: "Marketing Analytics", Description: "Campaign metrics, attribution, conversion analysis."},
	{ID: "brand-strategy", Name: "Brand Strategy", Description: "Brand positioning, messaging, identity, tone guidelines."},
	{ID: "lead-generation", Name: "Lead Generation", Description: "Prospecting, outbound strategies, top-of-funnel tactics."},

	// --- Customer Service -------------------------------------------------
	{ID: "service-level-agreements", Name: "Service Level Agreements", Description: "SLA definitions, response-time commitments, escalation policies."},
	{ID: "ticket-management", Name: "Ticket Management", Description: "Ticketing workflows, triage, resolution SLAs."},

	// --- IT ---------------------------------------------------------------
	{ID: "network-infrastructure", Name: "Network Infrastructure", Category: "technical", Description: "Network topology, firewalls, routing, VPN configuration."},
	{ID: "cybersecurity", Name: "Cybersecurity", Category: "technical", Description: "Threat models, access control, incident response, encryption."},
	{ID: "software-development", Name: "Software Development", Category: "technical", Description: "Engineering practices, languages, SDLC, version control."},
	{ID: "cloud-platforms", Name: "Cloud Platforms", Category: "technical", Description: "AWS, GCP, Azure services, deployment, cost optimisation."},

	// --- Legal ------------------------------------------------------------
	{ID: "contracts-agreements", Name: "Contracts & Agreements", Description: "Contract drafting, review, negotiation, standard clauses."},
	{ID: "regulatory-compliance", Name: "Regulatory Compliance", Description: "Industry regulations, compliance frameworks, audit readiness."},
	{ID: "intellectual-property", Name: "Intellectual Property", Description: "Patents, trademarks, copyrights, trade secrets, licensing."},

	// --- Operations -------------------------------------------------------
	{ID: "process-optimization", Name: "Process Optimization", Description: "Lean, Six Sigma, workflow efficiency, bottleneck analysis."},
	{ID: "logistics-distribution", Name: "Logistics & Distribution", Description: "Warehousing, shipping, fleet management, last-mile delivery."},
	{ID: "vendor-management", Name: "Vendor Management", Description: "Vendor selection, contracts, performance, relationships."},

	// --- Research & Development -------------------------------------------
	{ID: "research-methodology", Name: "Research Methodology", Description: "Experimental design, statistical analysis, peer review."},
	{ID: "data-analysis", Name: "Data Analysis", Description: "Quantitative analysis, dashboards, A/B testing, statistics."},
	{ID: "innovation-management", Name: "Innovation Management", Description: "Idea pipelines, R&D investment, innovation portfolios."},

	// --- Training & Education ---------------------------------------------
	{ID: "curriculum-design", Name: "Curriculum Design", Description: "Learning objectives, course structure, instructional design."},
	{ID: "performance-assessment", Name: "Performance Assessment", Description: "Evaluation methods, rubrics, performance metrics."},

	// --- Executive / Strategic --------------------------------------------
	{ID: "strategic-planning", Name: "Strategic Planning", Description: "Long-horizon planning, OKRs, scenario modelling."},
	{ID: "risk-management", Name: "Risk Management", Description: "Risk registers, mitigation planning, BCP / DR."},
	{ID: "stakeholder-communications", Name: "Stakeholder Communications", Description: "Executive reporting, board updates, investor relations."},

	// --- CoPresent UI (NEW) -----------------------------------------------
	// Visible for every role so any agent (GA or specialist) can opt in to
	// app-knowledge. Auto-attached to any agent whose tool list includes
	// an operator bundle (copresent-takeover / copresent-guide -- see
	// RequiredByToolSlugs) so picking the tool implies the domain. Document
	// chunks for this domain are seeded below from copresentUISeedCorpus.
	{ID: "copresent-ui", Name: "CoPresent UI", Category: "internal",
		Description:         "Knowledge of the CoPresent application layout, panels, modals, and interactive op-id targets. Auto-attached to any agent given a CoPresent operator tool (Takeover or Guide) so walkthroughs and explanations are anchored to the real UI rather than guessed from training data.",
		RelevantForRoles:    []string{"assistant", "accounting-finance", "human-resources", "customer-service", "quality-assurance", "sales-marketing", "it-support", "legal-compliance", "operations", "project-management", "research-development", "training-education", "personal-finance-advisor", "household-manager", "parenting-coach", "health-wellness-coach", "meal-planning-chef", "travel-planner", "creative-companion", "learning-companion", "relationships-social", "pet-care-specialist", "home-improvement-diy", "personal-legal-advisor", "mindfulness-coach", "entertainment-curator", "senior-care-advisor", "real-estate-advisor"},
		RequiredByToolSlugs: []string{"copresent-takeover", "copresent-guide"},
	},

	// --- Computer Use -----------------------------------------------------
	// Operational manual for the Computer Use capability. Mirrors the
	// copresent-ui pattern: tool requires domain, domain doesn't
	// require tool. Any agent given the Computer Use capability (slug
	// "computer-use") gets this knowledge auto-attached so the
	// generic agentReply template stays agnostic; everything specific
	// to scope tiers, the per-task approval gate, the post-approval
	// dispatch flow, and the planner's outcome-detection semantics
	// lives here as RAG-retrievable chunks. Seeded below from
	// computerUseSeedCorpus.
	//
	// Visibility: every role -- a knowledge specialist (e.g. a
	// research agent) might want to be able to TALK about Computer
	// Use even without holding the capability themselves, just like
	// copresent-ui is attachable without an operator tool.
	{ID: "computer-use", Name: "Computer Use", Category: "internal",
		Description:         "Operational manual for the Computer Use capability: tool surfaces (workerHost / workerComputer), scope tiers (observe / full), per-task approval flow (requestComputerUseScope -> canvas card -> Allow / Deny), post-approval execution semantics, and the planner's success-vs-failure detection. Auto-attached to any agent given the Computer Use capability so the generic prompt template stays agnostic.",
		RelevantForRoles:    []string{"assistant", "accounting-finance", "human-resources", "customer-service", "quality-assurance", "sales-marketing", "it-support", "legal-compliance", "operations", "project-management", "research-development", "training-education", "personal-finance-advisor", "household-manager", "parenting-coach", "health-wellness-coach", "meal-planning-chef", "travel-planner", "creative-companion", "learning-companion", "relationships-social", "pet-care-specialist", "home-improvement-diy", "personal-legal-advisor", "mindfulness-coach", "entertainment-curator", "senior-care-advisor", "real-estate-advisor"},
		RequiredByToolSlugs: []string{"computer-use-headless", "computer-use-embodied"},
	},

	// --- Workbench --------------------------------------------------------
	// Operational manual for the workbench capability. Mirrors the
	// computer-use entry pattern: tool requires domain, domain doesn't
	// require tool. Universal capability -- the workbench is the
	// default first choice for any headless work and is on by default
	// for every agent, so the domain is broadly RelevantForRoles.
	//
	// The chunks teach the agent: WHEN to use the workbench vs
	// computer-use (workbench is Linux + sandboxed; computer-use is
	// the user's actual machine + might be macOS), how workspaces
	// persist across calls within a Plan, and how to handle the
	// "this needs Xcode" kind of failure (declare the limitation in
	// respondToUser so the planner can re-route with computer-use).
	{ID: "workbench", Name: "Workbench", Category: "internal",
		Description:         "Operational manual for the Workbench capability: per-Plan sandboxed Linux workspace, the workbenchHost tool surface (exec / fs_read / fs_write / fs_list / fs_stat / http_fetch), persistence semantics (workspace outlasts individual Tasks; torn down at Plan terminal), the prefer-workbench-over-computer-use guidance, and the failure pattern when the agent discovers the workbench can't do the job (e.g. needs macOS / Xcode). Auto-attached to any agent given the workbench capability.",
		RelevantForRoles:    []string{"assistant", "accounting-finance", "human-resources", "customer-service", "quality-assurance", "sales-marketing", "it-support", "legal-compliance", "operations", "project-management", "research-development", "training-education", "personal-finance-advisor", "household-manager", "parenting-coach", "health-wellness-coach", "meal-planning-chef", "travel-planner", "creative-companion", "learning-companion", "relationships-social", "pet-care-specialist", "home-improvement-diy", "personal-legal-advisor", "mindfulness-coach", "entertainment-curator", "senior-care-advisor", "real-estate-advisor"},
		RequiredByToolSlugs: []string{"workbench-use"},
	},

	// --- CoPresent Conversation -------------------------------------------
	// Operational manual for the two-thread chat model (Phase 5 of the
	// chat-architecture plan). Distinct from copresent-ui (which covers
	// app surfaces) and computer-use (which covers off-app machine
	// driving): recent-chat is the contract for how an agent
	// behaves INSIDE the chat, given that there are now two threads
	// (Group + per-user Team) with hard isolation between them.
	//
	// Auto-attached at agent-prompt-assembly time whenever the agent is
	// dispatching for a non-empty spaceId -- i.e., the agent is acting
	// as a space participant. See replier.go for the auto-injection.
	// 1-on-1 / direct interactions (no spaceId) skip the domain so we
	// don't pay retrieval cost when chat-thread context is irrelevant.
	{ID: "recent-chat", Name: "Recent Chat", Category: "internal",
		Description:         "Operational manual for the single-chat space architecture: one v1:cognition:utterance stream visible to all participants, the owner's assistant as the only AI presence that speaks to humans, specialists invoked via askSpecialist returning structured JSON, canvas-not-chat for system events, and the recentChat tool for read-only chat context. Auto-attached to any agent participating in a space.",
		RelevantForRoles:    []string{"assistant", "accounting-finance", "human-resources", "customer-service", "quality-assurance", "sales-marketing", "it-support", "legal-compliance", "operations", "project-management", "research-development", "training-education", "personal-finance-advisor", "household-manager", "parenting-coach", "health-wellness-coach", "meal-planning-chef", "travel-planner", "creative-companion", "learning-companion", "relationships-social", "pet-care-specialist", "home-improvement-diy", "personal-legal-advisor", "mindfulness-coach", "entertainment-curator", "senior-care-advisor", "real-estate-advisor"},
		RequiredByToolSlugs: []string{"recent-chat"},
	},

	// --- Personal Finance -------------------------------------------------
	{ID: "personal-finance", Name: "Personal Finance", Category: "product",
		Description: "Personal budgeting, expense tracking, net-worth, financial goal setting, debt management, savings strategies."},
	{ID: "personal-taxes", Name: "Personal Taxes", Category: "product",
		Description: "Personal income tax: filing, deductions, credits, withholdings, IRS procedures, state-tax variations, estimated payments."},
	{ID: "personal-investing", Name: "Personal Investing", Category: "product",
		Description: "Brokerage accounts, retirement accounts (IRA / 401(k) / Roth), index funds, asset allocation, rebalancing, tax-loss harvesting."},
	{ID: "personal-insurance", Name: "Personal Insurance", Category: "product",
		Description: "Health, auto, home/renters, life, umbrella, disability insurance: shopping, claims, coverage analysis."},
	{ID: "personal-budgeting", Name: "Personal Budgeting", Category: "product",
		Description: "Monthly budget templates, envelope methods, zero-based budgeting, irregular-income budgeting, cash-flow planning."},

	// --- Household Management ---------------------------------------------
	{ID: "household-maintenance", Name: "Household Maintenance", Category: "product",
		Description: "Routine maintenance schedules: HVAC filter changes, gutter cleaning, water heater flush, smoke detector batteries, appliance servicing."},
	{ID: "home-inventory", Name: "Home Inventory", Category: "product",
		Description: "Tracking owned items for insurance / warranty: serial numbers, purchase dates, receipts, photos, replacement values."},
	{ID: "household-chores", Name: "Household Chores & Routines", Category: "product",
		Description: "Cleaning routines, family chore charts, weekly / monthly household tasks, supply inventory."},

	// --- Parenting --------------------------------------------------------
	{ID: "parenting", Name: "Parenting", Category: "product",
		Description: "Daily parenting routines, discipline approaches, age-appropriate guidance, family activities, parent-child communication."},
	{ID: "child-development", Name: "Child Development", Category: "product",
		Description: "Developmental milestones (motor, language, social-emotional, cognitive) by age range; warning signs; resources."},
	{ID: "school-education-personal", Name: "School & Education (Personal)", Category: "product",
		Description: "K-12 school logistics: calendars, parent-teacher conferences, IEP/504, homework support, college prep."},

	// --- Health & Wellness ------------------------------------------------
	{ID: "nutrition", Name: "Nutrition", Category: "product",
		Description: "Macronutrients, micronutrients, dietary patterns (Mediterranean, plant-based, low-carb), reading nutrition labels, recipe substitutions."},
	{ID: "fitness", Name: "Fitness", Category: "product",
		Description: "Workout programming (strength, cardio, mobility), exercise form, training periodization, injury prevention, home-gym basics."},
	{ID: "mental-health", Name: "Mental Health", Category: "product",
		Description: "Stress management, anxiety / depression awareness, therapy modalities, when to seek professional help, mental-health first aid."},
	{ID: "sleep-hygiene", Name: "Sleep Hygiene", Category: "product",
		Description: "Sleep cycles, light exposure, bedtime routines, common sleep disorders, evidence-based interventions for sleep quality."},
	{ID: "medical-records-personal", Name: "Personal Medical Records", Category: "product",
		Description: "Personal medication list, allergy log, vaccination history, prior procedures, family-history relevant conditions."},

	// --- Meal Planning & Recipes ------------------------------------------
	{ID: "recipes", Name: "Recipes", Category: "product",
		Description: "Personal recipe collection: ingredients, instructions, prep / cook time, servings, dietary tags, favorites."},
	{ID: "meal-planning", Name: "Meal Planning", Category: "product",
		Description: "Weekly meal planning, batch cooking, freezer meals, leftover strategy, themed weeks (taco Tuesday, etc.)."},
	{ID: "dietary-restrictions", Name: "Dietary Restrictions", Category: "product",
		Description: "Allergens, food intolerances, religious / ethical restrictions, medical diets (low-sodium, diabetic, kidney-friendly)."},

	// --- Travel -----------------------------------------------------------
	{ID: "travel-planning", Name: "Travel Planning", Category: "product",
		Description: "Trip itineraries, flights, hotels, activities, packing lists, travel insurance, budget per destination."},
	{ID: "travel-documents", Name: "Travel Documents", Category: "product",
		Description: "Passports, visas, vaccination requirements, TSA Pre-Check / Global Entry, international driver's permit, travel insurance docs."},
	{ID: "restaurant-dining", Name: "Restaurant & Dining", Category: "product",
		Description: "Restaurant favorites, dietary preferences, reservation history, regional specialties, dining budgets."},

	// --- Creative ---------------------------------------------------------
	{ID: "creative-arts", Name: "Creative Arts", Category: "product",
		Description: "Visual arts (drawing, painting), music, writing, crafts; technique references, materials, project history."},
	{ID: "music-appreciation", Name: "Music Appreciation", Category: "product",
		Description: "Personal music library, artists, genres, concert history, playlists, learning instruments."},
	{ID: "photography", Name: "Photography", Category: "product",
		Description: "Camera settings, composition principles, post-processing, photo organization, gear inventory."},

	// --- Learning ---------------------------------------------------------
	{ID: "language-learning", Name: "Language Learning", Category: "product",
		Description: "Vocabulary, grammar references, learning resources, conversation practice, immersion strategies for spoken languages."},
	{ID: "online-courses", Name: "Online Courses", Category: "product",
		Description: "MOOC enrollments (Coursera, edX, Udemy), course progress, certificates, learning notes, study schedule."},
	{ID: "book-summaries", Name: "Books & Reading", Category: "product",
		Description: "Personal reading list, book summaries, annotations, recommendations, reading goals."},

	// --- Relationships & Social -------------------------------------------
	{ID: "relationships-communication", Name: "Relationships & Communication", Category: "product",
		Description: "Communication frameworks (NVC, active listening), conflict resolution, healthy relationship patterns, attachment styles."},
	{ID: "life-events-celebrations", Name: "Life Events & Celebrations", Category: "product",
		Description: "Birthdays, anniversaries, holidays, gift histories, party planning, traditions."},
	{ID: "gifts", Name: "Gifts & Recommendations", Category: "product",
		Description: "Gift ideas tailored per person (preferences, history of gifts given/received), occasions, budgets."},

	// --- Pet Care ---------------------------------------------------------
	{ID: "pet-care", Name: "Pet Care", Category: "product",
		Description: "Vet schedules, medication, food, grooming, daily care routines per species / breed."},
	{ID: "pet-training", Name: "Pet Training", Category: "product",
		Description: "Positive-reinforcement training, behavior modification, basic commands, common problem behaviors."},
	{ID: "pet-health", Name: "Pet Health", Category: "product",
		Description: "Common health conditions per species / breed, emergency signs, preventive care, vaccinations."},

	// --- DIY & Home Improvement -------------------------------------------
	{ID: "diy-repairs", Name: "DIY & Repairs", Category: "product",
		Description: "Common home repairs (plumbing, electrical, drywall, painting), tool basics, when to DIY vs hire, safety."},
	{ID: "gardening", Name: "Gardening", Category: "product",
		Description: "Plant care by zone, watering schedules, pest management, seasonal planting, vegetable gardening."},
	{ID: "sustainability", Name: "Sustainability & Eco-Living", Category: "product",
		Description: "Energy efficiency, recycling, composting, sustainable shopping, low-waste living."},

	// --- Personal Legal ---------------------------------------------------
	{ID: "personal-legal", Name: "Personal Legal Matters", Category: "product",
		Description: "Tenant rights, consumer protection, small claims, traffic, employment law for individuals (NOT a substitute for an attorney)."},
	{ID: "estate-planning-personal", Name: "Estate Planning (Personal)", Category: "product",
		Description: "Wills, living trusts, healthcare directives, power of attorney, beneficiary designations."},
	{ID: "contracts-personal", Name: "Personal Contracts", Category: "product",
		Description: "Leases, employment offers, freelance agreements, NDAs, service contracts -- review checklists for non-lawyers."},

	// --- Mindfulness & Personal Growth ------------------------------------
	{ID: "mindfulness", Name: "Mindfulness & Meditation", Category: "product",
		Description: "Meditation techniques (focused attention, open monitoring, loving-kindness), breathwork, common challenges."},
	{ID: "journaling", Name: "Journaling", Category: "product",
		Description: "Journaling prompts, gratitude practices, morning pages, reflective writing, journal organization."},
	{ID: "personal-growth", Name: "Personal Growth", Category: "product",
		Description: "Goal-setting frameworks (SMART, OKR-personal), habit formation, accountability systems, self-reflection prompts."},

	// --- Entertainment ----------------------------------------------------
	{ID: "entertainment-media", Name: "Entertainment & Media", Category: "product",
		Description: "Movies, TV shows, podcasts, video games -- watched / unwatched lists, ratings, recommendations from sources."},

	// --- Senior Care ------------------------------------------------------
	{ID: "elder-care", Name: "Elder Care", Category: "product",
		Description: "Aging-in-place, in-home care services, assisted living, Medicare basics, caregiver burnout, family communication."},
	{ID: "end-of-life-planning", Name: "End-of-Life Planning", Category: "product",
		Description: "Hospice / palliative care, advance directives, funeral planning, legacy projects, grief support."},

	// --- Real Estate ------------------------------------------------------
	// Personal-tier real-estate domains. Cover the buy / sell / rent
	// research surface for an individual transacting one residential
	// property at a time. Commercial real estate / property management
	// would warrant a separate set of business-category domains if we
	// ever need them; these intentionally stay personal.
	{ID: "real-estate-listings", Name: "Real Estate Listings", Category: "product",
		Description: "Active for-sale + for-rent listings, MLS search, comparable sales (comps), price-history tracking, saved searches, listing alerts."},
	{ID: "mortgage-shopping", Name: "Mortgage Shopping", Category: "product",
		Description: "Loan types (conventional, FHA, VA, jumbo), interest rates, points, pre-approval, lender comparison, down-payment strategies, PMI, refinancing."},
	{ID: "neighborhood-research", Name: "Neighborhood Research", Category: "product",
		Description: "School-district ratings, crime stats, walkability + transit scores, commute times, amenities, future development, HOA / zoning notes."},
	{ID: "home-inspection", Name: "Home Inspection", Category: "product",
		Description: "Pre-purchase + pre-sale inspection checklists, common findings (roof, foundation, electrical, plumbing, HVAC, pests, radon), inspector-shopping, repair-estimate negotiation."},
	{ID: "property-taxes", Name: "Property Taxes", Category: "product",
		Description: "County assessment cycles, tax rates by jurisdiction, homestead / senior / veteran exemptions, assessment appeals, escrow vs direct payment."},
	{ID: "lease-agreements", Name: "Lease Agreements", Category: "product",
		Description: "Residential lease review, tenant + landlord rights, security deposits, rent escalation, renewal + termination clauses, common red flags."},
	{ID: "closing-process", Name: "Closing Process", Category: "product",
		Description: "Purchase contracts, earnest money, title search, title insurance, escrow, closing disclosure, closing costs breakdown, deed recording, walk-through checklist."},

	// =========================================================================
	// CATALOG EXPANSION (per docs/planning/knowledge-seeder.md)
	//
	// Adds ~150 domains across science, medicine, humanities, arts, and
	// specialized fields. Each entry carries a Tier:
	//   "A" -- general knowledge, LLM-seeded normally
	//   "B" -- safety-relevant, gets a disclaimer chunk prepended
	//   "C" -- high-stakes specialist, NOT auto-seeded; ships with a
	//          "upload your own authoritative content" placeholder
	//
	// Existing 96 entries above default to Tier A; safety-relevant ones
	// among them (personal-finance, personal-taxes, mental-health, etc.)
	// get explicit Tier "B" stamped via the tierOverride map below
	// rather than touching every existing line.
	// =========================================================================

	// --- Physics --------------------------------------------------------------
	{ID: "physics-classical-mechanics", Name: "Classical Mechanics", Category: "science", Tier: "A",
		Description: "Newtonian mechanics, kinematics, dynamics, conservation laws, Lagrangian + Hamiltonian formulations, rotational dynamics, oscillations."},
	{ID: "physics-thermodynamics", Name: "Thermodynamics", Category: "science", Tier: "A",
		Description: "Laws of thermodynamics, heat engines, entropy, statistical mechanics fundamentals, phase transitions."},
	{ID: "physics-electromagnetism", Name: "Electromagnetism", Category: "science", Tier: "A",
		Description: "Maxwell's equations, electric + magnetic fields, electromagnetic waves, circuits, optics."},
	{ID: "physics-quantum-mechanics", Name: "Quantum Mechanics", Category: "science", Tier: "A",
		Description: "Wave functions, Schrödinger equation, operators, uncertainty principle, entanglement, superposition, quantum measurement."},
	{ID: "physics-relativity", Name: "Relativity", Category: "science", Tier: "A",
		Description: "Special + general relativity, spacetime, Lorentz transformations, gravity as curvature, black holes, gravitational waves."},
	{ID: "physics-particle", Name: "Particle Physics", Category: "science", Tier: "A",
		Description: "Standard Model, quarks + leptons, gauge bosons, Higgs mechanism, particle accelerators, fundamental forces."},
	{ID: "physics-astrophysics", Name: "Astrophysics", Category: "science", Tier: "A",
		Description: "Stellar structure + evolution, nucleosynthesis, galactic dynamics, supernovae, neutron stars, exoplanets, observational techniques."},
	{ID: "physics-cosmology", Name: "Cosmology", Category: "science", Tier: "A",
		Description: "Big Bang model, cosmic microwave background, inflation, dark matter + dark energy, large-scale structure, expansion of the universe."},

	// --- Chemistry ------------------------------------------------------------
	{ID: "chemistry-organic", Name: "Organic Chemistry", Category: "science", Tier: "A",
		Description: "Carbon-based molecules, functional groups, reaction mechanisms, stereochemistry, synthesis pathways, spectroscopy."},
	{ID: "chemistry-inorganic", Name: "Inorganic Chemistry", Category: "science", Tier: "A",
		Description: "Periodic trends, ionic + covalent bonding, coordination chemistry, transition metals, ceramics + solid-state materials."},
	{ID: "chemistry-biochemistry", Name: "Biochemistry", Category: "science", Tier: "A",
		Description: "Proteins, enzymes, lipids, carbohydrates, nucleic acids, metabolism, biomolecular structure + function."},
	{ID: "chemistry-analytical", Name: "Analytical Chemistry", Category: "science", Tier: "A",
		Description: "Spectroscopic techniques (NMR, IR, mass spec, UV-Vis), chromatography, electrochemical analysis, sample prep, quantification."},
	{ID: "chemistry-physical", Name: "Physical Chemistry", Category: "science", Tier: "A",
		Description: "Quantum chemistry, kinetics, thermodynamics of reactions, molecular spectroscopy, statistical mechanics applied to chemistry."},

	// --- Biology --------------------------------------------------------------
	{ID: "biology-molecular", Name: "Molecular Biology", Category: "science", Tier: "A",
		Description: "DNA / RNA structure + replication, transcription + translation, gene regulation, molecular cloning techniques."},
	{ID: "biology-genetics", Name: "Genetics", Category: "science", Tier: "A",
		Description: "Mendelian + non-Mendelian inheritance, gene mapping, mutation, population genetics, genome sequencing."},
	{ID: "biology-cell", Name: "Cell Biology", Category: "science", Tier: "A",
		Description: "Cell organelles, membrane transport, cell cycle, signal transduction, cytoskeleton, apoptosis, organelle biogenesis."},
	{ID: "biology-ecology", Name: "Ecology", Category: "science", Tier: "A",
		Description: "Ecosystem dynamics, population biology, biodiversity, community interactions, biogeochemical cycles, conservation biology."},
	{ID: "biology-evolution", Name: "Evolution", Category: "science", Tier: "A",
		Description: "Natural selection, speciation, phylogenetics, adaptation, genetic drift, evolutionary development, evidence + mechanisms."},
	{ID: "biology-neuroscience", Name: "Neuroscience", Category: "science", Tier: "A",
		Description: "Neuron structure + function, synaptic transmission, brain anatomy, sensory + motor systems, learning + memory, neural development."},
	{ID: "biology-microbiology", Name: "Microbiology", Category: "science", Tier: "A",
		Description: "Bacteria, viruses, fungi, archaea, microbial physiology, pathogenesis, microbial ecology, applied microbiology."},
	{ID: "biology-botany", Name: "Botany", Category: "science", Tier: "A",
		Description: "Plant anatomy + physiology, photosynthesis, plant reproduction, plant taxonomy, plant ecology, agriculture-relevant biology."},
	{ID: "biology-zoology", Name: "Zoology", Category: "science", Tier: "A",
		Description: "Animal taxonomy, comparative anatomy + physiology, behavior, conservation status, evolutionary relationships across kingdoms."},
	{ID: "biology-immunology", Name: "Immunology", Category: "science", Tier: "A",
		Description: "Innate + adaptive immunity, antibodies, T-cell + B-cell biology, vaccines, immune disorders, transplantation immunology."},

	// --- Earth Sciences -------------------------------------------------------
	{ID: "earthsci-geology", Name: "Geology", Category: "science", Tier: "A",
		Description: "Plate tectonics, mineralogy + petrology, stratigraphy, geomorphology, earthquakes + volcanism, geological time scale."},
	{ID: "earthsci-meteorology", Name: "Meteorology", Category: "science", Tier: "A",
		Description: "Atmospheric physics, weather systems, forecasting, severe weather, climate fundamentals, atmospheric chemistry."},
	{ID: "earthsci-oceanography", Name: "Oceanography", Category: "science", Tier: "A",
		Description: "Ocean circulation, marine geology, marine biology basics, ocean chemistry, coastal processes, marine resources."},
	{ID: "earthsci-climate", Name: "Climate Science", Category: "science", Tier: "B",
		Description: "Climate system, anthropogenic + natural drivers, climate models, paleoclimate, regional climate impacts, mitigation + adaptation. Disclaimer: scientific consensus topic; recommend authoritative sources for policy advice."},
	{ID: "earthsci-environmental", Name: "Environmental Science", Category: "science", Tier: "A",
		Description: "Environmental systems, pollution, biodiversity loss, sustainability metrics, environmental policy basics, ecotoxicology."},

	// --- Mathematics ----------------------------------------------------------
	{ID: "math-algebra", Name: "Algebra", Category: "science", Tier: "A",
		Description: "Linear + abstract algebra, polynomials, equations + systems, group / ring / field theory basics, vector spaces."},
	{ID: "math-calculus", Name: "Calculus", Category: "science", Tier: "A",
		Description: "Limits, derivatives, integrals, multivariable calculus, vector calculus, differential equations basics, applied techniques."},
	{ID: "math-statistics", Name: "Statistics", Category: "science", Tier: "A",
		Description: "Descriptive + inferential statistics, hypothesis testing, regression, ANOVA, Bayesian methods, experimental design."},
	{ID: "math-probability", Name: "Probability", Category: "science", Tier: "A",
		Description: "Probability axioms, random variables, distributions, expectation + variance, Markov chains, stochastic processes."},
	{ID: "math-linear-algebra", Name: "Linear Algebra", Category: "science", Tier: "A",
		Description: "Vector spaces, matrices, eigenvalues + eigenvectors, linear transformations, decompositions, applications."},
	{ID: "math-discrete", Name: "Discrete Mathematics", Category: "science", Tier: "A",
		Description: "Combinatorics, graph theory, recurrence relations, propositional + predicate logic, set theory, number theory basics."},
	{ID: "math-topology", Name: "Topology", Category: "science", Tier: "A",
		Description: "Point-set + algebraic topology, continuity, compactness, connectedness, topological invariants, manifolds intro."},
	{ID: "math-number-theory", Name: "Number Theory", Category: "science", Tier: "A",
		Description: "Divisibility, primes, modular arithmetic, Diophantine equations, cryptographic applications, analytic number theory intro."},

	// --- Computer Science (beyond technical-documentation) --------------------
	{ID: "cs-algorithms", Name: "Algorithms", Category: "technical", Tier: "A",
		Description: "Sorting + searching, divide-and-conquer, greedy, dynamic programming, graph algorithms, string algorithms, complexity analysis."},
	{ID: "cs-data-structures", Name: "Data Structures", Category: "technical", Tier: "A",
		Description: "Arrays, lists, trees (BST, B-tree, trie, heap), hash tables, graphs, persistent structures, amortised analysis."},
	{ID: "cs-complexity-theory", Name: "Computational Complexity", Category: "technical", Tier: "A",
		Description: "P vs NP, NP-completeness, polynomial-time hierarchy, space complexity, approximation + randomised complexity classes."},
	{ID: "cs-distributed-systems", Name: "Distributed Systems", Category: "technical", Tier: "A",
		Description: "CAP theorem, consensus (Paxos / Raft), replication, distributed transactions, fault tolerance, distributed data stores."},
	{ID: "cs-databases", Name: "Databases", Category: "technical", Tier: "A",
		Description: "Relational + NoSQL models, SQL, normalization, indexing, transactions + ACID, query optimization, MVCC."},
	{ID: "cs-networking", Name: "Networking", Category: "technical", Tier: "A",
		Description: "OSI + TCP/IP layers, routing, congestion control, DNS, HTTP / TLS, BGP, modern transport (QUIC)."},
	{ID: "cs-security", Name: "Security & Cryptography", Category: "technical", Tier: "A",
		Description: "Threat models, symmetric + asymmetric cryptography, hashing + MACs, TLS, key exchange, common vulnerabilities (OWASP), secure coding patterns."},
	{ID: "cs-machine-learning", Name: "Machine Learning", Category: "technical", Tier: "A",
		Description: "Supervised + unsupervised learning, neural networks (CNN/RNN/Transformer), training pipelines, evaluation, common architectures + use cases."},
	{ID: "cs-computer-graphics", Name: "Computer Graphics", Category: "technical", Tier: "A",
		Description: "Rasterization + raytracing, shaders, 3D math, lighting models, animation, GPU pipelines, real-time techniques."},
	{ID: "cs-compilers", Name: "Compilers + Languages", Category: "technical", Tier: "A",
		Description: "Lexing + parsing, ASTs, type systems, semantic analysis, intermediate representations, optimization, code generation."},

	// --- Engineering ----------------------------------------------------------
	{ID: "eng-mechanical", Name: "Mechanical Engineering", Category: "science", Tier: "A",
		Description: "Statics + dynamics, fluid mechanics, heat transfer, machine design, manufacturing processes, materials selection."},
	{ID: "eng-electrical", Name: "Electrical Engineering", Category: "science", Tier: "A",
		Description: "Circuit analysis, signals + systems, control theory, power systems, semiconductors, embedded systems."},
	{ID: "eng-civil", Name: "Civil Engineering", Category: "science", Tier: "A",
		Description: "Structural analysis, geotechnical engineering, transportation, water resources, construction management, infrastructure."},
	{ID: "eng-chemical", Name: "Chemical Engineering", Category: "science", Tier: "A",
		Description: "Mass + energy balances, reactor design, separation processes, process control, chemical plant design, safety."},
	{ID: "eng-aerospace", Name: "Aerospace Engineering", Category: "science", Tier: "A",
		Description: "Aerodynamics, propulsion, orbital mechanics, aircraft + spacecraft design, materials for extreme environments."},
	{ID: "eng-biomedical", Name: "Biomedical Engineering", Category: "science", Tier: "A",
		Description: "Medical devices, biomaterials, biomechanics, medical imaging, prosthetics, tissue engineering."},
	{ID: "eng-materials", Name: "Materials Science", Category: "science", Tier: "A",
		Description: "Crystal structures, metals + alloys, polymers, ceramics, composites, nanomaterials, characterization techniques."},
	{ID: "eng-software-architecture", Name: "Software Architecture", Category: "technical", Tier: "A",
		Description: "Architectural patterns, microservices vs monolith, scalability + reliability, event-driven design, API design, DDD."},
	{ID: "eng-control-systems", Name: "Control Systems", Category: "science", Tier: "A",
		Description: "Feedback control, PID, state-space, stability analysis, frequency-domain methods, modern + adaptive control."},

	// --- Medicine & Health ---------------------------------------------------
	// Tier C across the clinical specialties: don't auto-seed actionable
	// medical content. Foundational basic-science domains (anatomy,
	// pharmacology fundamentals) are Tier B with disclaimers.
	{ID: "med-anatomy-physiology", Name: "Anatomy & Physiology", Category: "science", Tier: "B",
		Description: "Human anatomical systems, organ function, homeostasis, physiology of major systems. General reference only; not medical advice."},
	{ID: "med-pharmacology-basics", Name: "Pharmacology Basics", Category: "science", Tier: "B",
		Description: "Drug classifications, mechanisms of action, pharmacokinetics + pharmacodynamics. Educational reference; consult a licensed prescriber for medication decisions."},
	{ID: "med-internal-medicine", Name: "Internal Medicine", Category: "science", Tier: "C",
		Description: "Adult clinical medicine across organ systems. High-stakes domain -- not auto-seeded; upload authoritative clinical references if needed."},
	{ID: "med-surgery-general", Name: "General Surgery", Category: "science", Tier: "C",
		Description: "Surgical techniques + management. High-stakes domain -- not auto-seeded; consult surgical textbooks + licensed practitioners."},
	{ID: "med-surgery-orthopedic", Name: "Orthopedic Surgery", Category: "science", Tier: "C",
		Description: "Musculoskeletal surgical techniques. Tier C -- not auto-seeded."},
	{ID: "med-surgery-cardiac", Name: "Cardiac Surgery", Category: "science", Tier: "C",
		Description: "Cardiothoracic surgical techniques. Tier C -- not auto-seeded."},
	{ID: "med-surgery-neuro", Name: "Neurosurgery", Category: "science", Tier: "C",
		Description: "Neurosurgical techniques. Tier C -- not auto-seeded."},
	{ID: "med-cardiology", Name: "Cardiology", Category: "science", Tier: "C",
		Description: "Diagnosis + management of cardiovascular disease. Tier C -- not auto-seeded."},
	{ID: "med-neurology", Name: "Neurology", Category: "science", Tier: "C",
		Description: "Diagnosis + management of neurological conditions. Tier C -- not auto-seeded."},
	{ID: "med-pediatrics", Name: "Pediatrics", Category: "science", Tier: "C",
		Description: "Clinical pediatric care. Tier C -- not auto-seeded."},
	{ID: "med-geriatrics", Name: "Geriatrics", Category: "science", Tier: "C",
		Description: "Clinical care of older adults. Tier C -- not auto-seeded."},
	{ID: "med-psychiatry", Name: "Psychiatry", Category: "science", Tier: "C",
		Description: "Psychiatric diagnosis + treatment. Tier C -- not auto-seeded."},
	{ID: "med-public-health", Name: "Public Health", Category: "science", Tier: "B",
		Description: "Population-level health, health systems, prevention, health-policy basics. Educational reference."},
	{ID: "med-epidemiology", Name: "Epidemiology", Category: "science", Tier: "A",
		Description: "Study designs, measures of disease frequency, causal inference in observational data, outbreak investigation methods."},
	{ID: "med-dentistry", Name: "Dentistry", Category: "science", Tier: "C",
		Description: "Clinical dentistry. Tier C -- not auto-seeded."},
	{ID: "med-ophthalmology", Name: "Ophthalmology", Category: "science", Tier: "C",
		Description: "Eye care + surgery. Tier C -- not auto-seeded."},
	{ID: "med-dermatology", Name: "Dermatology", Category: "science", Tier: "C",
		Description: "Skin diagnosis + treatment. Tier C -- not auto-seeded."},
	{ID: "med-radiology", Name: "Radiology", Category: "science", Tier: "C",
		Description: "Medical imaging interpretation + technique. Tier C -- not auto-seeded."},
	{ID: "med-anesthesiology", Name: "Anesthesiology", Category: "science", Tier: "C",
		Description: "Perioperative management + anesthesia. Tier C -- not auto-seeded."},
	{ID: "med-veterinary", Name: "Veterinary Medicine", Category: "science", Tier: "C",
		Description: "Animal clinical care + surgery. Tier C -- not auto-seeded."},
	{ID: "med-sports", Name: "Sports Medicine", Category: "science", Tier: "B",
		Description: "Exercise physiology, common athletic injuries, rehab basics, performance science. Educational reference; not a substitute for clinical evaluation."},
	{ID: "med-nursing", Name: "Nursing", Category: "science", Tier: "C",
		Description: "Clinical nursing practice. Tier C -- not auto-seeded."},

	// --- Humanities & Social Sciences -----------------------------------------
	// History is the canonical broad-survey case: each entry below
	// spans multiple millennia or continents. The 30-chunk default
	// produced ~5 chunks per major sub-area, which retrieved as
	// generic primer content (e.g. asking about the Bronze Age
	// Collapse returned "Imperial administration and provinces" -- a
	// 0.4 cosine match that the model couldn't honestly cite). Mark
	// these BroadSurvey so the seeder runs the 60-chunk + named-
	// anchor prompt path.
	{ID: "hist-ancient", Name: "Ancient History", Category: "humanities", Tier: "A", BroadSurvey: true,
		Description: "Mesopotamia, Egypt, Greece, Rome, ancient China, India, Mesoamerica through ~500 CE."},
	{ID: "hist-medieval", Name: "Medieval History", Category: "humanities", Tier: "A", BroadSurvey: true,
		Description: "Europe + Mediterranean ~500-1500 CE, Byzantine empire, Islamic world, medieval Asia + Africa."},
	{ID: "hist-early-modern", Name: "Early Modern History", Category: "humanities", Tier: "A", BroadSurvey: true,
		Description: "Renaissance, Reformation, age of exploration, scientific revolution, early colonial empires, ~1500-1800."},
	{ID: "hist-modern", Name: "Modern History", Category: "humanities", Tier: "A", BroadSurvey: true,
		Description: "Industrial revolution, world wars, Cold War, decolonization, late-20th + 21st century, global politics + culture."},
	{ID: "hist-us", Name: "U.S. History", Category: "humanities", Tier: "A", BroadSurvey: true,
		Description: "Colonial period through present day -- founding, expansion, Civil War, industrialization, civil rights, contemporary."},
	{ID: "hist-world-civilizations", Name: "World Civilizations", Category: "humanities", Tier: "A", BroadSurvey: true,
		Description: "Comparative history of major civilizations -- Africa, Asia, Americas, Pacific -- their interactions, technologies, cultures."},

	{ID: "phil-ethics", Name: "Ethics", Category: "humanities", Tier: "A",
		Description: "Normative ethics (consequentialism, deontology, virtue ethics), metaethics, applied ethics, contemporary ethical debates."},
	{ID: "phil-metaphysics", Name: "Metaphysics", Category: "humanities", Tier: "A",
		Description: "Existence, identity, time, causality, free will, mind-body problem, modal realism, ontology fundamentals."},
	{ID: "phil-epistemology", Name: "Epistemology", Category: "humanities", Tier: "A",
		Description: "Knowledge + belief, justification, skepticism, theories of truth, social epistemology, virtue epistemology."},
	{ID: "phil-logic", Name: "Logic", Category: "humanities", Tier: "A",
		Description: "Propositional + predicate logic, modal + temporal logic, formal proof systems, argument analysis, common fallacies."},
	{ID: "phil-political", Name: "Political Philosophy", Category: "humanities", Tier: "A",
		Description: "Justice, liberty, equality, the state, democratic theory, liberalism + alternatives, contemporary political theory."},
	{ID: "phil-mind", Name: "Philosophy of Mind", Category: "humanities", Tier: "A",
		Description: "Consciousness, intentionality, qualia, functionalism, dualism vs physicalism, AI + machine consciousness debates."},
	{ID: "phil-science", Name: "Philosophy of Science", Category: "humanities", Tier: "A",
		Description: "Scientific method, theory choice, paradigm shifts, scientific realism vs antirealism, demarcation problem."},

	{ID: "lit-genres", Name: "Literature: Genres + Forms", Category: "humanities", Tier: "A",
		Description: "Poetry, fiction (novel + short story), drama, non-fiction, essay; key forms, conventions, historical development."},
	{ID: "lit-world", Name: "World Literature", Category: "humanities", Tier: "A",
		Description: "Major literary traditions across cultures + centuries -- canonical works + their context."},
	{ID: "lit-criticism", Name: "Literary Criticism", Category: "humanities", Tier: "A",
		Description: "Major critical schools (formalism, structuralism, post-structuralism, feminist, postcolonial), close-reading techniques."},

	{ID: "linguistics", Name: "Linguistics", Category: "humanities", Tier: "A",
		Description: "Phonology, morphology, syntax, semantics, pragmatics, sociolinguistics, historical + comparative linguistics."},
	{ID: "anthropology", Name: "Anthropology", Category: "humanities", Tier: "A",
		Description: "Cultural anthropology, archaeology, biological anthropology, linguistic anthropology, ethnographic methods."},
	{ID: "sociology", Name: "Sociology", Category: "humanities", Tier: "A",
		Description: "Social structure, institutions, stratification, social change, classical + contemporary theory, methods."},

	{ID: "psych-cognitive", Name: "Cognitive Psychology", Category: "science", Tier: "A",
		Description: "Attention, perception, memory, learning, language processing, reasoning + decision-making, cognitive neuroscience overlap."},
	{ID: "psych-developmental", Name: "Developmental Psychology", Category: "science", Tier: "A",
		Description: "Lifespan development -- infancy through old age, cognitive + social + emotional development, developmental theories."},
	{ID: "psych-social", Name: "Social Psychology", Category: "science", Tier: "A",
		Description: "Attitudes, conformity, group dynamics, persuasion, intergroup relations, social cognition, classic experiments."},
	{ID: "psych-behavioral", Name: "Behavioral Psychology", Category: "science", Tier: "A",
		Description: "Classical + operant conditioning, behavior modification, applied behavior analysis, learning theory."},
	{ID: "psych-clinical-basics", Name: "Clinical Psychology Basics", Category: "science", Tier: "B",
		Description: "Diagnostic frameworks (DSM-5 overview), major therapy modalities (CBT, psychodynamic, humanistic). Educational reference; not a substitute for licensed clinical care."},

	{ID: "econ-micro", Name: "Microeconomics", Category: "humanities", Tier: "A",
		Description: "Supply + demand, consumer + producer theory, market structures, game theory basics, externalities + public goods."},
	{ID: "econ-macro", Name: "Macroeconomics", Category: "humanities", Tier: "A",
		Description: "GDP + national accounts, monetary + fiscal policy, business cycles, inflation, employment, growth theory."},
	{ID: "econ-behavioral", Name: "Behavioral Economics", Category: "humanities", Tier: "A",
		Description: "Cognitive biases, prospect theory, nudges, intertemporal choice, behavioral game theory, applications to policy."},
	{ID: "econ-development", Name: "Development Economics", Category: "humanities", Tier: "A",
		Description: "Growth + poverty, institutional economics, foreign aid effectiveness, RCT methodology, comparative development."},

	{ID: "polisci-us", Name: "U.S. Political Science", Category: "humanities", Tier: "A",
		Description: "U.S. constitutional structure, federalism, branches of government, parties + elections, contemporary political institutions."},
	{ID: "polisci-comparative", Name: "Comparative Politics", Category: "humanities", Tier: "A",
		Description: "Political systems across countries, regime types, democratization, comparative institutions, party systems."},
	{ID: "polisci-international", Name: "International Relations", Category: "humanities", Tier: "A",
		Description: "Theories of IR (realism, liberalism, constructivism), international institutions, security studies, IPE, foreign policy."},

	{ID: "religious-studies", Name: "Religious Studies", Category: "humanities", Tier: "A",
		Description: "World religions (their texts, practices, history), comparative religion, religious philosophy, secular study of religion."},

	// --- Arts & Design --------------------------------------------------------
	{ID: "art-drawing-painting", Name: "Drawing & Painting", Category: "creative", Tier: "A",
		Description: "Drawing fundamentals, color theory, composition, mediums (oil, acrylic, watercolor, ink), historical movements + techniques."},
	{ID: "art-sculpture", Name: "Sculpture", Category: "creative", Tier: "A",
		Description: "Traditional + modern sculpture, materials (clay, stone, metal, wood), techniques (carving, modeling, assemblage), installation."},
	{ID: "art-digital", Name: "Digital Art", Category: "creative", Tier: "A",
		Description: "Vector + raster workflows, common tools (Procreate, Photoshop, Figma), 3D modeling basics, AI-assisted art."},
	{ID: "art-photography-advanced", Name: "Advanced Photography", Category: "creative", Tier: "A",
		Description: "Exposure + lens choice mastery, manual workflows, lighting setups, advanced post-processing, portfolio building, commercial vs editorial."},
	{ID: "art-history", Name: "Art History", Category: "humanities", Tier: "A",
		Description: "Major movements from antiquity to contemporary -- Renaissance, Baroque, Impressionism, Modernism, Postmodernism."},

	{ID: "music-theory", Name: "Music Theory", Category: "creative", Tier: "A",
		Description: "Notation, scales + modes, harmony + chord progressions, counterpoint, form + analysis, ear training basics."},
	{ID: "music-composition", Name: "Music Composition", Category: "creative", Tier: "A",
		Description: "Compositional techniques across genres, melodic + harmonic development, orchestration, songwriting craft, recording basics."},
	{ID: "music-performance", Name: "Music Performance", Category: "creative", Tier: "A",
		Description: "Practice methodologies, performance anxiety management, ensemble playing, instrument-specific technique principles."},
	{ID: "performing-arts", Name: "Performing Arts", Category: "creative", Tier: "A",
		Description: "Theater, dance, opera; performance traditions, training methodologies, production practice."},

	{ID: "architecture", Name: "Architecture", Category: "creative", Tier: "A",
		Description: "Architectural history + theory, design process, structural fundamentals, materials, sustainability, contemporary practice."},
	{ID: "design-industrial", Name: "Industrial Design", Category: "creative", Tier: "A",
		Description: "Product design process, ergonomics, manufacturing constraints, sustainability, prototyping, design + brand integration."},
	{ID: "design-graphic", Name: "Graphic Design", Category: "creative", Tier: "A",
		Description: "Typography, layout, color, identity systems, print + digital media, design principles + history."},
	{ID: "design-ux", Name: "UX Design", Category: "creative", Tier: "A",
		Description: "User research methods, information architecture, interaction design, prototyping, usability testing, design systems."},
	{ID: "design-fashion", Name: "Fashion Design", Category: "creative", Tier: "A",
		Description: "Garment construction, pattern-making, textile knowledge, fashion history, sustainable fashion, industry workflows."},
	{ID: "film-production", Name: "Film + Video Production", Category: "creative", Tier: "A",
		Description: "Camera + lens fundamentals, cinematography, editing, sound design, color grading, production workflows, screenwriting basics."},
	{ID: "game-design", Name: "Game Design", Category: "creative", Tier: "A",
		Description: "Game mechanics, level design, narrative design, playtesting, balance, common engines (Unity / Unreal / Godot) basics."},

	// --- Specialized Fields ---------------------------------------------------
	{ID: "law-constitutional", Name: "Constitutional Law", Category: "specialized", Tier: "B",
		Description: "Foundational constitutional principles + case law (focused on U.S.). Educational reference; consult a licensed attorney for legal advice."},
	{ID: "law-criminal", Name: "Criminal Law", Category: "specialized", Tier: "B",
		Description: "Substantive + procedural criminal law fundamentals. Educational reference; not legal advice."},
	{ID: "law-civil-procedure", Name: "Civil Procedure", Category: "specialized", Tier: "B",
		Description: "Civil litigation procedure, jurisdiction, pleadings, discovery, judgment + appeals. Educational reference; not legal advice."},
	{ID: "law-intellectual-property", Name: "Intellectual Property Law", Category: "specialized", Tier: "B",
		Description: "Copyright, trademark, patent, trade-secret fundamentals. Educational reference; consult an IP attorney for filings + enforcement."},
	{ID: "law-international", Name: "International Law", Category: "specialized", Tier: "B",
		Description: "Public + private international law fundamentals, treaties, human rights, international courts. Educational reference."},
	{ID: "law-environmental", Name: "Environmental Law", Category: "specialized", Tier: "B",
		Description: "Environmental regulatory frameworks (NEPA, CWA, CAA, Superfund), permitting, enforcement basics. Educational reference."},
	{ID: "law-tax", Name: "Tax Law", Category: "specialized", Tier: "B",
		Description: "Federal tax law fundamentals, tax research, common entity-tax topics. Educational reference; consult a CPA / tax attorney for planning + filing."},

	{ID: "edu-pedagogy", Name: "Pedagogy", Category: "humanities", Tier: "A",
		Description: "Teaching methodologies, learning theories (constructivism, behaviorism), classroom management, lesson planning."},
	{ID: "edu-assessment", Name: "Assessment + Evaluation", Category: "humanities", Tier: "A",
		Description: "Formative + summative assessment, rubrics, validity + reliability, standardized testing critique, alternative assessment."},
	{ID: "edu-special", Name: "Special Education", Category: "humanities", Tier: "B",
		Description: "Learning disabilities overview, IEP/504 frameworks, inclusion strategies. Educational reference; consult specialists for individual planning."},
	{ID: "edu-edtech", Name: "Educational Technology", Category: "humanities", Tier: "A",
		Description: "Learning platforms, digital tools, blended + flipped classroom models, accessibility considerations, contemporary EdTech."},

	{ID: "journalism", Name: "Journalism", Category: "humanities", Tier: "A",
		Description: "Reporting practice, interviewing, fact-checking, investigative methods, ethics + standards, media literacy."},

	// --- Sports (per-sport) ---------------------------------------------------
	{ID: "sport-football-american", Name: "Football (American)", Category: "hobby", Tier: "A",
		Description: "Rules, positions, schemes (offensive + defensive), notable history, fantasy basics, NFL + NCAA structure."},
	{ID: "sport-soccer", Name: "Soccer / Football", Category: "hobby", Tier: "A",
		Description: "Rules, positions, formations, major leagues + tournaments (FIFA, UEFA, MLS), notable history."},
	{ID: "sport-basketball", Name: "Basketball", Category: "hobby", Tier: "A",
		Description: "Rules, positions, offensive + defensive systems, NBA + NCAA structure, advanced stats, notable history."},
	{ID: "sport-baseball", Name: "Baseball", Category: "hobby", Tier: "A",
		Description: "Rules, positions, in-game strategy, sabermetrics intro, MLB + minor leagues, notable history."},
	{ID: "sport-tennis", Name: "Tennis", Category: "hobby", Tier: "A",
		Description: "Rules, scoring, technique fundamentals, ATP + WTA tour structure, Grand Slams, notable history."},
	{ID: "sport-golf", Name: "Golf", Category: "hobby", Tier: "A",
		Description: "Rules + etiquette, club selection, course management, scoring formats, PGA + LPGA structure, equipment."},

	// --- Games + Recreation ---------------------------------------------------
	{ID: "games-board", Name: "Board Games", Category: "hobby", Tier: "A",
		Description: "Modern board game design, classic games (chess, Go, backgammon), strategy + Eurogames, social deduction, game-night planning."},
	{ID: "games-video", Name: "Video Games", Category: "hobby", Tier: "A",
		Description: "Major genres + franchises, current platforms, gaming history, e-sports basics, accessibility + parental considerations."},
	{ID: "games-card", Name: "Card Games", Category: "hobby", Tier: "A",
		Description: "Traditional card games (poker, bridge, hearts), trading card games (Magic, Pokemon), solitaire variants, card-game strategy basics."},
	{ID: "games-chess", Name: "Chess", Category: "hobby", Tier: "A",
		Description: "Opening principles + named openings, middlegame strategy, endgame fundamentals, common tactics, ratings + tournament play."},

	{ID: "outdoor-hiking", Name: "Hiking & Backpacking", Category: "hobby", Tier: "B",
		Description: "Trail planning, gear selection, backcountry navigation, weather assessment, basic wilderness safety. Refer to authoritative outdoor-safety sources for emergencies."},
	{ID: "outdoor-camping", Name: "Camping", Category: "hobby", Tier: "A",
		Description: "Site selection, gear, cooking outdoors, weather considerations, family vs solo + car-camping vs backcountry."},
	{ID: "outdoor-climbing", Name: "Climbing", Category: "hobby", Tier: "B",
		Description: "Indoor + outdoor climbing styles (top-rope, lead, bouldering, trad), gear basics, technique, safety fundamentals. Take in-person instruction for outdoor climbing."},
	{ID: "outdoor-fishing", Name: "Fishing", Category: "hobby", Tier: "A",
		Description: "Freshwater + saltwater techniques, gear selection, regulations + licensing, common species, fly-fishing intro."},
	{ID: "outdoor-hunting", Name: "Hunting", Category: "hobby", Tier: "B",
		Description: "Game species, regulations + licensing, firearm + bow safety basics, scouting + field-dressing fundamentals. In-person hunter-safety courses required in most jurisdictions."},

	{ID: "collecting-coins", Name: "Coin Collecting", Category: "hobby", Tier: "A",
		Description: "Numismatics intro, grading, U.S. + world coin identification, storage + protection, key dates, market basics."},
	{ID: "collecting-stamps", Name: "Stamp Collecting", Category: "hobby", Tier: "A",
		Description: "Philately intro, identification, country + topical collecting, condition + grading, market basics."},
	{ID: "collecting-antiques", Name: "Antiques Collecting", Category: "hobby", Tier: "A",
		Description: "Identifying authentic antiques, eras + styles, valuation basics, restoration vs preservation, market dynamics."},

	// =================================================================
	// Role-catalog expansion: domains referenced by standardRoles in
	// integrations/knowledge/role_seed.go. Grouped by category so the
	// reading order matches the role catalog's growth. Tier defaults
	// to "A" unless explicit ("B" or "C"). RelevantForRoles is left
	// empty for these -- the role catalog's lockedDomainIds /
	// defaultDomainIds is the source of truth and the seeder
	// propagates lockedForRoles back onto the domain via
	// lockedRolesForDomain.
	// =================================================================

	// --- Professional / business (additions) ---
	{ID: "strategic-planning", Name: "Strategic Planning",
		Description: "Long-horizon planning, OKRs, scenario modelling -- the executive-assistant-grade reference."},
	{ID: "executive-calendar", Name: "Executive Calendar Management",
		Description: "Calendar discipline: meeting cadences, time-block strategies, exec inbox triage, travel-day blocking, focus-time protection."},
	{ID: "meeting-management", Name: "Meeting Management",
		Description: "Agendas, decisions, action items, follow-ups, meeting hygiene, async alternatives, post-mortems."},
	{ID: "business-analysis", Name: "Business Analysis",
		Description: "Requirements elicitation, stakeholder facilitation, process mapping, gap analysis, impact analysis."},
	{ID: "requirements-engineering", Name: "Requirements Engineering",
		Description: "Eliciting, specifying, validating, and managing requirements through a project lifecycle."},
	{ID: "process-modeling", Name: "Process Modeling",
		Description: "BPMN, value stream mapping, swim lanes, RACI; representing how work moves through a system."},
	{ID: "demand-planning", Name: "Demand Planning",
		Description: "Forecasting demand, S&OP, statistical + judgmental forecasting methods, accuracy measurement."},
	{ID: "procurement-sourcing", Name: "Procurement & Sourcing",
		Description: "RFx process, supplier evaluation, contract negotiation, strategic sourcing categories."},
	{ID: "product-management", Name: "Product Management",
		Description: "Roadmaps, prioritization, customer-development, product discovery, GTM coordination, OKRs."},
	{ID: "user-research", Name: "User Research",
		Description: "Interview methods, surveys, usability studies, diary studies, synthesis + insight generation."},
	{ID: "prioritization-frameworks", Name: "Prioritization Frameworks",
		Description: "RICE, MoSCoW, WSJF, Kano, value-vs-effort matrices, criteria-based prioritization."},

	// --- Technical / engineering (additions) ---
	{ID: "frontend-development", Name: "Frontend Development",
		Description: "Web UI engineering: rendering, state, routing, build tooling, browser APIs, component frameworks."},
	{ID: "ux-principles", Name: "UX Principles",
		Description: "Heuristics, mental models, information architecture, affordances, cognitive load, design ethics."},
	{ID: "accessibility", Name: "Accessibility (a11y)",
		Description: "WCAG, screen-reader support, keyboard nav, focus management, ARIA, inclusive design patterns."},
	{ID: "css-styling", Name: "CSS & Styling",
		Description: "Selectors, cascade, layout (flex/grid), typography, responsive design, CSS-in-JS patterns, design tokens."},
	{ID: "javascript-typescript", Name: "JavaScript & TypeScript",
		Description: "Language fundamentals, async patterns, type systems, module systems, tooling, performance."},
	{ID: "performance-optimization", Name: "Performance Optimization",
		Description: "Profiling, measurement, hot-path tuning, caching strategies, network optimization, bundle hygiene."},
	{ID: "design-systems", Name: "Design Systems",
		Description: "Token systems, component libraries, governance, docs, contribution + adoption patterns."},
	{ID: "api-design", Name: "API Design",
		Description: "REST, gRPC, GraphQL; resource modeling, versioning, pagination, error semantics, idempotency."},
	{ID: "system-architecture", Name: "System Architecture",
		Description: "Service decomposition, data ownership, scalability patterns, fault isolation, CAP, consistency models."},
	{ID: "version-control", Name: "Version Control (Git)",
		Description: "Branching strategies, merges + rebases, conflict resolution, code-review workflow, hooks."},
	{ID: "testing-strategy", Name: "Testing Strategy",
		Description: "Test pyramid, unit + integration + e2e, fakes vs mocks, property-based testing, test maintenance."},
	{ID: "code-review", Name: "Code Review",
		Description: "Reviewing for correctness, design, security, readability; giving and receiving feedback well."},
	{ID: "design-patterns", Name: "Design Patterns",
		Description: "GoF and modern patterns, anti-patterns, when patterns help vs hurt, language-idiomatic alternatives."},
	{ID: "infrastructure-as-code", Name: "Infrastructure as Code",
		Description: "Terraform, Pulumi, CloudFormation; module design, state management, drift detection."},
	{ID: "container-orchestration", Name: "Container Orchestration",
		Description: "Kubernetes fundamentals, workloads, services, ingress, secrets, scaling, multi-tenancy."},
	{ID: "observability-monitoring", Name: "Observability & Monitoring",
		Description: "Logs, metrics, traces; SLO/SLI, alerting fatigue, dashboards that answer questions."},
	{ID: "incident-response", Name: "Incident Response", Tier: "B",
		Description: "Runbooks, severity classification, comms cadence, blameless post-mortems, paging hygiene."},
	{ID: "ci-cd-pipelines", Name: "CI/CD Pipelines",
		Description: "Build / test / package / deploy pipelines, caching, secrets, artifact management, release strategies."},
	{ID: "site-reliability", Name: "Site Reliability Engineering",
		Description: "Toil reduction, error budgets, capacity planning, chaos engineering, on-call practices."},
	{ID: "application-security", Name: "Application Security", Tier: "B",
		Description: "OWASP top 10, secure coding, threat modeling at the app layer, dependency hygiene, secrets management."},
	{ID: "threat-modeling", Name: "Threat Modeling", Tier: "B",
		Description: "STRIDE, attack trees, data-flow diagrams, trust boundaries, mitigation prioritization."},
	{ID: "cryptography-basics", Name: "Cryptography Basics",
		Description: "Hashing, symmetric / asymmetric crypto, signatures, TLS, key management at a working level."},
	{ID: "vulnerability-management", Name: "Vulnerability Management", Tier: "B",
		Description: "CVE triage, patch SLAs, vulnerability scanning, exception processes, compensating controls."},
	{ID: "identity-access-management", Name: "Identity & Access Management",
		Description: "Authn vs authz, OIDC, OAuth2, SAML, RBAC vs ABAC, secrets / key rotation, least privilege."},

	// --- Data + analytics specialties ---
	{ID: "data-visualization", Name: "Data Visualization",
		Description: "Chart-type selection, perceptual principles, color, layout, dashboard design, story-with-data."},
	{ID: "experimental-design", Name: "Experimental Design",
		Description: "A/B testing, power analysis, randomization, controlling for confounders, sequential testing."},
	{ID: "sql-databases", Name: "SQL & Databases",
		Description: "Relational modeling, query writing, indexing, transactions, query plans, common engines."},
	{ID: "business-intelligence", Name: "Business Intelligence",
		Description: "BI platforms, semantic layers, self-service analytics, governance, KPI definition discipline."},
	{ID: "data-engineering", Name: "Data Engineering",
		Description: "Pipelines, batch + streaming, schema evolution, data quality, lineage, cost-awareness."},
	{ID: "machine-learning", Name: "Machine Learning",
		Description: "Supervised / unsupervised methods, feature engineering, model selection, evaluation, deployment."},
	{ID: "deep-learning", Name: "Deep Learning",
		Description: "Neural network architectures, training tricks, transfer learning, attention, modern frameworks."},
	{ID: "statistical-modeling", Name: "Statistical Modeling",
		Description: "Linear + GLM, mixed models, Bayesian methods, time-series basics, model checking."},
	{ID: "python-programming", Name: "Python Programming",
		Description: "Language features, idioms, standard library, packaging, virtualenv / poetry, async, testing."},
	{ID: "etl-pipelines", Name: "ETL / ELT Pipelines",
		Description: "Source -> transform -> sink architectures, orchestration tools, dependency graphs, retries."},
	{ID: "streaming-systems", Name: "Streaming Data Systems",
		Description: "Kafka, Pulsar, exactly-once semantics, watermarks, windowing, stream-table joins."},
	{ID: "data-warehouse-design", Name: "Data Warehouse Design",
		Description: "Star + snowflake schemas, slowly-changing dimensions, OLAP vs OLTP, modern lakehouse patterns."},
	{ID: "data-governance", Name: "Data Governance",
		Description: "Ownership, classification, quality SLAs, lineage, privacy + retention, regulatory mapping."},

	// --- Writing + comms ---
	{ID: "technical-writing", Name: "Technical Writing",
		Description: "Audience analysis, structure, voice, conventions for API docs / runbooks / how-tos."},
	{ID: "documentation-systems", Name: "Documentation Systems",
		Description: "Docs-as-code, static site generators, versioning, search, doc reviews, info architecture."},
	{ID: "content-strategy", Name: "Content Strategy",
		Description: "Audience modeling, content lifecycle, editorial governance, voice + tone, content audits."},
	{ID: "english-grammar-style", Name: "English Grammar & Style",
		Description: "Standard grammar, usage references, common style guides (AP, Chicago), clarity + concision."},

	// --- UX + design extensions ---
	{ID: "prototyping-tools", Name: "Prototyping Tools",
		Description: "Figma + alternatives: component libraries, prototyping flows, design tokens, handoff."},
	{ID: "interaction-design", Name: "Interaction Design",
		Description: "Motion, micro-interactions, gestural patterns, input modalities, feedback loops."},
	{ID: "visual-design", Name: "Visual Design",
		Description: "Hierarchy, balance, contrast, scale, type pairing, grid systems, mood + brand expression."},
	{ID: "graphic-design", Name: "Graphic Design",
		Description: "Print + digital fundamentals, layout, typography, color, identity systems."},
	{ID: "typography", Name: "Typography",
		Description: "Type anatomy, families, classifications, hierarchy, spacing, pairing, type for the web."},
	{ID: "color-theory", Name: "Color Theory",
		Description: "Color wheels, harmony, contrast, color spaces, accessibility-aware palettes."},
	{ID: "layout-design", Name: "Layout Design",
		Description: "Grid systems, modular scales, balance and rhythm, responsive layouts, multi-device design."},
	{ID: "illustration", Name: "Illustration",
		Description: "Drawing for communication: editorial, technical, character, environment, style development."},
	{ID: "drawing-fundamentals", Name: "Drawing Fundamentals",
		Description: "Line, shape, value, perspective, anatomy, composition, gesture, observational drawing."},
	{ID: "digital-illustration-tools", Name: "Digital Illustration Tools",
		Description: "Procreate, Photoshop, Illustrator, Affinity: brushes, layers, vector vs raster, color management."},
	{ID: "composition-principles", Name: "Composition Principles",
		Description: "Rule of thirds, leading lines, framing, balance, golden ratio in visual + photo work."},
	{ID: "lighting-techniques", Name: "Lighting Techniques",
		Description: "Natural + artificial light, key/fill/rim, three-point lighting, color temperature, mood lighting."},
	{ID: "photo-editing", Name: "Photo Editing",
		Description: "RAW workflow, tonal + color adjustments, retouching, dodging + burning, output for print + web."},
	{ID: "camera-systems", Name: "Camera Systems",
		Description: "Exposure triangle, focal length, sensor sizes, lens choice, formats (DSLR / mirrorless / phone)."},

	// --- Video + audio production ---
	{ID: "video-editing", Name: "Video Editing",
		Description: "NLE workflow, cut theory, pacing, dialogue editing, transitions, multi-cam, export ladders."},
	{ID: "video-production", Name: "Video Production",
		Description: "Pre-production, shoot logistics, lighting, audio, directing talent, asset management."},
	{ID: "color-grading-basics", Name: "Color Grading Basics",
		Description: "Primary + secondary correction, LUTs, scopes, color spaces, look development, matching shots."},
	{ID: "audio-post", Name: "Audio Post-Production",
		Description: "Dialogue cleanup, ADR, foley, sound design, mixing for stereo + surround, loudness targets."},
	{ID: "post-workflow", Name: "Post-Production Workflow",
		Description: "Project organization, proxy workflows, asset management, color/sound roundtrips, deliverables."},

	// --- Writing genres ---
	{ID: "creative-writing", Name: "Creative Writing",
		Description: "Craft across fiction, poetry, essay; voice, structure, revision, workshop practice."},
	{ID: "fiction-craft", Name: "Fiction Craft",
		Description: "Character, plot, scene + sequel, POV, pacing, theme, revision strategies."},
	{ID: "story-structure", Name: "Story Structure",
		Description: "Three-act, five-act, hero's journey, beat sheets; how structure serves emotional payoff."},
	{ID: "character-development", Name: "Character Development",
		Description: "Want vs need, arcs, backstory, voice, relationships, contradictions, on-page presence."},
	{ID: "publishing-industry", Name: "Publishing Industry",
		Description: "Trade vs indie publishing, agents + queries, contracts, marketing, royalties, rights."},
	{ID: "screenwriting", Name: "Screenwriting",
		Description: "Feature + TV format, structure, dialogue, scene craft, the development pipeline."},
	{ID: "dialogue-craft", Name: "Dialogue Craft",
		Description: "Subtext, voice, beats, dialogue that does double-duty for character + plot."},
	{ID: "film-industry", Name: "Film Industry",
		Description: "Above-the-line / below-the-line, financing, distribution, festival circuit, guild basics."},
	{ID: "tv-industry", Name: "Television Industry",
		Description: "Network / streaming / cable, writers' rooms, showrunner role, season + episode structure."},
	{ID: "songwriting", Name: "Songwriting",
		Description: "Lyric + melody, song forms, hooks, prosody, co-writing dynamics, demo workflow."},
	{ID: "lyric-writing", Name: "Lyric Writing",
		Description: "Imagery, rhyme schemes, prosody, point of view, narrative + non-narrative lyrics."},
	{ID: "music-theory", Name: "Music Theory",
		Description: "Scales, intervals, harmony, voice leading, form analysis at increasing levels of depth."},
	{ID: "ear-training", Name: "Ear Training",
		Description: "Interval ID, chord recognition, melodic + harmonic dictation, transcription practice."},
	{ID: "music-history", Name: "Music History",
		Description: "Western art music + world traditions: eras, major works, composers, performance practice."},
	{ID: "composition-fundamentals", Name: "Composition Fundamentals",
		Description: "Motivic development, voice leading, orchestration basics, form, study scores."},
	{ID: "music-production-basics", Name: "Music Production Basics",
		Description: "DAW workflow, arrangement, mixing, mastering essentials, sound design for music."},
	{ID: "music-pedagogy", Name: "Music Pedagogy",
		Description: "Teaching method books, lesson planning, practice strategies, age-appropriate progressions."},

	// --- Game design ---
	{ID: "game-design", Name: "Game Design",
		Description: "Systems, mechanics, progression, economy, narrative integration, playtesting."},
	{ID: "game-mechanics", Name: "Game Mechanics",
		Description: "Core loops, verbs, feedback, complexity ramps, anti-frustration features, polish."},
	{ID: "level-design", Name: "Level Design",
		Description: "Spatial pacing, sightlines, encounter design, tutorialization, environmental storytelling."},
	{ID: "game-narrative", Name: "Game Narrative",
		Description: "Interactive storytelling, branching dialogue, environmental narrative, agency vs authorship."},
	{ID: "playtesting", Name: "Playtesting",
		Description: "Recruiting testers, structuring sessions, observation, surveys, iterating on feedback."},

	// --- Education layers ---
	{ID: "elementary-pedagogy", Name: "Elementary Pedagogy",
		Description: "K-5 instructional methods, developmentally appropriate practice, differentiation."},
	{ID: "early-literacy", Name: "Early Literacy",
		Description: "Phonemic awareness, phonics, fluency, vocabulary, comprehension at K-3 levels."},
	{ID: "elementary-math", Name: "Elementary Mathematics",
		Description: "Number sense, operations, fractions, measurement, early geometry, word problems."},
	{ID: "classroom-management", Name: "Classroom Management",
		Description: "Routines, transitions, behavior systems, student engagement, conflict de-escalation."},
	{ID: "middle-school-pedagogy", Name: "Middle School Pedagogy",
		Description: "Grades 6-8 methods, adolescent development, content-area literacy, project-based learning."},
	{ID: "prealgebra-algebra", Name: "Prealgebra & Algebra I",
		Description: "Variables, expressions, linear equations, systems, polynomials at middle / early high school level."},
	{ID: "study-skills", Name: "Study Skills",
		Description: "Time management, note-taking systems, spaced practice, retrieval, test-prep habits."},
	{ID: "high-school-pedagogy", Name: "High School Pedagogy",
		Description: "Grades 9-12 methods, AP / IB pathways, college prep, content-area depth + breadth."},
	{ID: "algebra", Name: "Algebra II",
		Description: "Functions, polynomials, rational expressions, exponential + log, sequences + series."},
	{ID: "geometry-trigonometry", Name: "Geometry & Trigonometry",
		Description: "Euclidean geometry, coordinate geometry, triangle trig, unit circle, identities."},
	{ID: "biology", Name: "Biology",
		Description: "Cell, molecular, organismal, ecology, evolution; high school + introductory college."},
	{ID: "chemistry", Name: "Chemistry",
		Description: "General chemistry: atoms, bonding, stoichiometry, equilibrium, acid-base, thermochemistry."},
	{ID: "physics-high-school", Name: "Physics (High School)",
		Description: "Algebra-based physics: kinematics, forces, energy, momentum, waves, basic E&M."},
	{ID: "english-literature", Name: "English Literature",
		Description: "Literary analysis, major works + periods, rhetoric, composition for academic writing."},
	{ID: "world-history", Name: "World History",
		Description: "Major civilizations, eras, themes; pre-modern to modern, regional + comparative perspectives."},
	{ID: "us-history", Name: "U.S. History",
		Description: "Founding through present: political, social, economic, cultural threads + key events."},
	{ID: "test-prep-strategy", Name: "Test Prep Strategy",
		Description: "Pacing, elimination, when to skip, anxiety management, score-target backwards-mapping."},
	{ID: "sat-act-content", Name: "SAT / ACT Content",
		Description: "Math + verbal content scope, common question types, official-prep resource discipline."},
	{ID: "gre-gmat-content", Name: "GRE / GMAT Content",
		Description: "Quant, verbal, AWA, analytical writing; content scope + question patterns."},
	{ID: "calculus", Name: "Calculus",
		Description: "Limits, derivatives, integrals, series; single and multivariable, applications."},
	{ID: "precalculus", Name: "Precalculus",
		Description: "Functions, conics, polar + parametric, sequences, intro to limits."},
	{ID: "linear-algebra", Name: "Linear Algebra",
		Description: "Vectors, matrices, linear systems, eigenvalues, decompositions, applications."},
	{ID: "math-problem-solving", Name: "Mathematical Problem Solving",
		Description: "Heuristics, working backwards, invariants, generalization; competition + classic problems."},

	// --- Languages ---
	{ID: "spanish-language", Name: "Spanish Language",
		Description: "Grammar, vocabulary, conversation, reading + writing at CEFR A1 -> C2."},
	{ID: "spanish-culture", Name: "Spanish-Speaking Cultures",
		Description: "Latin American + Iberian culture, regional variation, history, arts, customs."},
	{ID: "language-pedagogy", Name: "Language Teaching Pedagogy",
		Description: "Comprehensible input, CI methods, task-based teaching, assessment, motivation."},
	{ID: "mandarin-chinese", Name: "Mandarin Chinese",
		Description: "Tones, pinyin, characters, grammar, conversation, reading + writing at CEFR A1 -> C2."},
	{ID: "chinese-culture", Name: "Chinese Cultures",
		Description: "Mainland + Taiwan + diaspora: history, philosophy, arts, modern context."},
	{ID: "french-language", Name: "French Language",
		Description: "Grammar, vocabulary, conversation, reading + writing at CEFR A1 -> C2."},
	{ID: "french-culture", Name: "French-Speaking Cultures",
		Description: "Metropolitan + Quebec + Francophone Africa: history, arts, customs, modern context."},

	// --- Sciences (broad survey) ---
	{ID: "physics", Name: "Physics",
		Description: "Undergrad-survey physics: mechanics + E&M + thermo + waves + intro modern physics.",
		BroadSurvey: true},
	{ID: "classical-mechanics", Name: "Classical Mechanics",
		Description: "Newtonian + Lagrangian + Hamiltonian formalism, rigid bodies, oscillations, central forces."},
	{ID: "electromagnetism", Name: "Electromagnetism",
		Description: "Electrostatics, magnetostatics, Maxwell's equations, EM waves, materials, optics."},
	{ID: "thermodynamics", Name: "Thermodynamics & Statistical Mechanics",
		Description: "Laws of thermo, entropy, free energy, ensembles, kinetic theory, phase transitions."},
	{ID: "quantum-mechanics", Name: "Quantum Mechanics",
		Description: "States, operators, measurement, harmonic oscillator, hydrogen atom, perturbation theory."},
	{ID: "organic-chemistry", Name: "Organic Chemistry",
		Description: "Functional groups, mechanisms, stereochemistry, synthesis, spectroscopy."},
	{ID: "inorganic-chemistry", Name: "Inorganic Chemistry",
		Description: "Periodic trends, bonding, coordination chemistry, organometallics, solid state basics."},
	{ID: "physical-chemistry", Name: "Physical Chemistry",
		Description: "Thermodynamics, kinetics, quantum chemistry, spectroscopy, statistical mechanics."},
	{ID: "biochemistry", Name: "Biochemistry",
		Description: "Biomolecules, enzymes, metabolism, gene expression, signaling."},
	{ID: "cell-biology", Name: "Cell Biology",
		Description: "Organelles, membrane transport, cell cycle, signal transduction, cytoskeleton."},
	{ID: "molecular-biology", Name: "Molecular Biology",
		Description: "DNA replication, transcription, translation, regulation, recombinant techniques."},
	{ID: "genetics", Name: "Genetics",
		Description: "Mendelian + population genetics, linkage, recombination, genomics, modern methods."},
	{ID: "ecology", Name: "Ecology",
		Description: "Populations, communities, ecosystems, biodiversity, conservation biology."},
	{ID: "evolution", Name: "Evolution",
		Description: "Natural selection, drift, speciation, phylogenetics, macroevolution + microevolution."},
	{ID: "astronomy", Name: "Astronomy",
		Description: "Solar system, stars, galaxies, observational methods, modern instruments + missions.",
		BroadSurvey: true},
	{ID: "astrophysics", Name: "Astrophysics",
		Description: "Stellar structure + evolution, compact objects, interstellar medium, high-energy phenomena."},
	{ID: "cosmology", Name: "Cosmology",
		Description: "Big bang, CMB, structure formation, dark matter, dark energy, inflation."},
	{ID: "planetary-science", Name: "Planetary Science",
		Description: "Formation, dynamics, atmospheres, surfaces, habitability, exoplanets."},
	{ID: "geology", Name: "Geology",
		Description: "Rocks + minerals, plate tectonics, geomorphology, stratigraphy, earth history.",
		BroadSurvey: true},
	{ID: "mineralogy", Name: "Mineralogy",
		Description: "Crystal systems, mineral ID, optical mineralogy, common rock-forming minerals."},
	{ID: "plate-tectonics", Name: "Plate Tectonics",
		Description: "Plate motions, boundaries, hot spots, mountain building, seafloor spreading."},
	{ID: "earth-history", Name: "Earth History",
		Description: "Geologic time, mass extinctions, climate history, fossil record, plate reconstructions."},

	// --- Humanities ---
	{ID: "european-history", Name: "European History",
		Description: "Ancient through modern Europe: empires, reformations, revolutions, modern states.",
		BroadSurvey: true},
	{ID: "ancient-history", Name: "Ancient History",
		Description: "Mesopotamia, Egypt, Greece, Rome, China, India, Mesoamerica: founding civilizations.",
		BroadSurvey: true},
	{ID: "historiography", Name: "Historiography",
		Description: "How history is written: methods, schools of thought, evidence, primary vs secondary."},
	{ID: "philosophy", Name: "Philosophy",
		Description: "Major branches + figures across Western + non-Western traditions.",
		BroadSurvey: true},
	{ID: "ethics", Name: "Ethics",
		Description: "Major frameworks (virtue, deontology, consequentialism), applied ethics, metaethics."},
	{ID: "logic", Name: "Logic",
		Description: "Propositional + predicate logic, soundness + completeness, informal fallacies, proof methods."},
	{ID: "epistemology", Name: "Epistemology",
		Description: "Theories of knowledge, justification, skepticism, social epistemology, evidence."},
	{ID: "metaphysics", Name: "Metaphysics",
		Description: "Being, identity, time, causation, possibility, free will, mind."},
	{ID: "philosophy-history", Name: "History of Philosophy",
		Description: "Ancient + medieval + modern + contemporary; major figures and the conversations between them."},

	// --- Medical (Tier C unless noted) ---
	{ID: "family-medicine", Name: "Family Medicine", Tier: "C",
		Description: "Primary care across age groups: common conditions, preventive care, chronic disease management."},
	{ID: "patient-communication", Name: "Patient Communication", Tier: "B",
		Description: "Patient-centered communication, motivational interviewing, shared decision making."},
	{ID: "clinical-pharmacology-basics", Name: "Clinical Pharmacology (Basics)", Tier: "C",
		Description: "Drug classes, mechanism, common interactions at a textbook level. Not personalized regimen guidance."},
	{ID: "preventive-medicine", Name: "Preventive Medicine", Tier: "B",
		Description: "Screening guidelines, vaccinations, risk factor modification, public-health basics."},
	{ID: "common-chronic-conditions", Name: "Common Chronic Conditions", Tier: "C",
		Description: "Hypertension, diabetes, asthma, common cardiovascular conditions at a patient-education level."},
	{ID: "pediatrics", Name: "Pediatrics", Tier: "C",
		Description: "Child health: development, common illnesses, immunization schedules, anticipatory guidance."},
	{ID: "vaccination-schedules", Name: "Vaccination Schedules", Tier: "B",
		Description: "ACIP-aligned childhood + adult schedules at an educational level. Always defer to a clinician."},
	{ID: "dentistry", Name: "Dentistry", Tier: "C",
		Description: "Oral health, preventive dentistry, common conditions, terminology at an educational level."},
	{ID: "oral-health", Name: "Oral Health",
		Description: "Brushing + flossing technique, diet impacts, common myths, when to see a dentist."},
	{ID: "physical-therapy", Name: "Physical Therapy", Tier: "C",
		Description: "Movement principles, common rehab approaches, education on safe progression."},
	{ID: "musculoskeletal-health", Name: "Musculoskeletal Health", Tier: "B",
		Description: "Joints, muscles, posture, common injuries, ergonomic + activity guidance at education level."},
	{ID: "sports-medicine-basics", Name: "Sports Medicine (Basics)", Tier: "B",
		Description: "Common sports injuries, recovery + return-to-play principles, education only."},
	{ID: "ergonomics", Name: "Ergonomics",
		Description: "Workplace ergonomics, posture, equipment setup, repetitive-strain prevention basics."},
	{ID: "nursing-fundamentals", Name: "Nursing Fundamentals", Tier: "C",
		Description: "Patient assessment, vital signs, infection control, basic care -- nursing-school level material."},
	{ID: "drug-interactions-overview", Name: "Drug Interactions (Overview)", Tier: "C",
		Description: "Common interaction patterns at an educational level. Defer to a pharmacist for any specific regimen."},
	{ID: "veterinary-medicine", Name: "Veterinary Medicine", Tier: "C",
		Description: "Companion-animal health, common conditions, preventive care at an owner-education level."},
	{ID: "animal-nutrition", Name: "Animal Nutrition",
		Description: "Diets across companion + livestock species, common deficiencies, life-stage considerations."},
	{ID: "psychology-fundamentals", Name: "Psychology Fundamentals",
		Description: "Major theories, learning, memory, development, social psychology, research methods."},
	{ID: "stress-management", Name: "Stress Management",
		Description: "Coping skills, behavioral techniques, sleep + activity + nutrition lifestyle anchors."},

	// --- Legal sub-specialties ---
	{ID: "immigration-law", Name: "Immigration Law", Tier: "B",
		Description: "Visa categories, naturalization, asylum, removal proceedings at an educational level."},
	{ID: "family-law", Name: "Family Law", Tier: "B",
		Description: "Divorce, custody, adoption, prenups at an educational level. Jurisdiction-specific in practice."},

	// --- Trades ---
	{ID: "construction-fundamentals", Name: "Construction Fundamentals",
		Description: "Materials, methods, sequencing, common assemblies, residential + light-commercial framing."},
	{ID: "building-codes", Name: "Building Codes",
		Description: "IBC + IRC overview, common code requirements, when permits are needed, inspection processes."},
	{ID: "trades-safety", Name: "Trades Safety", Tier: "B",
		Description: "OSHA basics, PPE, ladder + scaffold safety, electrical safety, lockout/tagout."},
	{ID: "estimating", Name: "Construction Estimating",
		Description: "Quantity takeoff, unit pricing, markup, contingency, common estimating tools."},
	{ID: "subcontractor-management", Name: "Subcontractor Management",
		Description: "Bidding, contracts, scheduling, coordination, dispute resolution, payments."},
	{ID: "electrical-trades", Name: "Electrical Trades", Tier: "B",
		Description: "Residential + commercial electrical work principles. Licensed labor in most jurisdictions."},
	{ID: "circuit-design-basics", Name: "Circuit Design Basics",
		Description: "Ohm's law, AC + DC fundamentals, common residential circuit configurations."},
	{ID: "electrical-troubleshooting", Name: "Electrical Troubleshooting",
		Description: "Systematic fault isolation, common failure modes, meter use, safe practices."},
	{ID: "plumbing-trades", Name: "Plumbing Trades",
		Description: "Drain / waste / vent + supply systems, fixtures, common repairs, code basics."},
	{ID: "water-systems", Name: "Water Systems",
		Description: "Municipal + well systems, pressure + flow basics, water treatment, common problems."},
	{ID: "hvac-systems", Name: "HVAC Systems",
		Description: "Heating + ventilation + cooling principles, sizing, controls, common faults, refrigerants."},
	{ID: "energy-efficiency", Name: "Energy Efficiency",
		Description: "Envelope + mechanical + lighting strategies, audits, common upgrades, ROI thinking."},
	{ID: "refrigerant-handling", Name: "Refrigerant Handling", Tier: "B",
		Description: "Regulations (EPA 608), recovery + recycling, common refrigerants, safety."},
	{ID: "carpentry", Name: "Carpentry",
		Description: "Framing + finish work, hand + power tools, joinery, common assemblies."},
	{ID: "woodworking", Name: "Woodworking",
		Description: "Hand + machine woodworking, joinery techniques, wood selection, finishing."},
	{ID: "joinery-techniques", Name: "Joinery Techniques",
		Description: "Mortise + tenon, dovetail, lap, biscuit, pocket-screw; when to use each."},
	{ID: "automotive-repair", Name: "Automotive Repair",
		Description: "Passenger-car repair: engine, drivetrain, brakes, suspension, electrical, diagnostics."},
	{ID: "vehicle-diagnostics", Name: "Vehicle Diagnostics",
		Description: "OBD-II, scan tools, common fault codes, systematic troubleshooting approaches."},
	{ID: "engine-systems", Name: "Engine Systems",
		Description: "Internal combustion fundamentals, fuel + ignition + emissions, common failure modes."},
	{ID: "vehicle-electrical", Name: "Vehicle Electrical Systems",
		Description: "12V systems, batteries, charging, starting, body electrical, network buses."},
	{ID: "welding", Name: "Welding",
		Description: "MIG, TIG, stick, flux-core; metallurgy basics, joint design, safety, common defects."},
	{ID: "metallurgy-basics", Name: "Metallurgy Basics",
		Description: "Metal properties, alloys, heat treatment, common materials in fabrication."},
	{ID: "weld-inspection", Name: "Weld Inspection",
		Description: "Visual inspection, common discontinuities, NDT methods overview, code basics."},
	{ID: "landscaping", Name: "Landscaping",
		Description: "Design, installation, maintenance: plants + hardscape + irrigation + lighting."},
	{ID: "horticulture", Name: "Horticulture",
		Description: "Plant biology + propagation + production: ornamental, fruit, vegetable."},
	{ID: "plant-propagation", Name: "Plant Propagation",
		Description: "Seed starting, cuttings, division, grafting; commercial + home techniques."},
	{ID: "greenhouse-management", Name: "Greenhouse Management",
		Description: "Climate control, irrigation, fertility, pest management, crop planning."},
	{ID: "irrigation-systems", Name: "Irrigation Systems",
		Description: "Drip + spray + subsurface, scheduling, water budgeting, common system layouts."},
	{ID: "lawn-care", Name: "Lawn Care",
		Description: "Turf species, mowing + fertility + irrigation + pest, renovation, seasonal calendars."},
	{ID: "integrated-pest-management", Name: "Integrated Pest Management",
		Description: "Monitoring, thresholds, cultural + biological + chemical controls, IPM decision-making."},

	// --- Sports / fitness ---
	{ID: "exercise-science", Name: "Exercise Science", Tier: "B",
		Description: "Anatomy + physiology of training, periodization concepts, common protocols."},
	{ID: "stretching-mobility", Name: "Stretching & Mobility",
		Description: "Static + dynamic stretching, mobility drills, joint-by-joint approach, programming."},
	{ID: "strength-training", Name: "Strength Training",
		Description: "Major lifts, programming approaches, periodization, recovery + form principles."},
	{ID: "running-training", Name: "Running Training",
		Description: "Training plans (5K -> marathon), pace zones, form, common injuries, racing."},
	{ID: "race-nutrition", Name: "Race Nutrition",
		Description: "Fueling for endurance: carbs/protein/fat, hydration, GI strategy, race-day planning."},
	{ID: "injury-prevention-running", Name: "Injury Prevention (Running)", Tier: "B",
		Description: "Common running injuries, prevention principles, return-to-running progressions."},
	{ID: "yoga-practice", Name: "Yoga Practice",
		Description: "Asana + pranayama, sequencing, alignment, common styles, history + philosophy basics."},
	{ID: "meditation", Name: "Meditation",
		Description: "Concentration + insight + loving-kindness traditions, posture, breath, mind training."},
	{ID: "anatomy-basics", Name: "Human Anatomy (Basics)",
		Description: "Major systems, bones, muscles, joints; movement vocabulary for fitness + yoga teachers."},
	{ID: "yoga-philosophy", Name: "Yoga Philosophy",
		Description: "Yoga sutras, eight limbs, history of major lineages, contemporary practice frameworks."},
	{ID: "powerlifting", Name: "Powerlifting", Tier: "B",
		Description: "Squat / bench / deadlift technique, programming, federations, competition basics."},

	// --- Hospitality ---
	{ID: "culinary-technique", Name: "Culinary Technique",
		Description: "Knife skills, mother sauces, cooking methods, seasoning, mise en place."},
	{ID: "menu-engineering", Name: "Menu Engineering",
		Description: "Cost-of-goods, contribution margin, menu design + psychology, profitability analysis."},
	{ID: "food-safety", Name: "Food Safety", Tier: "B",
		Description: "ServSafe topics: temperatures, cross-contamination, allergens, sanitation."},
	{ID: "kitchen-management", Name: "Kitchen Management",
		Description: "Brigade system, scheduling, inventory, prep lists, line setup, expediting."},
	{ID: "food-costing", Name: "Food Costing",
		Description: "Recipe costing, portion control, waste tracking, vendor negotiation, food cost percentage."},
	{ID: "wine-education", Name: "Wine Education",
		Description: "Regions, grape varieties, viticulture, vinification, service + tasting fundamentals."},
	{ID: "food-pairing", Name: "Food & Wine Pairing",
		Description: "Pairing principles, regional traditions, cocktail + spirit pairing, sensory thinking."},
	{ID: "service-etiquette", Name: "Service Etiquette",
		Description: "Formal + casual service, hospitality fundamentals, problem resolution, training service staff."},
	{ID: "cellar-management", Name: "Cellar Management",
		Description: "Inventory, storage conditions, rotation, pricing strategy, list construction."},
	{ID: "coffee-specialty", Name: "Specialty Coffee",
		Description: "Origins, processing, roasting basics, brewing methods, espresso, milk technique."},
	{ID: "brewing-methods", Name: "Brewing Methods",
		Description: "Drip, pour-over, French press, Aeropress, immersion + percolation principles."},
	{ID: "espresso-technique", Name: "Espresso Technique",
		Description: "Grind + dose + tamp + extraction, machine operation, common faults, dialing in."},
	{ID: "milk-steaming", Name: "Milk Steaming",
		Description: "Texture, temperature, latte art fundamentals, common defects + corrections."},
	{ID: "event-planning", Name: "Event Planning",
		Description: "Project management for events: budget, vendors, timelines, run-of-show, post-event review."},
	{ID: "venue-management", Name: "Venue Management",
		Description: "Booking, scheduling, capacity + safety, vendor coordination, day-of operations."},

	// --- Civic ---
	{ID: "civic-services", Name: "Civic Services",
		Description: "Local + state + federal services orientation, where-to-go-for-what for common needs."},
	{ID: "government-benefits", Name: "Government Benefits",
		Description: "Social Security, Medicare, Medicaid, SNAP, unemployment: eligibility + application basics."},
	{ID: "voting-election-basics", Name: "Voting & Elections",
		Description: "Voter registration, polling, ballots, election cycles, civic participation fundamentals."},
	{ID: "nonprofit-management", Name: "Nonprofit Management",
		Description: "Mission, governance, programs, finances, fundraising, evaluation."},
	{ID: "grant-writing", Name: "Grant Writing",
		Description: "Research, LOIs, full proposals, budgets, reporting, building a grants calendar."},
	{ID: "fundraising", Name: "Fundraising",
		Description: "Individual + major-donor giving, campaigns, events, planned giving, retention."},
	{ID: "impact-measurement", Name: "Impact Measurement",
		Description: "Logic models, theory of change, indicators, evaluation methods, learning loops."},

	// --- Transportation ---
	{ID: "aviation-fundamentals", Name: "Aviation Fundamentals", Tier: "C",
		Description: "Aerodynamics, navigation, weather, regulations at the private + commercial study level."},
	{ID: "aviation-weather", Name: "Aviation Weather", Tier: "C",
		Description: "METARs / TAFs, hazardous weather, flight planning, sources + interpretation."},
	{ID: "instrument-flying", Name: "Instrument Flying", Tier: "C",
		Description: "IFR procedures, approaches, navigation aids, cross-country planning."},
	{ID: "aircraft-systems", Name: "Aircraft Systems",
		Description: "Powerplant, electrical, hydraulic, fuel, environmental systems at a pilot study level."},
	{ID: "aviation-regulations", Name: "Aviation Regulations", Tier: "C",
		Description: "FARs Part 61 / 91 / 121 / 135 overview, currency, medicals, airspace rules."},

	// --- Agriculture ---
	{ID: "row-crop-agronomy", Name: "Row Crop Agronomy",
		Description: "Corn / soy / wheat: variety selection, planting, fertility, weed + insect + disease management."},
	{ID: "soil-science", Name: "Soil Science",
		Description: "Soil texture, chemistry, biology, sampling + interpretation, conservation."},
	{ID: "fertility-management", Name: "Crop Fertility Management",
		Description: "Macronutrient + micronutrient management, soil tests, application strategies, 4Rs."},
	{ID: "farm-machinery", Name: "Farm Machinery",
		Description: "Tractors + implements: tillage, planting, spraying, harvest, maintenance."},
	{ID: "weather-climate-ag", Name: "Weather & Climate for Agriculture",
		Description: "Sources, growing degree days, frost dates, drought monitoring, regional climate trends."},
	{ID: "livestock-management", Name: "Livestock Management",
		Description: "Cow-calf / sheep / goat / hog operations: nutrition, health, breeding, marketing."},
	{ID: "grazing-systems", Name: "Grazing Systems",
		Description: "Rotational + management-intensive grazing, pasture species, infrastructure, monitoring."},

	// --- Personal additions ---
	{ID: "stretching-mobility-personal", Name: "Personal Mobility Practices",
		Description: "Self-directed mobility work: routines, common patterns, simple equipment + body-weight options."},
	{ID: "learning-methods", Name: "Learning Methods",
		Description: "Spaced repetition, retrieval practice, interleaving, mental models, deliberate practice."},
	{ID: "conflict-resolution", Name: "Conflict Resolution",
		Description: "Interest-based negotiation, active listening, de-escalation, repair after rupture."},
	{ID: "destination-research", Name: "Destination Research",
		Description: "Trip research methods: sources, seasonality, neighborhoods, costs, safety, logistics."},
	{ID: "journaling", Name: "Journaling",
		Description: "Practices for reflection: prompts, gratitude, morning pages, decision journaling."},
	{ID: "film-appreciation", Name: "Film Appreciation",
		Description: "Cinema history, major movements, directors, formal analysis, critical viewing."},
}

// tierOverride patches the tier of legacy domains (those added before
// the Tier field landed) without rewriting every entry in
// standardDomains. Keys are domain IDs; values are the explicit tier
// to use. Anything not in this map AND not carrying an explicit Tier
// in standardDomains defaults to "A" via effectiveTier().
//
// Tier-B (safety-relevant -- LLM-seeded but with a disclaimer chunk
// prepended): finance, taxes, legal, mental health, medical-records,
// parenting, child-development, dietary-restrictions, labor-law.
//
// Tier-C (high-stakes specialist -- not auto-seeded): currently none
// in the legacy 96; the new medical-clinical and surgical entries
// in the catalog expansion above are tagged C explicitly.
var tierOverride = map[string]string{
	// Personal finance + tax + investing -- specific advice can cause
	// real harm. Disclaimer chunk required.
	"personal-finance":   "B",
	"personal-taxes":     "B",
	"personal-investing": "B",
	"personal-insurance": "B",
	"personal-budgeting": "B",

	// Personal legal matters + estate planning -- disclaimer required.
	"personal-legal":           "B",
	"estate-planning-personal": "B",
	"contracts-personal":       "B",

	// Health + medical -- general info, not professional advice.
	"mental-health":            "B",
	"medical-records-personal": "B",
	"sleep-hygiene":            "B",
	"dietary-restrictions":     "B",

	// Parenting + child development -- safety-relevant content
	// (developmental milestones, when-to-call-a-doctor signals).
	"parenting":         "B",
	"child-development": "B",

	// Tax regulations + labor law in the business set.
	"tax-regulations": "B",
	"labor-law":       "B",
}

// effectiveTier returns the tier the seeder should use for a domain.
// Explicit Tier on the StandardDomain wins; tierOverride map is the
// next layer; default is "A". Returns one of "A" / "B" / "C".
func effectiveTier(d StandardDomain) string {
	if t := strings.TrimSpace(d.Tier); t != "" {
		return t
	}
	if t, ok := tierOverride[d.ID]; ok {
		return t
	}
	return "A"
}

// roleDomainMap mirrors the old ROLE_DOMAIN_MAP. The domain's RelevantForRoles
// field gets populated from this inverted mapping at seed time so a role
// -> domains lookup is a single query against the domain concept instead
// of a separate mapping concept.
var roleDomainMap = map[string][]string{
	"assistant":            {"inventory-supply-chain", "financial-data", "employee-records", "customer-relations", "product-catalog", "quality-metrics", "legal-documents", "project-data", "technical-documentation", "strategic-planning", "stakeholder-communications"},
	"accounting-finance":   {"financial-data", "accounting-principles", "tax-regulations", "budgeting-forecasting", "payroll-benefits", "inventory-supply-chain", "contracts-agreements", "risk-management", "regulatory-compliance"},
	"human-resources":      {"employee-records", "talent-acquisition", "labor-law", "training-development", "organizational-design", "payroll-benefits", "performance-assessment", "legal-documents", "regulatory-compliance"},
	"customer-service":     {"customer-relations", "product-catalog", "service-level-agreements", "ticket-management", "quality-metrics", "sales-pipeline", "training-development"},
	"quality-assurance":    {"quality-metrics", "product-catalog", "process-optimization", "technical-documentation", "regulatory-compliance", "service-level-agreements", "data-analysis", "research-methodology"},
	"sales-marketing":      {"sales-pipeline", "marketing-analytics", "brand-strategy", "lead-generation", "customer-relations", "product-catalog", "contracts-agreements", "data-analysis", "stakeholder-communications"},
	"it-support":           {"technical-documentation", "network-infrastructure", "cybersecurity", "software-development", "cloud-platforms", "ticket-management", "process-optimization", "vendor-management"},
	"legal-compliance":     {"legal-documents", "contracts-agreements", "regulatory-compliance", "intellectual-property", "labor-law", "risk-management", "tax-regulations", "stakeholder-communications"},
	"operations":           {"process-optimization", "logistics-distribution", "vendor-management", "inventory-supply-chain", "quality-metrics", "product-catalog", "budgeting-forecasting", "risk-management"},
	"project-management":   {"project-data", "process-optimization", "budgeting-forecasting", "stakeholder-communications", "risk-management", "quality-metrics", "vendor-management", "data-analysis", "organizational-design"},
	"research-development": {"research-methodology", "data-analysis", "innovation-management", "technical-documentation", "intellectual-property", "product-catalog", "software-development", "budgeting-forecasting"},
	"training-education":   {"curriculum-design", "performance-assessment", "training-development", "employee-records", "organizational-design", "data-analysis", "technical-documentation", "stakeholder-communications"},

	// --- Personal-category role -> domain mappings ---
	// Per the v1 brainstorm + the personal-tier expansion: knowledge
	// domains can encapsulate either broad themes (reference content
	// like recipes / how-to guides) OR granular data (validated
	// records like a household inventory or a personal medical log).
	// Mapping below mixes both shapes per role -- the SAME domain
	// concept holds both whether agents read it as RAG content
	// (documentChunk text) or query it as records (typed concepts
	// like SpreadsheetRow).
	"personal-finance-advisor": {"personal-finance", "personal-taxes", "personal-investing", "personal-insurance", "personal-budgeting", "estate-planning-personal"},
	"household-manager":        {"household-maintenance", "home-inventory", "household-chores", "personal-finance", "personal-budgeting"},
	"parenting-coach":          {"parenting", "child-development", "school-education-personal", "nutrition", "mental-health"},
	"health-wellness-coach":    {"nutrition", "fitness", "mental-health", "sleep-hygiene", "medical-records-personal", "mindfulness"},
	"meal-planning-chef":       {"recipes", "meal-planning", "dietary-restrictions", "nutrition"},
	"travel-planner":           {"travel-planning", "travel-documents", "restaurant-dining", "personal-finance"},
	"creative-companion":       {"creative-arts", "music-appreciation", "photography", "personal-growth"},
	"learning-companion":       {"language-learning", "online-courses", "book-summaries", "personal-growth"},
	"relationships-social":     {"relationships-communication", "life-events-celebrations", "gifts"},
	"pet-care-specialist":      {"pet-care", "pet-training", "pet-health"},
	"home-improvement-diy":     {"diy-repairs", "gardening", "sustainability", "home-inventory", "household-maintenance"},
	"personal-legal-advisor":   {"personal-legal", "estate-planning-personal", "contracts-personal"},
	"mindfulness-coach":        {"mindfulness", "journaling", "personal-growth", "mental-health"},
	"entertainment-curator":    {"entertainment-media", "music-appreciation", "book-summaries", "restaurant-dining"},
	"senior-care-advisor":      {"elder-care", "end-of-life-planning", "medical-records-personal", "estate-planning-personal", "personal-legal"},

	// Real-estate advisor draws on the seven dedicated real-estate
	// domains plus tangential personal domains: personal-finance +
	// personal-taxes (mortgage cost + tax implications),
	// personal-insurance (homeowners / renters coverage during the
	// transaction), home-inventory (rolls forward into the new
	// property), personal-legal (purchase contract review backstop),
	// and contracts-personal (lease + offer-letter review patterns
	// transfer cleanly to real-estate paperwork).
	"real-estate-advisor": {"real-estate-listings", "mortgage-shopping", "neighborhood-research", "home-inspection", "property-taxes", "lease-agreements", "closing-process", "personal-finance", "personal-taxes", "personal-insurance", "home-inventory", "personal-legal", "contracts-personal"},
}

// copresentUISeedCorpus is the initial content that populates the
// copresent-ui domain so the first walkthrough has something to work
// with. It's deliberately small and high-signal -- a handful of
// paragraphs covering the major app surfaces. As the app grows, the
// seedCopresentUI capability can be re-run with additional entries and
// the idempotent chunk id (hash of domain+source+seq+text) will write
// new versions without duplicating.
var copresentUISeedCorpus = []struct {
	SourceRef string
	Text      string
}{
	{
		SourceRef: "overview:layout",
		Text:      `CoPresent is a real-time collaboration app where humans and AI agents share "spaces" for conversation. There is ONE real route -- /space -- with a single ?panel= query axis that drives the right column. Every other historical path (/dashboard, /spaces, /agents, /settings, /profile) redirects to /space (+ the matching panel query when applicable). The top-level layout: a LEFT presence panel (auto-shows when the user is joined to the active space; collapses when they aren't), a CENTRE canvas (a quiet liquid-glass surface that hosts the per-space card timeline; the legacy R3F welcome scene + stereoscopic / particle stack was retired) that expands into the left region when no presence is mounted, and a RIGHT column that renders when ?panel= is set. Header elements, left-to-right: the app logo, a space-context pill (shows the active space's title; clicking routes to ?panel=chat so the chat elevates into the right column) or a dashed "Select a space" placeholder when none is active, a segmented nav toggle with SIX tiles ([Spaces | Agents | Knowledge | Training | Tasks | Settings]; writes the ?panel= query), an optional **Computer Use pill** (data-op-id=header.computer-use-pill; only renders when the user has a paired worker -- see surface:header.computerUsePill), and a Profile pill on the far right (initials avatar + first + last name; opens the Profile MODAL, not a route). The PROFILE modal (data-op-id=header.profile) replaces the old /profile page; it holds account details, the CoPresent + MemQL versions, and sign-out actions. CHAT has no tile in the header nav anymore: the space-context pill already routes to ?panel=chat, and when joined chat also floats as a widget over the canvas, so the duplicate was retired. Common user paths: "open spaces" -> uiClick nav.spaces; "manage agents" -> uiClick nav.agents; "manage knowledge / domains / library / sources" -> uiClick nav.knowledge; "train an agent / change an agent's knowledge or skills" -> uiClick nav.training (the only surface for editing KNOWLEDGE; skills are editable both here and in the Create Agent modal's Skills tab); "see what tasks are running / done" -> uiClick nav.tasks; "change settings" -> uiClick nav.settings; "pause computer-use globally / open computer-use settings" -> uiClick header.computer-use-pill; "view profile / sign out" -> uiClick header.profile; "return to the active conversation" -> uiClick the space-context pill (data-op-id=nav.currentSpace).`,
	},
	{
		SourceRef: "surface:agents",
		Text:      `The Agents right panel (opId=agents.listPanel, opens at ?panel=agents via uiClick nav.agents; the legacy /agents URL redirects to /space?panel=agents) lists every AI agent the user has. The list is sorted in two buckets: the auto-seeded Assistant (Sofia) is PINNED at the top, then a hairline "Specialists" divider (opId=agents.list.divider), then the rest of the specialists alphabetically. The Assistant is a per-user singleton and cannot be created from the picker -- "Assistant" is explicitly excluded from the role dropdown in the Create Agent modal. The "+" button (opId=agents.new) opens the Create Agent modal for creating specialist agents only. ACTION BUTTONS PER ROW are STATE-DEPENDENT -- this is the most common place agents get confused, read this carefully: (a) Assistant row: Edit ONLY (agents.row.edit.<id>). There is NO deactivate and NO delete button -- the GA is a structural requirement for space creation, agent auto-join, and other guarded flows. Do not try to click a delete button on Sofia; it does not exist. (b) Active specialist row: Edit (agents.row.edit.<id>) + Deactivate (agents.row.deactivate.<id>). NO delete button yet. (c) Inactive specialist row: Activate (agents.row.activate.<id>) + Delete (agents.row.delete.<id>). NO edit button here -- inactive agents must be reactivated first if you want to edit them. So to DELETE A SPECIALIST, you always need TWO clicks: first uiClick agents.row.deactivate.<id> (the row re-renders into the inactive shape), then uiClick agents.row.delete.<id> (opens a confirm dialog). To EDIT, just uiClick agents.row.edit.<id> -- it opens the CreateAgentModal in edit mode, pre-populated with the agent's current name, role, gender, personality. ROLE-MOVED FIELDS (read this carefully): KNOWLEDGE DOMAINS were removed from the Create Agent / Edit Agent modal -- they live only in the Training panel (?panel=training, see surface:training). SKILLS remain editable in the modal's Skills tab (createAgent.tab.skills, with createAgent.skill.<slug> rows) AND in Training. So "add HR knowledge to Cleo" -> open Training (knowledge has no modal home); "give my Operations agent the email skill" -> either the modal's Skills tab or Training. The Edit modal carries Personality + Skills tabs + basic fields (no Knowledge tab); the Role dropdown is locked for the GA (cannot demote Sofia from assistant) but editable for specialists. Per-agent skill chips on the agent CARD (in the AgentsListPanel rows) hide bundle primitives -- when an agent has the Computer Use umbrella the chip reads "Computer Use", not "workerHost" / "workerComputer" / "workerStatus" (those are internal fan-out names; the user picks the umbrella).`,
	},
	{
		SourceRef: "concept:agentLifecycle.deleteFlow",
		Text:      `Deleting a specialist agent is a GATED two-step flow because Delete only appears on an INACTIVE row. The correct walkthrough cadence when a user says "delete <name>": (1) uiReadState -- confirm the row shows Edit + Deactivate (active) or Activate + Delete (already inactive). (2) If active: uiHighlight agents.row.deactivate.<id>, narrate "We'll deactivate <name> first so the Delete button appears", uiAskUser "Ready to deactivate <name>?" with ["Yes, deactivate", "Cancel"]. On Yes, uiClick agents.row.deactivate.<id> -- the row re-renders, Edit/Deactivate disappear and Activate/Delete appear. (3) uiHighlight agents.row.delete.<id>, uiAskUser "Ready to delete <name>? This cannot be undone." with ["Yes, delete <name>", "Cancel"]. On Yes, uiClick agents.row.delete.<id>. (4) A confirm dialog mounts (agents.deleteConfirm.*); if present, uiClick agents.deleteConfirm.confirm to finalize. COMMON MISSTEPS to avoid: do NOT click agents.row.edit.<id> expecting a Delete option inside the edit modal -- there is no Delete inside the CreateAgentModal, delete lives only on the inactive-row action bar. Do NOT try to delete the Assistant; its row has no Delete/Deactivate buttons at all (see surface:agents). If the user asks to delete the GA, narrate that it's structurally required and offer to edit it instead.`,
	},
	{
		SourceRef: "concept:agentLifecycle.editFlow",
		Text:      `Editing an agent opens the CreateAgentModal in EDIT mode (same component as Create, toggled by passing the agent prop). Entry: uiClick agents.row.edit.<id>. The modal pre-populates every field (Gender, Name, Role, Intelligence policy, Personality styles, Skills) from the agent's stored values (Knowledge has no modal tab -- it's Training-only), skips the Describe phase entirely (lands directly on Configure), and the submit button reads "Save Changes" instead of "Create Agent". The pinned describe strip (textarea + "Set up my agent" button) is hidden in edit mode -- there's no natural-language re-description step. All Configure-form fields behave identically to Create; the Role dropdown is the only field with an EDIT-mode nuance: when editing the Assistant (role=='assistant'), the Role dropdown is DISABLED so the user cannot demote Sofia away from assistant and break the per-user singleton invariant -- every other field (including name!) is still editable for the GA. If the agent has an explicit model pin saved, the model-pin selector (createAgent.explicitModel) at the bottom of the card shows the current override (it renders unconditionally -- there is no "Advanced" collapsible anymore). Editing an inactive specialist requires REACTIVATING it first (Activate button on the row) because the row in inactive state shows Activate + Delete, no Edit. Walkthrough cadence for edit: (1) uiClick agents.row.edit.<id>. (2) uiReadState to confirm the modal is open (look for createAgent.submit). (3) Make the requested change (uiType for Name, uiSelect for Role or Intelligence policy, uiClick for list rows inside the Personality / Skills tabs -- Knowledge is Training-only). (4) Commit confirmation: uiHighlight createAgent.submit, uiAskUser "Ready to save changes to <Name>?" with ["Yes, save", "Cancel"]. On Yes, uiClick createAgent.submit.`,
	},
	{
		SourceRef: "surface:createAgentModal",
		Text:      `The Create Agent modal (route modal:createAgent) is a two-phase form. Phase 1 (Describe) is a pinned textarea at the top of the modal (createAgent.description) with a voice-input toggle (createAgent.voice) and a "Set up my agent" button (createAgent.generate) that fires AI suggestion. Phase 2 (Configure) appears below once a suggestion lands -- the same pinned strip stays visible but the button morphs into "Start over" (createAgent.startOver). The footer's secondary button switches from "Configure manually" (createAgent.configureManually; skips AI generation and opens Phase 2 with defaults) in Describe phase to the primary Create button (createAgent.submit) in Configure phase. Phase 2 is laid out as a single card. TOP of the card -- BASIC FIELDS (pinned, always visible): (1) Gender pill toggle (createAgent.gender.female / createAgent.gender.male), (2) Name input (createAgent.name) with random-name button (createAgent.randomName), (3) Role <select> (createAgent.role), (4) Intelligence policy <select> (createAgent.policy). BOTTOM of the card -- a TWO-TAB picker (createAgent.tab.<key>, keys 'personality' | 'skills'): the Personality tab has a search input (createAgent.tab.personality.search) above a sub-category pill strip (createAgent.tab.personality.subcategory.<key> with keys 'all' | 'warm' | 'direct' | 'thoughtful') then a scrollable list of style rows (createAgent.style.<slug>); the Skills tab (createAgent.tab.skills) lists skill rows (createAgent.skill.<slug>). The KNOWLEDGE tab was removed -- knowledge domains live exclusively on the Training panel (?panel=training, see surface:training). Skills are editable here AND in Training. BOTTOM-MOST: a model-pin selector (createAgent.explicitModel) for the rare case of pinning a specific model -- it renders unconditionally now (the old "Advanced" collapsible / createAgent.advancedToggle was removed). Use the COMMIT CONFIRMATION pattern (concept:commitConfirmation) before pressing createAgent.submit: highlight the submit button, uiAskUser to confirm, and on "Yes" uiClick it yourself. After creation, if the user asked for specific knowledge or skills in the original request, narrate "Now let's open Training to give <Name> her knowledge and skills" and uiClick nav.training to chain into the Training flow.`,
	},
	{
		SourceRef: "surface:createAgentModal.personalityPicker",
		Text:      `The Configure card's lower half is a TWO-TAB picker -- Personality and Skills (createAgent.tab.personality / createAgent.tab.skills). Knowledge domains are NOT here (they moved to Training); Skills ARE here (and also in Training). PERSONALITY tab layout top-to-bottom: (A) a search input (createAgent.tab.personality.search) for filtering personality styles by typed text. (B) a SUB-CATEGORY pill strip (createAgent.tab.personality.subcategory.<key>) with keys 'all' | 'warm' | 'direct' | 'thoughtful' for narrowing. (C) a scrollable list of style rows -- each row is a full-width button with a checkbox indicator on the left, a label in the middle, and a lock glyph on the right if locked. Row op-ids: createAgent.style.<slug>. The Aligned style (createAgent.style.aligned) is ALWAYS LOCKED-ON -- the organisation-wide baseline voice every agent carries; it cannot be deselected (see concept:lockedAlignedStyle). To toggle an additional style use uiClick createAgent.style.<slug>; aria-pressed signals current state. To switch sub-category use uiClick createAgent.tab.personality.subcategory.<key>. The scrollable list auto-scrolls when uiClick/uiPointerTo targets a row that isn't visible. The SKILLS tab (createAgent.tab.skills) sits next to Personality with its own scrollable list of createAgent.skill.<slug> rows. There is NO Knowledge tab -- createAgent.tab.knowledge / createAgent.domain.<slug> do not exist; reroute any knowledge edits to the Training panel.`,
	},
	{
		SourceRef: "surface:createAgentModal.fieldSemantics",
		Text:      `CRITICAL disambiguation on the Configure Agent form: Name and Role are TWO DIFFERENT FIELDS. NAME (op-id createAgent.name, a single-line input) is the agent's DISPLAY NAME -- one word, like "Zeus", "Astra", "Iris", "Felix". It's how the user will address the agent in conversation. NAME IS NOT THE ROLE. When the user says "create an IT Support agent", that's the ROLE, not the name; the agent still needs a name. ROLE (op-id createAgent.role, a <select> dropdown) is the agent's domain specialty. THE EXACT ROLE SLUGS ARE (USE THESE VERBATIM): assistant, accounting-finance, human-resources, customer-service, quality-assurance, sales-marketing, it-support, legal-compliance, operations, project-management, research-development, training-education. Common user phrase -> slug: "Operations" / "Operations Manager" / "ops" -> "operations" (NOT "operations_manager"); "IT support" -> "it-support"; "HR" / "Human Resources" -> "human-resources"; "sales" / "marketing" -> "sales-marketing"; "R&D" / "research" -> "research-development"; "legal" -> "legal-compliance"; "project manager" / "PM" -> "project-management"; "QA" / "quality" -> "quality-assurance"; "training" -> "training-education"; "accounting" / "finance" -> "accounting-finance"; "customer service" / "support" -> "customer-service". Set role with uiSelect(createAgent.role, value=<slug>), NOT uiType. Never type "IT Support" into NAME. Never type a role slug into NAME. Always ask the user what NAME they want via uiAskUser. If they say "any" or "random" or "you pick", click createAgent.randomName (a button next to the name input) which generates a locale+gender-appropriate random name in the field. If uiSelect returns "no option matching" with an Available list, retry uiSelect using an EXACT slug from that list. INTELLIGENCE (op-id createAgent.policy, a <select>) is a separate basic field that picks the SI Router policy the agent runs on. Policy slugs: balancedChat (default), strongReasoning, fastCoding, lowLatencyVoice, cheapestCapable. Model pinning (the rare case) lives at the bottom of the card: uiSelect createAgent.explicitModel directly -- it renders unconditionally now (the old "Advanced" collapsible was removed, so there is no createAgent.advancedToggle to expand first). Most walkthroughs never touch it; only mention it if the user explicitly asks to pin a model.`,
	},
	{
		SourceRef: "surface:createAgentModal.walkthroughCadence",
		Text:      `When walking a user through Create Agent, pace the flow field-by-field. After opening the modal and clicking createAgent.configureManually (which skips the AI-suggest Describe step and jumps to Phase 2), the standard cadence is: (1) BASIC FIELDS FIRST. uiAskUser gender (Female / Male) and uiClick createAgent.gender.{female|male}. (2) uiAskUser "What name would you like for your agent?" offering options like ["Zeus (male)", "Aria (female)", "Pick for me"]. If they pick "Pick for me", uiClick createAgent.randomName. If they give a name, uiType it into createAgent.name. (3) uiSelect createAgent.role with the role slug derived from their original request ("IT support" -> "it-support"). (4) INTELLIGENCE policy: uiSelect createAgent.policy (default "balancedChat" is fine for almost every walkthrough; only ask if the user mentions needing fast coding, low-latency voice, strong reasoning, or cheap). SKIP model-pinning (createAgent.explicitModel) unless explicitly asked. (5) PERSONALITY PICKER. Before pointing at the personality list, emit uiPointerTo on a representative style row so the modal scrolls and the user sees what you're describing (see concept:scrollIntoView). Only ask about ADDITIONAL styles -- Aligned is locked-on. Optionally narrow via uiClick createAgent.tab.personality.subcategory.{warm|direct|thoughtful} if the user's description leans one way. Then uiClick createAgent.style.<slug>. (6) SKILLS (optional): if the user named skills, uiClick createAgent.tab.skills, then uiClick the createAgent.skill.<slug> rows for the requested skills. NO KNOWLEDGE STEP HERE -- the Knowledge tab was removed (createAgent.tab.knowledge / createAgent.domain.<slug> / createAgent.capability.<slug> / createAgent.integration.<slug> do NOT exist); knowledge is added later in Training. (7) COMMIT CONFIRMATION: uiHighlight createAgent.submit, then uiAskUser({question: "Ready to create <Name>, your <role> agent?", options: ["Yes, create <Name>", "Cancel"], allowFreeForm: false}). On "Yes" uiClick createAgent.submit yourself. (8) IF the user's original request mentioned specific knowledge or skills ("an HR agent who handles email", "Operations agent that knows our finance docs"), CHAIN INTO TRAINING: narrate "Now let's open Training to give <Name> the knowledge and skills you asked for" and uiClick nav.training. Then drag <Name> from the agents palette onto the Studio's Agent slot, drag the requested domains + skills into Knowledge and Skills zones, uiHighlight training.train, COMMIT CONFIRMATION, uiClick training.train. See surface:training and surface:trainingStudio for the full Training flow.`,
	},
	{
		SourceRef: "concept:walkthroughRules",
		Text:      `Walkthrough mode = teaching, not execution. Rules: (1) NARRATE before each meaningful action using uiNarrate when interactivity is 'conversational'. One short sentence per step, present tense: "Opening the Agents panel now." / "Asking for a name next." (2) ASK before filling fields with values the user didn't explicitly provide. Use uiAskUser with 2-3 concrete options plus free-form. Do NOT derive a name, style, or gender from the user's request text — those were role/context descriptions, not field values. (3) PAUSE between meaningful steps: use uiPointerTo or uiHighlight to draw the user's eye before uiClick. (4) BE PATIENT with your iteration budget. Each uiAskUser round-trip is one of your turns; budget accordingly. If you run out, the user sees you freeze. Prefer fewer smarter asks over many tiny ones. (5) Never click commit (Create, Save, Delete, Send). Always highlight + release + summarise.`,
	},
	{
		SourceRef: "concept:scrollIntoView",
		Text:      `SCROLL THE USER TO WHAT YOU'RE TALKING ABOUT. Modal forms (Create Agent, Create Space, Settings) scroll internally, and fields below the fold are INVISIBLE until the section is scrolled into view. Narration alone does not scroll the page -- the user reads your message and looks at whatever was already visible, missing the section you described. Every cursor-moving primitive auto-scrolls its target into view via scrollIntoView: uiPointerTo, uiHighlight, uiClick, uiSelect, uiType. So the discipline is: before narrating about a section you haven't touched yet, emit a uiPointerTo on a representative op-id in that section. This pulls the section into the viewport and gives the user a cursor anchor pointing at what you're describing. Correct pattern: uiPointerTo(createAgent.skill.computer-use) -> uiNarrate("Next is Skills -- the things Cleo can actually do. I've picked the ones IT Support maps to."). WRONG pattern: uiNarrate("Knowledge Domains are below, you can scroll to see them.") -- the user doesn't scroll, they wait for you to scroll. When highlighting a commit button at the end (uiHighlight createAgent.submit) the highlight itself scrolls, so no extra pointer needed. SPECIAL CASE -- TABBED PICKER NESTED SCROLL: the Create Agent Personality / Skills tab panels each contain their OWN overflow-y-auto list that is typically only ~250px tall. A target row can have pixel-coordinates that fall inside the browser viewport while still being clipped by the tab panel's own scroll boundary. The auto-scroll primitive handles this by walking scrollable ancestors, but you should STILL emit uiPointerTo on the target row BEFORE narrating or clicking, especially when the row is not at the top of the list. Correct cadence for adding "Creative" to Personality: uiClick createAgent.tab.personality -> uiPointerTo createAgent.style.creative (this scrolls the row into the panel's visible area) -> uiNarrate "I'll add Creative to the mix so she can brainstorm freely." -> uiClick createAgent.style.creative. For sections you've scrolled past without interacting -- Personality list, Skills list -- always pointer-in first, then narrate, then act.`,
	},
	{
		SourceRef: "surface:spaces",
		Text:      `The Spaces right panel (opens at ?panel=spaces via uiClick nav.spaces) lists the user's conversation spaces and is the user's primary surface for picking, creating, and managing them. Every space is a multi-participant room with configurable slot caps (default 5 humans, 3 agents); the owner's Assistant auto-joins every space they create. THREE-TAB LIFECYCLE: Active (status=active; the working set), Saved (status=saved; the user explicitly chose to keep these forever), and Archived (status=archived; auto-deletes after the retention window). Tab buttons: spaces.tab.active, spaces.tab.saved, spaces.tab.archived. Active also shows a count badge. Each row shows its name + last-activity timestamp + per-state action buttons; the row body itself is spaces.row.select.<id> for opening the space (auto-joins on click; no manual Join button anymore). Per-state row actions: ACTIVE rows have spaces.row.rename.<id>, spaces.row.save.<id>, spaces.row.archive.<id>. SAVED rows have spaces.row.rename.<id>, spaces.row.archive.<id> (saved -> archived restarts the retention countdown). ARCHIVED rows have spaces.row.save.<id> (rescue from purge), spaces.row.restore.<id> (back to active), spaces.row.delete-now.<id> (skip the wait, hard-delete now); they also show a small "expires in N days" countdown badge. The "+" button (top right) opens Create Space (modal:createSpace). DAILY SPACE: when User.preferences.dailySpaceEnabled is on (default), a private auto-provisioned per-user-per-day singleton is pinned at the TOP of the Active tab with a "Daily" badge. The pinned daily row has NO per-row action buttons -- the rollover is automation-managed (yesterday's daily archives or saves at the next day boundary based on User.preferences.dailySpaceRolloverAction). Non-daily Active rows still get the full Rename / Save / Archive triplet. When the user clicks any space row, the chat widget floats on the right side of the canvas (or elevates into the right column if the user clicks the space-context pill to route to ?panel=chat); the presence panel shows up on the left automatically. Auto-join replaced the legacy "Join this space" empty-state -- clicking a row IS the join; there is no manual Join button.`,
	},
	{
		SourceRef: "surface:settings",
		Text:      `The Settings right panel (opens at ?panel=settings via uiClick nav.settings; the legacy /settings URL redirects to /space?panel=settings) has sections for General (theme switcher, language selector, replay-intro button), Takeover (agent-takeover appearance clean/dim + cursor speed -- see surface:settings.takeover), Spaces (lifecycle preferences -- archive retention 30/60 days at settings.spaces.retention.<value>, IANA timezone for the daily-space rollover at settings.spaces.timezone, daily-rollover action archive/save at settings.spaces.rollover.<value>), Groups (admin view of groups the user belongs to, with invite flows), Devices (microphone / camera / audio output selectors, accessed via the presence.devices button that opens a session-settings panel), and Sessions (active session list, sign-out-all-sessions). EVERY setting on this panel persists to the user's record in memQL (v1:identity:user.preferences) -- not browser localStorage. That means preferences follow the user across browsers / devices / fresh sessions; clearing browser data does NOT reset them. The legacy 3D Glasses / stereoscopic-rendering toggle (settings.stereo3D) was retired along with the R3F welcome-scene stack and is no longer present.`,
	},
	{
		SourceRef: "surface:settings.takeover",
		Text:      `The Takeover section on the Settings page (data-op-ids begin settings.takeover) is the home for preferences that shape how an agent drives the UI on the user's behalf during a TAKEOVER -- clean autopilot, the AI does it FOR you. CoPresent Control v2 (copresent#187/#188) retired the old single "CoPresent Control" settings section AND its two-mode (Standard / Interactive) split; there is now ONE takeover experience with just two preferences: Appearance (settings.takeover.appearance.clean / settings.takeover.appearance.dim) and a continuous Cursor Speed slider (settings.takeover.cursorSpeed). Both persist to v1:identity:user.preferences (takeoverMode, cursorTweenMs) on the server, so they follow the user across browsers and devices. There is NO "interactive pace" preset anymore (it belonged to the retired two-mode model). The OTHER way an agent can drive the UI -- CoPresent GUIDE, an immersive voice-to-voice Scene walkthrough (the AI does it WITH you) -- has no settings here; Guide is consent-gated per run (see concept:takeoverVsGuide + surface:guide). The legacy data-op-ids settings.copresentControl.* / .standard.* / .interactive.* NO LONGER EXIST -- do not reference them.`,
	},
	{
		SourceRef: "surface:profile",
		Text:      `The Profile MODAL (data-op-id=header.profile trigger; the modal itself carries profile.modal.* op-ids) shows the user's account information: first name, last name, email, phone, gender, date of birth, role in the organisation, the CoPresent app version, and the MemQL version. All editable fields use inline edit controls; the app + MemQL versions are read-only. Sign-out and sign-out-all-sessions live in the modal footer. The modal opens by clicking the header's Profile pill on the far right of the nav row (initials avatar + user name). The legacy /profile URL redirects to /space -- there is no longer a dedicated profile route or right-panel Profile tab. To open programmatically during a takeover: uiClick header.profile. To close: uiClick profile.modal.done.`,
	},
	{
		SourceRef: "concept:takeoverSession",
		Text:      `A Takeover session is the bounded window during which an agent drives the UI on the user's behalf via CoPresent Takeover (clean autopilot; skill copresent_takeover). Visual signals while a session is active: a soft theme-coloured cursor (~28px) animates to target elements with an ease-in-out-sine curve; an optional spotlight pulses around highlighted targets; and the Take-Back Control button floats bottom-right (data-op-id=control.take-back). CoPresent Control v2 (copresent#188/#189) made Takeover a CLEAN autopilot: there is NO floating control widget and NO conversational surface tied to the takeover anymore -- the agent's narration (uiNarrate) and any questions (uiAskUser) ride the normal chat/voice channel, not a dedicated widget. Sessions are begun with uiRequestControl({reason}) and ended with uiReleaseControl({summary}). The session persists across uiNavigate calls AND uiClick calls that trigger route changes -- so a cross-page session is ONE session, not many. Commit buttons (Create/Save/Delete/Send/Invite/Remove/Confirm/Sign-Out) are NOT silent -- before clicking one the agent asks for explicit consent via uiAskUser with options like ["Yes, create it", "Cancel"], and only clicks on "Yes". The user's option pick IS the consent. This lets multi-step walkthroughs (e.g. create agent + create space) chain without the user having to manually take back control between steps. (The other UI-driving mode, Guide, runs Scenes under forced voice with its own consent card -- see surface:guide.)`,
	},
	{
		SourceRef: "concept:releaseLanding",
		Text:      `LAND THE CURSOR ON A NAVIGABLE SURFACE BEFORE uiReleaseControl. The agent cursor fades when the user clicks Take Back Control; whatever element the cursor was last over is where it disappears. Releasing while the cursor is in the middle of a Settings field, a closed modal, or a buried form section leaves the user without an obvious next step -- the takeover ends and they're stranded. The fix is a single uiClick on the header's space-context pill as the LAST action before uiReleaseControl. Two variants render in the header (only one at a time): when an active space is selected the pill carries data-op-id="nav.currentSpace" and routes to ?panel=chat (drops the user into the active conversation); when no active space is selected (typical first-login / fresh-account state) the pill carries data-op-id="nav.noActiveSpaceLabel" and routes to ?panel=spaces (drops the user on the spaces list). Read the surfaces / after-state to pick the right one and uiClick it. Same rule for BOTH ways of driving the UI -- Takeover (clean autopilot) and Guide (voice Scenes; the Guide's own end-of-run flow lands the user on a sensible surface). The intro / first-login walkthrough also follows this rule (the runOnboarding orchestrator does it automatically before its own release call), so behaviour stays uniform whether the agent is LLM-driven or curriculum-driven. SKIP the landing click only when (a) your goal already finished on a useful surface (Profile modal open, the row the user asked about is highlighted in a list, the spaces panel is showing the new space you just created), or (b) the user explicitly asked to be left where they are. Forcing a redundant click steals their context. Otherwise: click the pill, then release. The cursor fading on a real button reads as "agent finished and handed me a starting point", not "agent abandoned me mid-form".`,
	},
	{
		SourceRef: "concept:takeoverVsGuide",
		Text:      `CoPresent has TWO distinct ways an agent drives the UI -- TAKEOVER and GUIDE (copresent#187 split the retired single "CoPresent Control" into these). They use the SAME operator primitives (uiClick / uiType / uiNarrate / uiAskUser / ...); the EXPERIENCE differs, and the agent picks per request. TAKEOVER (skill copresent_takeover): clean AUTOPILOT -- the AI does it FOR you. For transactional get-it-done asks where the agent can just execute: "change theme to dark", "delete the agent named Test", "create an ops agent". No widget, no forced voice; the agent drives the cursor, narrates lightly in chat/voice as needed, and uses the COMMIT-CONFIRMATION gate (uiAskUser) before any mutating click. Begun with uiRequestControl, ended with uiReleaseControl. Appearance (clean/dim) + cursor speed come from Settings -> Takeover. GUIDE (skill copresent_guide): immersive voice-to-voice SCENE walkthrough -- the AI does it WITH you. For pedagogical / tour / demo asks ("walk me through", "teach me", "give me a tour", "show me how to..."). Consent-forward entry via the GuideConsentCard (guide.consent.accept / guide.consent.decline); on accept VOICE IS FORCED ON for the run; the agent narrates an ordered sequence of Scenes while driving the UI + Canvas (with the annotation primitive for pointing). Lifecycle idle -> entering(consent) -> running <-> paused -> ended. PICKING: if the user just wants the outcome -> Takeover; if the user wants to be shown/taught step by step -> Guide. (delegateTakeover -- the old specialist->GA hand-off -- is RETIRED; the GA holds both skills directly, so there is no hand-off.)`,
	},
	{
		SourceRef: "concept:alreadySelected",
		Text:      `CHECK BEFORE YOU ACT. Every toggle in CoPresent's forms -- gender pills, personality rows, knowledge domain rows, skill rows (capabilities + integrations), tab buttons, sub-category pills, create-space agent-selector rows -- carries aria-pressed="true" when it's the currently selected option. <select> elements (createAgent.role, createAgent.policy, createAgent.explicitModel) carry the current value on el.value. Inputs (createAgent.name, etc.) carry the current text on el.value. BEFORE calling uiClick on a toggle, uiSelect on a dropdown, or uiType on an input, verify the value isn't already what the user asked for. Signals: the uiReadState / after-state summary shows aria-pressed="true" on the matching row, or a previous step returned "already set to X" / "already active". If the value already matches, DO NOT call the primitive -- clicking an already-pressed TOGGLEABLE row DESELECTS it (surprising the user who just said "yes, that one"), retyping the same name churns the input, re-selecting the same option wastes an iteration. Instead emit uiHighlight({target: <field-op-id>}) + uiNarrate({target: <field-op-id>, message: "<field> is already set to <value>"}). SEPARATE CASE -- LOCKED ROWS: some rows are permanently selected and cannot be toggled at all. They look visually identical to a selected toggleable row (checked + primary-tinted border) but carry a small lock glyph on the right edge, plus either a disabled attribute on the <button> or are rendered as a plain <div> (no button element at all, for the pinned auto-join rows). uiReadState marks these as non-interactive / locked. NEVER emit uiClick on a locked row -- the click is a no-op, the cursor flies for nothing, the user sees a pointless move, and you burn an iteration. This is the single most common misfire in walkthroughs because aria-pressed="true" reads the same on both; always check for the lock signal before concluding "already selected, I should click to deselect". See concept:lockedDefaults for the full per-modal roster of locked rows. The backend short-circuits the action if you do call it anyway, returning a text that includes "already set" / "already active" / "locked" -- but that still costs a turn you should have saved.`,
	},
	{
		SourceRef: "concept:highlightBeforeMove",
		Text:      `Every cursor-moving primitive (uiClick, uiType, uiSelect, uiPointerTo) now wraps its action in a scoped spotlight: the target ELEMENT is highlighted BEFORE the cursor starts moving, the cursor flies to the highlighted element, the action fires (or short-circuits if already set), a brief dwell lets the user see the result with the ring still on, then the highlight fades. The ring paints directly on the real DOM node via a data-copresent-highlight attribute -- no separate overlay, no tracking loop, no ghost ring when the target re-renders. uiHighlight is still the "leave it on until explicitly cleared" primitive; the others are scoped to their single action. You don't need to call uiHighlight before every uiClick -- the click already carries its own spotlight for the duration of the action. Reserve uiHighlight for the final "here's the commit button, press it" moment or when narrating about a section the user should look at without any action happening on it.`,
	},
	{
		SourceRef: "concept:canvasAnnotation",
		Text:      `CANVAS LIVE-ANNOTATION (copresent#193) is the agent's pointing/markup overlay on the centre Canvas during a Takeover or (especially) a Guide -- the on-screen equivalent of a presenter's laser pointer. The agent emits an annotation directive carrying: kind ('laser' | 'arrow' | 'circle'), a target (the op-id or card-internal op-id to anchor on), an optional from (a second anchor, for an arrow drawn between two points), and an optional label (a short caption rendered next to the mark). 'laser' is a transient dot that pulses at the target (use it to say "look here" mid-narration); 'arrow' draws from -> target (use it to connect two things, e.g. "this button feeds that list"); 'circle' rings the target (use it to emphasise a field or card region). Annotations are EPHEMERAL visual aids -- they do NOT click, type, or mutate anything; pair them with uiNarrate so the user hears what the mark means. #193 also added CARD-INTERNAL op-ids: canvas cards now expose stable op-ids for their inner controls (buttons, toggles, regions) so annotations + clicks can target a specific element INSIDE a card, not just the card as a whole; these appear in the regenerated op-id manifest as <opIdPrefix>-pattern entries. Prefer a circle/arrow + narration over a bare uiPointerTo when you're teaching (Guide) and want the user to register WHAT a control does, not just where the cursor is.`,
	},
	{
		SourceRef: "concept:canvasWidgets",
		Text:      `CoPresent has TWO floating widgets that share one liquid-glass aesthetic (frosted white-alpha surface, backdrop-blur-2xl + saturate-150, soft inset highlight on top edge, white-alpha text). The Chat WIDGET (right side, 384px wide, data-op-id=canvas.chatWidget) and the Presence WIDGET (left side, 384px wide, data-op-id=canvas.presenceWidget) are HIDDEN BY DEFAULT -- the canvas is clean on entry. Users open each via a corner restore button; closing returns to the clean state. (CoPresent Control v2 RETIRED the old "CoPresent Control Widget" / copresentControl.widget.* -- Takeover is a clean autopilot with no widget, and Guide narration rides forced voice + the GuideConsentCard, not a widget.) Both widgets share the liquid-glass styling (src/lib/theme/liquidGlass.ts -- glassWidgetRoot / glassText / glassAccentButton / glassControlSurface). IMPORTANT: chat + presence widgets are MUTUALLY EXCLUSIVE with their corresponding side panels. Opening the Presence widget COLLAPSES the left panel; canvas expands into that space. Opening the Chat widget COLLAPSES the right column ONLY when chat was the right-column tile (rightView === 'chat'); otherwise the chat widget floats alongside. Entering FOCUS-CANVAS auto-shows both widgets; exiting auto-hides them. During walkthroughs, do NOT open/close widgets -- the user's layout is the starting point.`,
	},
	{
		SourceRef: "concept:surfaceAwareness",
		Text:      `Every uiReadState response and every tool result's after-state line includes a ` + "`surfaces`" + ` field: a compact list of the canonical top-level regions currently mounted and targetable in the DOM. Canonical surface op-ids: ` + "`presence.panel`" + ` (left presence panel), ` + "`spacePage.chatRightPanel`" + ` (right chat panel), ` + "`canvas.presenceWidget`" + ` (floating presence widget on the canvas), ` + "`canvas.chatWidget`" + ` (floating chat widget on the canvas). CHECK ` + "`surfaces`" + ` BEFORE NARRATING about participants, video/audio controls, chat input, or anything surface-scoped -- narration must match what the user actually sees. Pairings are mutually exclusive: presence.panel XOR canvas.presenceWidget (never both; canvas-expand unmounts the panel entirely), spacePage.chatRightPanel XOR canvas.chatWidget for chat. Phrase narration accordingly: "Presence widget" / "Chat widget on the canvas" when the canvas.* entries are in ` + "`surfaces`" + `, "Presence panel" / "Chat panel" when the panel entries are. Do NOT default to "panel" when the widget is what's on screen -- users find it disorienting ("the cursor is over the widget but the agent is describing the panel"). If the required surface isn't in ` + "`surfaces`" + ` and the task needs it, prefer opening the PANEL via the header nav tile over toggling the widget -- the panel is the primary surface; the widget is a canvas-mode overlay.`,
	},
	{
		SourceRef: "surface:canvas.presenceWidget",
		Text:      `The Presence WIDGET (data-op-id=canvas.presenceWidget) is the floating left-side presence surface in FOCUS-CANVAS mode. It is NOT a mirror of the Presence panel -- it REPLACES it. When canvas is expanded, the regular PresencePanel is UNMOUNTED (leftColumnMounted=false on SpacePage); presence.panel is absent from the DOM. The widget is then the only presence surface the user can see, and every presence.* op-id (presence.camera-toggle, presence.mic-toggle, presence.devices, presence.layout.gallery, presence.layout.spotlight, presence.invite, per-participant cards under presence.participant.<id>) is rendered INSIDE the widget -- same underlying MediaControls / GalleryLayout / SpotlightLayout components, just hosted in the widget's 384px liquid-glass frame. HOW TO TELL WHICH SURFACE YOU ARE ON: read the ` + "`surfaces`" + ` list in uiReadState / the after-state line of every tool result. If ` + "`canvas.presenceWidget`" + ` is listed, narrate in terms of "the Presence widget on the canvas"; DO NOT say "the Presence panel" -- the panel does not exist on screen. If ` + "`presence.panel`" + ` is listed instead, narrate in terms of "the Presence panel on the left". These are mutually exclusive -- only one will be in surfaces at a time. Visibility toggle is canvas.widgetsVisibility (same op-id as the chat widget's toggle; subject="presence" differentiates "Hide presence" vs "Show presence"). Walkthrough rule: DON'T open/close the widget as part of the walkthrough -- the user's chosen layout is the starting state; narrate against whatever is already on screen. Only open a panel/widget if the task genuinely requires a surface that isn't currently present.`,
	},
	{
		SourceRef: "surface:canvas.chatWidget",
		Text:      `The Chat WIDGET (data-op-id=canvas.chatWidget) is the floating right-side chat surface that appears in FOCUS-CANVAS mode or when the right column is on a non-chat view. It is NOT a mirror of the right-column chat panel -- it REPLACES it. When canvas is expanded or the widget is open over a non-chat right column, the chat right-column panel (data-op-id=spacePage.chatRightPanel) is UNMOUNTED (rightColumnRendered flips false); spacePage.chatRightPanel is absent from the DOM. The widget then hosts every chat.* op-id: chat.input (free-form composer), chat.send (send button -- bottom-row action and inline on the input when the textarea has content), chat.mic (speech-to-text toggle), and the implicit message list (one row per utterance, no single chat.messages op-id). HOW TO TELL WHICH SURFACE YOU ARE ON: read the ` + "`surfaces`" + ` list in uiReadState / after-state. If ` + "`canvas.chatWidget`" + ` is listed, narrate "the Chat widget on the canvas"; if ` + "`spacePage.chatRightPanel`" + ` is listed, narrate "the Chat panel on the right". Mutually exclusive. Primary uses during a takeover: (1) show the user how to send a message (uiHighlight chat.input -> uiAskUser "what would you like me to type?" -> uiType into chat.input -> uiHighlight chat.send -> user confirms -> uiClick chat.send). (2) Show where speech-to-text lives (uiPointerTo chat.mic). Visibility toggle is canvas.widgetsVisibility with subject="chat" ("Hide chat" / "Show chat"); same op-id as the presence widget's toggle, differentiated by subject. Walkthrough rule: DON'T open/close the widget -- narrate against the starting layout.`,
	},
	{
		SourceRef: "concept:lockedAlignedStyle",
		Text:      `The Create Agent modal's Personality Style section has a LOCKED default chip called "Aligned" (op-id createAgent.style.aligned) that sits first in the row, is always selected, and cannot be deselected. Aligned represents the organization-wide baseline voice every CoPresent agent carries -- professional, helpful, consistent across conversations. The other chips (Friendly, Professional, Assertive, Empathetic, Analytical, Creative, Patient, Concise) are layered ON TOP of Aligned, not alternatives to it. During a walkthrough, do NOT ask the user whether to include Aligned (it's not a choice) -- only ask which ADDITIONAL styles they want. If the user says "no special personality" or "just default", that's fine: Aligned alone is a complete valid configuration. When narrating this section, mention that Aligned is always on and the user's picks are extras, so they don't feel like the form is missing something.`,
	},
	{
		SourceRef: "concept:lockedDefaults",
		Text: `Across every CoPresent form, the only fields pre-selected by default are LOCKED items -- the user cannot uncheck them, and neither can you. DETECTION: a locked row shows (a) a small lock glyph (LockClosedIcon) on its right edge, and (b) either the disabled attribute on its <button>, or is rendered as a <div> instead of a button at all (the pinned auto-join GA rows follow this pattern and carry data-op-intent="info"). uiReadState flags both shapes as non-interactive / locked. RULE: NEVER emit uiClick on a locked row. The click is swallowed -- the cursor flies for no visible effect, the user loses confidence, and you burn an iteration. Full per-modal roster of locked items:

CREATE AGENT (route modal:createAgent):
- Personality tab: createAgent.style.aligned ("Aligned", the organisation-wide baseline voice; always on; every agent carries this on top of any user picks).
- Skills tab: the universal-toolkit skills are locked-on for every role -- createAgent.skill.<slug> rows that render with a lock glyph and cannot be removed (the baseline every agent carries). the UI-driving skills copresent_takeover + copresent_guide are GA-ONLY and locked-on (they tie the General Assistant to the Takeover + Guide features so she can drive the UI); they replaced the retired copresent-control and do NOT appear as opt-in rows in the specialist Create Agent modal. (There is NO Knowledge tab in this modal -- the org-wide baseline knowledge domain is managed in Training, not here; createAgent.domain.<slug> / createAgent.capability.<slug> / createAgent.integration.<slug> do NOT exist.)

CREATE SPACE (route modal:createSpace): createSpace.activeAssistant -- a PINNED non-clickable row at the top of the participant selector. Shows the owner's GA name plus the subtitle "Assistant - Joins automatically". It is a <div>, not a button, so it has no aria-pressed, no checkbox, and no onClick -- uiClick is a category error here, not just a no-op. The backend auto-joins the GA via the autoJoinSI automation regardless of what the picker sends, so there is nothing to toggle. (The space participant picker selects HUMANS (createSpace.human.<id>) and GROUPS (createSpace.group.<id>) via the createSpace.tab.<id> tabs -- specialist agents are NOT space participants, so there is no createSpace.agent.<id> row.)

CREATE GROUP (route modal:group): createGroup.generalAssistant -- a PINNED non-clickable row at the top of the agents picker, rendered ONLY when the signed-in user is a selected member of this group. Subtitle: "Assistant - Joins because you're a member". Same <div>-not-button semantics as the Create Space pinned row. The GA auto-joins because its owner (the current user) is in the group; if the user removes themselves from the member list, the pinned row disappears and the GA no longer auto-joins.

Everything else in these forms starts UNSELECTED and requires an explicit user pick. This contrasts with many apps that pre-fill "reasonable defaults" for optional fields; CoPresent deliberately doesn't, so the user's choices feel intentional. During walkthroughs, NEVER assume an optional field has a sensible default you can fill without asking -- Gender, Personality styles beyond Aligned, additional Knowledge Domains, additional Skills (capabilities + integrations beyond the locked defaults), additional humans/groups in Create Space, additional members/agents in Create Group all need user input (via uiAskUser with 2-3 concrete options). EXCEPTIONS: (a) role-derived inference -- if the user said "IT support agent", the ROLE field can be populated without asking (it-support), but NAME still needs to be asked. (b) The Intelligence policy stays at "balancedChat" unless the user mentions a specific need (fast coding, strong reasoning, low-latency voice, cheapest). (c) The model-pin selector (createAgent.explicitModel) stays untouched unless the user explicitly asks to pin a model.`,
	},
	{
		SourceRef: "surface:createSpaceModal",
		Text:      `The Create Space modal (op-id createSpace) is a two-phase form similar to Create Agent. Phase 1 (Describe) takes a natural-language description; Phase 2 (Configure) lets the user set the title (createSpace.title), description (createSpace.description), and pick PARTICIPANTS via tabbed pickers (createSpace.tab.<id> switches between Humans and Groups): humans at createSpace.human.<id> per row, groups at createSpace.group.<id> per row (createSpace.tab.group.createLink jumps to group creation). SPECIALIST AGENTS ARE NOT SPACE PARTICIPANTS -- there is no createSpace.agent.<id> row; only the owner's Assistant participates, and it auto-joins. The Assistant is rendered as a PINNED locked row at the top (data-op-id=createSpace.activeAssistant) showing the GA's name plus the subtitle "Assistant - Joins automatically". That row is a <div>, not a button -- it has no checkbox, no aria-pressed, no onClick -- so do NOT uiClick it; see concept:lockedDefaults. Human/group rows BELOW it are the pickable ones (one button per row, carrying aria-pressed for already-selected detection), up to the remaining human-slot budget. The "Configure manually" shortcut (createSpace.configureManually) skips Phase 1. Commit: uiHighlight createSpace.submit, then uiAskUser({question: "Ready to create the <Title> space?", options: ["Yes, create it", "Cancel"], allowFreeForm: false}). On "Yes" uiClick createSpace.submit yourself. On "Cancel" release. This is the COMMIT CONFIRMATION pattern (concept:commitConfirmation) -- the user's option pick is the consent signal, replacing the older "never click commit" rule so create-agent -> create-space chains can complete in one session.`,
	},
	{
		SourceRef: "concept:commitConfirmation",
		Text:      `COMMIT CONFIRMATION is the required pattern for EVERY mutating click (Create / Save / Delete / Send / Invite / Remove / Confirm / Sign-Out). Before clicking the commit button, the agent MUST: (1) uiHighlight the commit button so the user's eye is on it. (2) uiAskUser({question, options: ["Yes, <verb> <subject>", "Cancel"], allowFreeForm: false}) -- binary consent gate, not open-ended. The question names the specific subject (NAME / TITLE) so the user confirms a SPECIFIC object. (3) On "Yes" -- uiClick the commit button yourself; the user's pick IS the consent. Then narrate and chain into any follow-up step. (4) On "Cancel" or empty answer -- narrate that the walkthrough can't complete without this commit and release. This replaces the older "never click commit" rule. Rationale: requiring the user to manually press Create forced them to take back control, which cancelled the session and broke multi-step walkthroughs like "create an operations agent, then create a weekly meeting space with that agent." The uiAskUser pick is a STRONGER consent than a silent button press because the user read a sentence naming the exact subject. Destructive actions (delete, sign out all, kick participant) use the same pattern but with harder warning copy ("This can't be undone." / "You'll need to log in again elsewhere.").`,
	},
	{
		SourceRef: "surface:settings.takeover.cursorSpeed",
		Text:      `The Takeover Cursor Speed slider (data-op-id=settings.takeover.cursorSpeed) lives on the Settings panel under Settings -> Takeover. Container chain: nav.settings -> settings.takeover. It governs the agent cursor speed during a Takeover (the single clean-autopilot experience; there is no separate Standard/Interactive mode anymore). It has a COUNTER-INTUITIVE inverted mapping you MUST understand before driving it: the slider's INPUT VALUE is NOT the ms duration shown in the readout -- they are inverted via the formula sliderValue = MAX+MIN - cursorTweenMs (MAX=2500, MIN=250). What that means in practice: slider value 250 = visual LEFT = "Slow" label = cursorTween 2500ms (slowest cursor); slider value 2500 = visual RIGHT = "Fast" label = cursorTween 250ms (fastest cursor). The readout next to the slider ("Xms per move") displays the ACTUAL duration -- low ms (250ms) = fast, high ms (2500ms) = slow. COMMON MISTAKE: seeing "2500ms per move" and typing 100 into the slider to "go faster." That clamps to 250 (the slider's min), which leaves the cursor at the SLOWEST setting. CORRECT BEHAVIOUR: To set max speed, uiType('2500') into the slider input. To set min speed, uiType('250'). The confirm happens automatically on change (persisted to v1:identity:user.preferences.cursorTweenMs in memQL on the server, not the browser, so the value follows the user across devices). Alternative phrasings the user might say: "max speed", "fastest cursor", "slow it down", "speed up the agent" -- all map to either 2500 (fast) or 250 (slow) as the VALUE typed into the range input, never the ms duration.`,
	},
	{
		SourceRef: "surface:settings.takeover.appearance",
		Text:      `The Takeover Appearance toggle (data-op-ids: settings.takeover.appearance.clean and settings.takeover.appearance.dim) lives on the Settings panel under Settings -> Takeover. Container chain: nav.settings -> settings.takeover. Two values: 'clean' (default; rest of the app is disabled but not visually darkened -- agent cursor + highlight spotlight are the only takeover cues) and 'dim' (rest of the viewport tinted dark with a cursor-following spotlight; stronger "agent is driving" signal). Clicks are blocked across the entire viewport in BOTH options -- this only changes the visual appearance. It applies to the single Takeover (clean autopilot) experience; there is no longer a separate "Interactive Mode" that overrode it. Persists to v1:identity:user.preferences.takeoverMode on the server, so the choice follows the user across browsers and devices. Phrasings: "dim mode", "clean mode", "darken the app when agent drives", "no dim during takeovers" -- all map here. uiClick the matching button to set; aria-pressed marks the active one.`,
	},
	{
		SourceRef: "surface:guide",
		Text:      `CoPresent GUIDE (skill slug copresent_guide; copresent#190) is the immersive, voice-to-voice walkthrough -- the AI does it WITH the user, narrating an ordered sequence of Scenes while driving the live UI + Canvas. It is the counterpart to Takeover (copresent_takeover, clean autopilot, the AI does it FOR you); both are GA-only locked-on skills that replaced the retired copresent_control. A Guide is a PERSISTED, re-runnable object (an ordered list of Scenes, each carrying narration intent + Canvas action directives + an interruptibility contract); the Guide/Scene data lives in memQL (queryable via the guide queries) while the live RUN-state (current Scene, running/paused/ended, raise-hand queue) is client-only. ENTRY IS CONSENT-FORWARD: the agent asks in conversation, then a single-confirm GuideConsentCard surfaces -- data-op-id guide.consent with guide.consent.accept and guide.consent.decline (it is NOT a mini-chat / widget). On accept, VOICE IS FORCED ON (the direct voice session engages, mic un-muted) and never falls back to text for the duration of the Guide; on decline the Guide ends. While running, the agent narrates Scene by Scene and drives the UI with the SAME operator primitives takeover uses (uiClick, uiNarrate, uiPointerTo, uiHighlight, ...) plus the canvas annotation primitive (see concept:canvasAnnotation). Scenes carry a per-Scene interruptible flag; pause / resume and a raise-hand queue let the user interject (raise-hand is stubbed pending copresent#191). The lifecycle is: idle -> entering(consent) -> running <-> paused -> ended. WALKTHROUGH GUIDANCE: a request like "walk me through the app" / "give me a tour" / "show me how to set up an agent, narrate it" is a GUIDE; a request like "just create an ops agent for me" / "change the theme to dark" is a TAKEOVER. See concept:takeoverVsGuide for picking between them.`,
	},
	{
		SourceRef: "concept:multiStepWalkthrough",
		Text:      `Multi-step walkthroughs chain two or more create / configure flows in one Takeover session. Canonical example: "Create an operations agent, then create a weekly meeting space and invite my team." Steps: (1) uiRequestControl({reason: "Setting up your operations agent + meeting space"}). (2) Drive create-agent form field-by-field (see surface:createAgentModal.walkthroughCadence). (3) At createAgent.submit, COMMIT CONFIRMATION pattern: highlight + uiAskUser("Ready to create <Name>, your Operations agent?") + on "Yes" click submit yourself. Session stays live. (4) Narrate the transition: "Agent created -- now let's make the meeting space." (5) Navigate to create-space (uiClick nav.spaces or equivalent), click + NEW, click Configure manually, fill title ("Weekly Operations Meeting"). NOTE: specialist agents are NOT added to a space via the picker -- the space picker selects HUMANS (createSpace.human.<id>) and GROUPS (createSpace.group.<id>); the owner's Assistant auto-joins. If the user wants their team in the space, toggle the relevant createSpace.human.<id> / createSpace.group.<id> rows. (6) At createSpace.submit, COMMIT CONFIRMATION again: highlight + uiAskUser("Ready to create the Weekly Operations Meeting space?") + on "Yes" click submit. (7) uiReleaseControl with a summary naming both created objects. Critical rules: do NOT uiReleaseControl between steps -- one session covers the whole chain. Do NOT force the user to press the first commit manually (that cancels the session). The uiAskUser confirmation at each commit is the consent gate that lets the chain survive.`,
	},
	{
		SourceRef: "surface:tasks",
		Text:      `The Tasks right panel (data-op-id=tasks.listPanel, opens at ?panel=tasks via uiClick nav.tasks) lists every Plan in the active space. A Plan is a unit of work the planner orchestrates -- v0.1's only Plan kind is 'analyzeFile' (created automatically when the user drops a file in chat); future kinds (analyzeText, conductResearch, executeWorkflow, etc.) will land here as the planner gains capabilities. Plans are grouped into THREE collapsible sections: Active (statuses: queued / routing / running), Needs attention (status: failed -- v0.2 also: needsAgent, awaitingFeedback), and Done (statuses: succeeded / cancelled, last 7 days by default). Each row shows: a kind icon, status pill, the Plan's goal text (e.g. "Analyze Q3-headcount.xlsx"), the assigned agent (if any), the Plan's duration once finished, and a relative timestamp. Per-row op-ids: tasks.row.<planId>.toggle (expand drawer with input/output JSON), tasks.row.<planId>.cancel (DISABLED in v0.1 -- the cancel mutation lands in v0.2). Empty state when no Plans exist: "Drop a file in chat to start one." The Tasks page deep-links via ?panel=tasks&plan=<planId> (the plan.completed canvas card's "View task" button drives this) -- the panel auto-expands the matching row. v0.2 adds: per-Plan token-spend bar, needs-feedback amber badge with one-click respond, refinement count, expand-to-Tasks listing under a Plan, time-window filter chip on the Done section.`,
	},
	{
		SourceRef: "concept:planner",
		Text:      `The PLANNER is the system that turns "user actions worth tracking" (file drops, chat-triggered analysis requests, future research / workflow goals) into Plan + Task records the user can see and act on. Two concepts model the work: v1:planner:plan (the user-visible unit -- one Plan per dropped file in v0.x) and v1:planner:task (one executable step inside a Plan; v0.x always has exactly one Task per analyzeFile Plan, kind 'fileProcessor'). The frontend surfaces the planner via TWO places: (1) the canvas, which lands a 'plan.created' card at start and a 'plan.completed' card on every Plan terminal transition (succeeded / failed / cancelled) showing the result summary + Validate / Reject / Refine / Attach-to-domain actions, and (2) the Tasks right panel (?panel=tasks) listing every Plan grouped by status. Lifecycle covers: queued -> routing -> running (with sub-states paused / awaitingFeedback / needsAgent) -> succeeded | failed | cancelled. v0.x collapses these transitions into one synchronous pass inside the file-upload handler -- the analysis runs inline, the Plan + Task + Document records are stamped with timestamped transitions, and the canvas cards land. Subsequent rounds lift the analysis itself into a planner-owned async execution surface; the Plan goes 'queued' on file drop, the user can keep chatting, and the plan.completed card lands when the Plan terminates. From the user's vantage the synchronous -> asynchronous jump is invisible: same UI, same shapes, just longer wall-clock between transitions on long-running work. WALKTHROUGH GUIDANCE: when a user asks "what tasks are running" / "what's the planner doing" / "did my file analysis finish" / "show me the analysis history" -> uiClick nav.tasks. When a user asks about a specific file analysis they just dropped -> point them to the plan.completed canvas card on the active space's canvas and offer to open ?panel=tasks for the full list. When a user wants to refine an analysis ("look at column X again", "categorize by department instead") -> open the plan.completed card and uiClick canvas.card.planCompleted.refine.toggle, then narrate the inline composer that spawns a child Plan. The planner is NOT the same thing as a UI-driving session (Takeover / Guide) -- the planner orchestrates background work the system does on the user's behalf; Takeover/Guide is the user authorizing an agent to drive their UI for a bounded session.`,
	},
	{
		SourceRef: "surface:knowledge",
		Text:      `The Knowledge right panel (data-op-id=knowledge.listPanel, opens at ?panel=knowledge via uiClick nav.knowledge) is the user's library of validated content the agents draw from when answering. Two-column layout: LEFT is the domain list (grouped by category: Core, Business, Technical, Product, Internal) with a "+ New" pill button at the top right of the panel header (data-op-id=knowledge.new) that opens an inline create form. RIGHT is the selected-domain detail showing the domain's name + description + drop-zone for direct file upload + the list of attached validated Documents. Per Q21: domains carry a SCOPE -- 'workspace' (default; visible to everyone in the workspace) or 'private' (visible only to the creator). The create form's scope picker has two pills: createKnowledge.scope.workspace and createKnowledge.scope.private. Per-domain row op-id: knowledge.row.<domainId>.select (open the detail view). There is NO per-row dropzone op-id -- file upload happens via the drop zone in the selected-domain DETAIL view, not on the row. Detail view: the drop zone target accepts PDF, DOCX, text, markdown, image (25MB max); files dropped here flow through the same Plan + analyze pipeline as chat-originated uploads but with the target domain pre-set. Attached Document rows show fileName + itemCount + a checkmark indicating validation. WALKTHROUGH GUIDANCE: when the user wants to "add knowledge", "create a domain", "make a knowledge area", "set up an HR knowledge base", "give the agent reference material" -> uiClick nav.knowledge then uiClick knowledge.new. When the user has just analyzed a file and wants to attach it to a knowledge domain, the plan.completed canvas card has a built-in "Attach to domain ▾" picker -- prefer that path over re-uploading the file via the Knowledge page.`,
	},
	{
		SourceRef: "concept:validation",
		Text:      `Per Q15: every analyzed Document carries a VALIDATION STATUS gating whether its content can be used as canonical truth. States are: 'unvalidated' (just analyzed; usable as reference for research/exploration but NOT as authoritative source); 'validated' (the user reviewed and approved -- this is the only status that lets a Document be ingested into a knowledge domain); 'rejected' (user soft-deleted; agents skip it for reasoning); 'partiallyValidated' (some items validated, some not -- only the validated items propagate); 'superseded' (a newer Document took over). The validation flow lives on the plan.completed canvas card: when the analyzer finishes a Document, the card shows [Validate] (canvas.card.planCompleted.validate) and [Reject] (canvas.card.planCompleted.reject) action buttons. Once validated, the card surfaces an [Attach to domain ▾] picker (canvas.card.planCompleted.attachDomain.toggle) listing every active knowledge domain; selecting one writes mutationAttachDocumentToDomain so the Document propagates into that domain's retrieval surface. Validation also unlocks the [Refine ...] action (canvas.card.planCompleted.refine.toggle), which opens an inline composer for the user to give feedback ("look at column F more carefully") -- submitting spawns a child Plan with kind='refineAnalysis' that the handleRefinementPlan automation drives through its own lifecycle. Validation is per-Document at the v0.x surface; per-item validation (Q15 hybrid granularity, e.g. "rows 1-50 yes, 51-100 no") ships in subsequent rounds via a dedicated drawer. WALKTHROUGH GUIDANCE: never click Validate on the user's behalf without explicit approval (validation marks data canonical and propagates downstream); always uiAskUser to confirm before clicking canvas.card.planCompleted.validate or canvas.card.planCompleted.reject.`,
	},
	{
		SourceRef: "surface:training",
		Text:      `The Training right panel (data-op-id=training.listPanel, opens at ?panel=training via uiClick nav.training) is THE place to change an agent's knowledge and skills. The Knowledge tab was removed from the Create Agent / Edit Agent modal -- Training is the ONLY surface for knowledge. (Skills remain editable in the modal's Skills tab too; Training is just the richer surface for them.) Layout: a panel header with the title "Training" and the subtitle "Drag items onto the Studio card on the canvas, then Train", a three-tab strip (training.tab.agents / training.tab.knowledge / training.tab.skills), a search input scoped to the active tab (training.search.<tab>), and a scrollable palette of draggable rows. PALETTES: (a) AGENTS palette (training.palette.agent.<bareId> per row) -- lists every active agent, sorted with the Assistant (Sofia) PINNED at the top, then a hairline "Other agents" divider, then the rest alphabetically. The GA row is visually grayed (opacity 60%) and carries a "Read-only" lock pill -- users can drop her on the Studio to INSPECT her config, but the Studio renders a read-only banner and locks every zone. Sofia's training is system-managed via provisionAssistantOnUserCreate; user-side training would be undone on the next cluster boot. (b) KNOWLEDGE palette (training.palette.domain.<id> per row) -- lists every workspace + private knowledge domain the user has access to. Drag onto the Studio's Knowledge zone to stage. (c) SKILLS palette (training.palette.tool.<slug> per row) -- lists agent capabilities + integrations. The list HIDES bundle primitives (workerHost / workerComputer / workerStatus / uiClick / uiType / etc.) and shows the umbrellas instead -- "Takeover" + "Guide" (GA-only) and "Computer Use". Users pick the umbrella; the runtime fans it out. INTERACTIONS: every palette row is BOTH clickable and draggable. Click an Agents-palette row -> select that agent into the Studio's slot (toggles off if you click the same row twice). Drag any row onto the Studio card on the canvas -> stage it in the matching zone. Search filters the active tab live. Switching tabs preserves search per-tab. Walkthrough flow: see surface:trainingStudio for the studio-card semantics + concept:trainingFlow for the multi-step orchestration.`,
	},
	{
		SourceRef: "surface:trainingStudio",
		Text:      `The Training Studio is a per-canvas card (opId family training.studio.*) that appears when the user is on the Training panel. It is the drop target for the Training panel's drag-able rows, and it commits the staged changes via the Train button. Three drop zones, top to bottom: (1) AGENT slot (training.studio.clearAgent.<bareId> for the clear button when an agent is staged) -- accepts an agent from the Agents palette; replaces on re-drop; click the × to clear. The slot displays the agent's name + role label. (2) KNOWLEDGE zone -- accepts knowledge domains. Each staged domain renders as a chip (training.studio.domain.<domainId>); FILLED chips are already-trained on the agent, OUTLINED chips are newly added and will be embedded on the next Train run. Locked chips (core baseline + tool-required domains) cannot be removed. (3) SKILLS zone -- accepts skills. Each staged skill renders as a chip (training.studio.tool.<slug>); same fill / outline / lock semantics as Knowledge. The Skills zone HIDES every bundle primitive (workerHost, workerComputer, workerStatus, uiClick, uiType, ...); only the umbrellas (Takeover, Guide, Computer Use) appear. The TRAIN button (training.train) sits below the three zones. Disabled when no agent is staged or there are no unsaved changes. Label morphs: "Train <Name>" when there's something to commit; "Nothing to train" when there isn't. Click Train -> creates a Plan + 3 Tasks visible on the Tasks panel, lands a training.completed canvas card on the active space when the Plan succeeds. No popup toast; progress + outcome live on the Tasks panel. READ-ONLY MODE: when the slot agent is the Assistant (Sofia), the Studio renders the ReadOnlyAgentBanner ("<Name> is read-only -- her training is system-managed"), the three zones are grayed and locked (no drop, no chip remove ×), and the Train button label morphs to "<Name> is read-only" + becomes disabled. The slot still accepts the GA via drop (so users can inspect Sofia's domains/skills in the chips), it just refuses to commit. IN-FLIGHT MODE: while a trainAgent Plan is running for the slot agent, the Studio shows TrainingInFlightBanner with a "View tasks" deep-link button (training.studio.inflight.viewTasks.<planId>) -- the staged set is frozen, drop / remove / Train are all locked, the agent stays usable in chat with its current state, and clicking "View tasks" jumps to ?panel=tasks&plan=<planId> so the user can watch progress or cancel from there.`,
	},
	{
		SourceRef: "concept:trainingFlow",
		Text:      `The canonical Training flow when the user says "give my Operations agent the email and finance-docs skills" (or similar -- adding knowledge / skills / both to a specific agent): (1) uiClick nav.training to open the Training panel + spawn the Studio card on the canvas. (2) AGENTS tab is the default. uiClick training.tab.agents if you're not already there. uiClick (or drag) training.palette.agent.<bareId> to put the target agent into the Studio's Agent slot. Confirm via uiReadState that the slot now shows the agent's name. (3) Switch to Knowledge: uiClick training.tab.knowledge. For each requested domain, drag (or select then drag-drop simulated) training.palette.domain.<id> onto the Studio's Knowledge zone. uiReadState to confirm a chip landed. Filled chips were already trained; outlined chips are new. Skip drops for domains already filled (they'd be a no-op). (4) Switch to Skills: uiClick training.tab.skills. For each requested skill, drag training.palette.tool.<slug> onto the Studio's Skills zone. The UI-driving umbrellas "Takeover" (slug copresent_takeover) + "Guide" (slug copresent_guide) and the machine-driving umbrella "Computer Use" (slug computer-use) cover all the operator / worker primitives -- never look for individual workerHost / uiClick chips, those are hidden. (5) When the staged set matches the user's request, COMMIT CONFIRMATION: uiHighlight training.train, uiAskUser({question: "Ready to train <Name> on <list of new items>?", options: ["Yes, train <Name>", "Cancel"], allowFreeForm: false}). On "Yes" uiClick training.train yourself. (6) Narrate that progress lives on the Tasks panel; offer to chain into ?panel=tasks if the user wants to watch. (7) Land the cursor on the space-context pill (concept:releaseLanding) and uiReleaseControl with a summary naming the agent + the items applied. EDGE CASES: (a) Sofia/GA in the slot -- the Studio is read-only; abort the flow with narration "<Name> is your Assistant -- her training is system-managed and can't be edited from here. Want me to spin up a specialist instead?" Do NOT click training.train (it's disabled). (b) An in-flight training Plan exists -- the Studio is frozen with TrainingInFlightBanner. Narrate the lock + offer to deep-link to ?panel=tasks via the banner's "View tasks" button. (c) The user wants to remove a skill or domain rather than add: same flow, but instead of dragging from the palette, click the × on the chip (chips are training.studio.domain.<id> / training.studio.tool.<slug>); then Train commits the removal.`,
	},
	{
		SourceRef: "concept:agentSkills.umbrellas",
		Text:      `CoPresent has umbrella skills in the agent skill catalog whose user-facing label hides a backend fan-out. The UI-driving umbrellas are "Takeover" (slug copresent_takeover) and "Guide" (slug copresent_guide) -- BOTH GA-only and locked-on; they replaced the retired single "CoPresent Control" (slug copresent-control, copresent#187). The machine-driving umbrella is "Computer Use" (slug computer-use). The umbrella is what the user picks; the backend stores the fan-out primitives in capabilities.tools. Takeover AND Guide both fan out into the SAME operator primitives (uiClick, uiType, uiSelect, uiHighlight, uiNavigate, uiReadState, uiAskUser, uiWaitFor, uiRetry, uiNarrate, uiRequestControl, uiReleaseControl, agentUpdateSelf, similarTo) -- the experience differs (autopilot vs voice Scenes), not the primitive set. Computer Use fans out into the worker primitives (workerHost, workerComputer, workerStatus). (The legacy copresent-control slug still expands to the same operator primitives so GA rows seeded before the split keep working until re-provisioned.) EVERY UI surface that renders skill chips (the Training Studio's SkillsZone, agent-card capability badges, Edit-mode chip lists, the Training panel's Skills palette) HIDES the primitive names and shows the umbrella label. The user never sees "workerHost" / "uiClick" / etc. as standalone chips. Helper isBundlePrimitiveSlug() in agentDefaults.ts is the single source of truth for "is this a fanned-out primitive?". WHEN AN AGENT HAS Computer Use: the agent receives workerHost (shell + filesystem + http), workerComputer (mouse + keyboard + screenshot), and workerStatus (live availability check). When asked "what skills do you have?" the agent should NAME THE UMBRELLAS, not the primitives -- "Takeover" / "Guide" / "Computer Use", not "uiClick" / "workerHost". Computer Use is selectable on EVERY agent (not GA-only) and is default-on for newly-created agents from the Create Agent modal. Existing agents do not get Computer Use auto-added on save; the user enables it via the Training panel.`,
	},
	{
		SourceRef: "surface:settings.computerUse",
		Text:      `Settings -> Computer Use (the Computer Use section on the Settings panel, sibling of the Takeover section; its controls carry computer-use.* op-ids -- there is no bare settings.computerUse op-id) is where the user pairs / unpairs / scopes their cockpit-gui worker. The cockpit-gui worker is a separate process the user runs on their own machine that receives mouse + keyboard + shell + filesystem + http calls from the agent (when the agent has the Computer Use skill). Two visual states: NOT CONNECTED -- "○ Not connected" status line + a [Connect this computer] button (data-op-id=computer-use.connect). Click opens the ConnectComputerModal, which displays a one-shot pairing CODE (data-op-id=connect-computer.copy-code) and a one-line shell COMMAND (data-op-id=connect-computer.copy-command, format: 'memql-cockpit-gui worker pair <code>') the user runs on their machine. CONNECTED -- "● Connected: <hostname>" status line + a sub-line "macOS arm64 · last seen Xs ago" + a "Capabilities: HEADLESS + GUI" badge + scope toggles (observe / interact / full; persist to v1:agents:agentAuthorization.computerUseScope on the GA's authorization row, default 'full') + [Disconnect] (data-op-id=computer-use.disconnect, revokes the worker_token; the cockpit-gui worker stops being able to connect) + [Connect another computer] (data-op-id=computer-use.connect-another, opens the same ConnectComputerModal for adding a second machine). The card live-polls workersForUserQuery every 4s + flips to "offline" after 30s without a heartbeat. WALKTHROUGH GUIDANCE: when the user asks "set up computer use", "let my agent control my computer", "pair my mac", "install the worker": uiClick nav.settings -> uiPointerTo computer-use.connect -> narrate the Connect button -> uiClick computer-use.connect -> read the modal's code + command aloud, do NOT click copy buttons (the user copies); do NOT close the modal until the user confirms the pair completed in their terminal. Disconnect is destructive (revokes the worker_token; cockpit must re-pair) -- COMMIT CONFIRMATION required before uiClick computer-use.disconnect.`,
	},
	{
		SourceRef: "surface:header.computerUsePill",
		Text:      `The Computer Use pill (data-op-id=header.computer-use-pill) is a floating header chip that ONLY renders when the user has at least one paired worker (revealed once the first ConnectComputerModal flow completes). It sits between the header nav-tile strip and the Profile pill. The pill is BOTH a visual indicator AND a one-click kill switch. Visual states: GLOBAL ENABLED + worker connected -> green dot + "Computer Use" label; GLOBAL ENABLED + no worker online -> amber dot; GLOBAL DISABLED -> red dot + slashed icon. CLICK opens a small popover with two buttons: (1) header.computer-use-pill.toggle -- flips the user-level GLOBAL kill switch (writes v1:identity:user.preferences.computerUseEnabled). When OFF, every workerHost / workerComputer / workerStatus call is rejected at the WorkerService gateway with a ` + "`computer_use_disabled`" + ` error, regardless of agent capabilities or scope. The pill is the user's "panic button" for stopping the agent without disconnecting the cockpit. (2) header.computer-use-pill.manage -- routes to ?panel=settings + scrolls to the Computer Use section (its Connect/Disconnect controls carry computer-use.* op-ids; see surface:settings.computerUse). WALKTHROUGH GUIDANCE: when the user says "stop the agent from controlling my computer" / "pause computer use" / "kill switch": uiClick header.computer-use-pill -> uiClick header.computer-use-pill.toggle -> uiNarrate "Computer Use is paused -- agents can't reach your machine until you flip this back on". When the user says "open computer use settings": uiClick header.computer-use-pill -> uiClick header.computer-use-pill.manage. The pill is NOT visible to users who never paired a worker; in that state route them through nav.settings -> settings.computerUse instead.`,
	},
	{
		SourceRef: "concept:computerUseAgentCapability",
		Text:      `When an agent has the "Computer Use" skill (capability slug computer-use, fanned out to workerHost + workerComputer + workerStatus on save), it can drive the user's own computer outside CoPresent: shell exec, filesystem read/write, HTTP fetch, mouse + keyboard + screenshot. Distinct from Takeover/Guide (which drive the CoPresent SPA itself). RUNTIME GATING -- four checks fire BEFORE any wire traffic: (1) Agent capability flag -- must include computer-use. (2) The user's standing scope on v1:agents:agentAuthorization.computerUseScope (observe = read-only fs + GET HTTP + screenshot/cursor/display info; full = everything: shell exec, fs_write, full HTTP, mouse + keyboard + scroll + window_focus). (3) Per-call action's required scope (e.g. workerHost.exec needs full; workerHost.fs_read needs observe; workerComputer.mouse_click needs full). (4) The user-level kill switch on v1:identity:user.preferences.computerUseEnabled. Out-of-scope calls park the calling Plan in awaitingFeedback with feedbackReason=scope_elevation_required; kill-switch denies return computer_use_disabled. AVAILABILITY STATES the agent reasons about (surfaced by the workerStatus tool + the per-turn computerUseStatus prompt seed): connected = a paired cockpit-gui worker is online RIGHT now (tool calls dispatch; detail = the worker hostname); disconnected = the user has paired before but the cockpit isn't running; unconfigured = the user has never paired a worker. When asked "what can you do?" the agent should mention Computer Use alongside its other capabilities and reflect the LIVE STATE -- "I can drive your computer; right now your cockpit isn't running, but once you start it I can run shell commands, manage files, and even drive your screen with mouse + keyboard." If unconfigured, the next move is uiClick nav.settings then uiPointerTo computer-use.connect (see surface:settings.computerUse). If disconnected, narrate that the cockpit needs to be running and walk them through opening Terminal + running 'memql-cockpit-gui worker run'. If connected, the Computer Use pill (header.computer-use-pill) is visible and acts as the live indicator + kill switch (see surface:header.computerUsePill).`,
	},
}

// computerUseSeedCorpus is the operational manual for the
// `computer-use` capability. Same shape as copresentUISeedCorpus
// (sourceRef + text) and ingested through the same handler. Each
// chunk is a self-contained paragraph anchored on a clear topic so
// RAG retrieval can pull the right chunk for a given user query
// without needing the full set in context.
//
// Authoring rules for adding chunks here (read before edits):
//   - Keep each chunk under ~2 KB. RAG ranks chunks individually;
//     fat chunks dilute relevance.
//   - Lead with the topic anchor in the first sentence (e.g.
//     "Scope tiers determine what a Computer Use call can DO ...").
//     The retriever embeds the whole chunk; the lead sentence
//     is what gives it semantic shape.
//   - Tool-name references stay verbatim: `workerHost`,
//     `workerComputer`, `workerStatus`, `requestComputerUseScope`.
//     The agent learns the wire-level names from tool definitions;
//     the chunk reinforces when to reach for each one.
//   - NO hardcoded user-task examples ("open Safari", "list
//     Downloads"). Pattern shape only. If a specific agent needs
//     curated examples, those land in a per-agent training source,
//     not in the standard seed.
var computerUseSeedCorpus = []struct {
	SourceRef string
	Text      string
}{
	{
		SourceRef: "computerUse:overview",
		Text:      `Computer Use is the capability that lets you drive the user's own computer outside CoPresent. It is distinct from Takeover/Guide: Takeover (autopilot) + Guide (voice Scenes) drive the CoPresent SPA you're embedded in (DOM op-ids, takeovers, walkthroughs), while Computer Use drives the user's MACHINE (shell commands, files, mouse, keyboard, screenshot) via a paired cockpit-gui worker process running on their hardware. The capability fans out into four primitive tools when an agent picks it: ` + "`workerHost`" + ` (shell exec / filesystem / HTTP fetch -- headless), ` + "`workerComputer`" + ` (mouse / keyboard / screenshot -- GUI-driving), ` + "`workerStatus`" + ` (cheap connectivity probe), and ` + "`requestComputerUseScope`" + ` (the per-task approval gate). When you mention this skill to the user, call it "Computer Use" (the umbrella name). Never expose the four primitives by name in user-facing text -- they are wire-level internals.`,
	},
	{
		SourceRef: "computerUse:toolSurfaces",
		Text:      `Two execution surfaces sit under Computer Use, each with a distinct shape: ` + "`workerHost`" + ` is HEADLESS -- it runs shell commands, reads/writes files, fetches URLs. Use it when the task has a one-shot command equivalent (filesystem operations, package installs, HTTP fetches, anything you'd type at a terminal). It's faster, more reliable, and easier to verify than scripted GUI input. ` + "`workerComputer`" + ` is GUI-DRIVING -- it moves the mouse, types on the keyboard, scrolls, takes screenshots. Use it when the task genuinely requires looking at or driving the user's screen (interacting with a native app that has no CLI, taking a screenshot of the desktop, clicking through a UI flow inside an app outside CoPresent). When the SAME task is achievable on either surface, prefer ` + "`workerHost`" + ` for the smaller blast radius and the cleaner contract -- unless the user explicitly asked for the cursor / keyboard path. ` + "`workerStatus`" + ` is a cheap probe with no side effects; call it when the cockpit's connectivity may have aged (the user told you mid-turn they started their cockpit; you're about to kick off a long workerHost / workerComputer flow). Don't spam it -- the per-turn computerUseStatus prompt seed is already fresh at turn start.`,
	},
	{
		SourceRef: "computerUse:scopeTiers",
		Text:      `TWO scope tiers determine what a Computer Use call can DO once the user has approved it. ` + "`observe`" + ` is read-only filesystem + GET HTTP + read-only screen probes (screenshot, cursor_position, display_info, window_list). Tool surfaces: workerHost.fs_read / fs_list / fs_stat / http_fetch (GET only) AND workerComputer's read-only actions. ` + "`full`" + ` is everything: shell exec, fs_write, full HTTP (any method), mouse, keyboard, scroll, window_focus. Tool surfaces: workerHost (any action) AND workerComputer (any action). Pick the LEAST scope that finishes the task -- if the user only needs you to "read what's in this folder" or "show me what's on screen", request observe; otherwise request full. The earlier ` + "`interact`" + ` middle tier (mouse + keyboard but NOT shell) was retired because it locked the agent into a single execution path when shell was often the cleaner one (e.g. "open Chrome" via 'open -a Chrome' is faster + more reliable than scripting cmd+space + type + return); the user is already approving "drive my machine", an arbitrary line between "GUI without shell" and "shell" doesn't help them reason about consent. If you ever see ` + "`interact`" + ` come back from a legacy authorization row, treat it as ` + "`full`" + `.`,
	},
	{
		SourceRef: "computerUse:perTaskApproval",
		Text:      `The user wants explicit per-task approval for every Computer Use action. Standing scope on v1:agents:agentAuthorization is bookkeeping -- it does NOT auto-approve work. Before you ever call ` + "`workerHost`" + ` or ` + "`workerComputer`" + `, you MUST first call ` + "`requestComputerUseScope`" + ` so the user sees a permission card on the canvas describing what you're about to do, the scope you need, and Allow / Deny buttons. This is non-negotiable -- the canvas card is the user's signal that you're about to do something on their machine, and skipping it cheats them out of consent even when the standing scope nominally covers the action. The flow has three parts: (1) Call ` + "`requestComputerUseScope({intent, requestedScope, summary})`" + ` BEFORE every workerHost / workerComputer call. The intent is the user's request restated as one short imperative. The summary is one paragraph the user reads on the canvas card explaining what you'll actually do, why, and how long it'll take. (2) End your turn with a short ` + "`respondToUser`" + ` along the lines of "I've requested <scope> access -- there's an approval card on your canvas; click Allow and I'll get to work." DO NOT call workerHost / workerComputer in the same turn -- the user's click on the card is the gate. (3) You do NOT need to wait for the user to re-ask. When they click Allow on the canvas card, the planner automatically dispatches a NEW turn back to you carrying planApprovedTrigger=true -- that turn (a separate prompt render) is where you actually run the work.`,
	},
	{
		SourceRef: "computerUse:postApprovalExecution",
		Text:      `When a turn arrives with planApprovedTrigger=true, the user already clicked Allow on the canvas permission card and the Plan transitioned to status=running. The planner forwarded this turn so you can do the work. Mandatory flow on this turn, in this exact order: (1) DO NOT call ` + "`requestComputerUseScope`" + ` again. The user just approved; calling elevation again would loop forever. The user-message in this turn's history IS the original goal; just execute it. (2) Dispatch the tool surface that fits the task. With ` + "`full`" + ` scope you may use either workerHost (shell, files, HTTP) or workerComputer (mouse, keyboard, screenshot) -- pick the most reliable path; shell is usually cleanest for things like "open <app>", "navigate to <URL>", "create / move / rename a file". With ` + "`observe`" + ` scope you only have read-only actions on workerHost (fs_read / fs_list / fs_stat / http_fetch GET) and read-only workerComputer probes (screenshot / cursor_position / display_info / window_list). (3) As soon as the worker tool returns ok=true, call ` + "`canvasPublish`" + ` in the SAME turn to drop a task-done card on the canvas (kind="document", data={format: "markdown", title, source}, importance="notify"). The source markdown should contain a one-line outcome stating what just landed, a short bulleted list of the concrete artefacts (file paths, command output, URLs touched), and any caveat the user should know. (4) End your turn with a short factual ` + "`respondToUser`" + ` stating what you just did. Short, no re-asking, no "let me know if..." padding. (5) If the worker call returns ok=false (cockpit unreachable, command errored, dispatcher denied even though the user approved), skip canvasPublish and explain the failure honestly in the respondToUser. Do not pretend success in your text and do not call requestComputerUseScope again on this turn -- the planner reads worker invocations and stamps the Plan succeeded vs failed automatically.`,
	},
	{
		SourceRef: "computerUse:planOutcomeSemantics",
		Text:      `The planner is the authority on whether a Plan succeeded or failed. After your post-approval turn finishes, the planner queries v1:worker:invocation rows belonging to this Plan id. Every workerHost / workerComputer call writes a row at completion with outcome ∈ {success, failure, cancelled, timeout, denied_by_scope, denied_by_policy, kill_switch_engaged, no_worker_available}. If at least one row has outcome=success, the planner stamps Plan status=succeeded and writes your reply text to Plan.output.reply. If zero rows have outcome=success (you never dispatched a worker tool successfully), the planner stamps Plan status=failed and writes your reply text to Plan.errorMessage so the user sees the actual reason in the Tasks panel. Practical implication: you cannot fake success in your respondToUser text. A turn that finishes without dispatching a worker tool at all (or dispatches one and it fails) lands as Plan failed, regardless of how the reply reads. Better to fail honestly with a reply explaining what blocked you than to pretend success and have the Tasks panel disagree with the user's lived experience.`,
	},
	{
		SourceRef: "computerUse:availabilityStates",
		Text:      `Three availability states surface to you per turn via the computerUseStatus prompt seed: ` + "`connected`" + ` -- a paired cockpit-gui worker is online RIGHT now and reachable; tool calls dispatch normally; the per-turn detail field carries the worker hostname. ` + "`disconnected`" + ` -- the user has paired a cockpit before but it's not running right now; tool calls will fail with no_worker_available; do NOT call workerHost / workerComputer in this state. ` + "`unconfigured`" + ` -- the user has never paired a worker; same fail mode as disconnected, plus the user needs to set up. When asked "what can you do?" / "what skills do you have?", reflect the LIVE state honestly: connected -> "I can drive your computer -- run commands, manage files, drive the screen with mouse + keyboard." Disconnected -> add "your cockpit isn't running right now; once you start it I can drive your machine." Unconfigured -> add "you'd need to set it up in Settings first." NEVER tell a user to grant scope in Settings ("go to Settings and grant Full / Observe scope") -- that UI does not exist; scope is granted per-task on the canvas, period. NEVER tell a user to restart the cockpit with a "--scope full" or similar flag -- no such flag exists; pairing has nothing to do with scope.`,
	},
	{
		SourceRef: "computerUse:errorBudget",
		Text:      `If ` + "`requestComputerUseScope`" + ` itself returns an error (rare; transient backend issue), tell the user honestly: "I tried to request access but the request didn't go through -- something's wrong on the backend. Try asking me again in a minute." Never substitute cockpit-restart fiction or Settings advice. If the user denied the permission card, the Plan transitions to status=cancelled with feedbackResponse.response="deny" -- you'll see this on a subsequent turn (no planApprovedTrigger; just the user re-engaging in chat). Acknowledge the denial briefly and offer alternatives within the user's standing scope. If the permission card auto-dismissed at the 3-minute timeout, the Plan is cancelled with feedbackResponse.response="timeout" -- treat it as a soft "no answer" and offer to re-request when the user is ready.`,
	},
}

// workbenchSeedCorpus is the operational manual for the `workbench-use`
// capability. Universal capability (every agent has it by default).
// Same shape as computerUseSeedCorpus -- chunks lead with a topic
// anchor and stay self-contained so RAG retrieval lands the right
// chunk for a given query.
var workbenchSeedCorpus = []struct {
	SourceRef string
	Text      string
}{
	{
		SourceRef: "workbench:overview",
		Text:      `The Workbench is your default working environment for any HEADLESS task -- writing files, running shell commands, fetching URLs from the open web. It is a per-Plan sandboxed Linux directory in the cluster; YOU drive it, the user does not see it as a filesystem they can browse, and nothing on the user's machine is touched. Reach for the workbench FIRST for any "do this task and produce a file / output" work. Computer Use (` + "`workerHost`" + ` / ` + "`workerComputer`" + `) is the FALLBACK -- use it only when the workbench cannot do the job (the task needs macOS-only tooling like Xcode, the user wants you to drive a GUI app on their machine, or the user explicitly asked you to work on a file already on their computer). The single tool surface for the workbench is ` + "`workbenchHost`" + `, discriminated by an ` + "`action`" + ` field. When you mention this skill to the user, you can call it "the workbench" or just describe what you're doing ("I'll write the report to a file"). The wire name ` + "`workbenchHost`" + ` is internal -- don't surface it.`,
	},
	{
		SourceRef: "workbench:actions",
		Text:      `The ` + "`workbenchHost`" + ` tool's ` + "`action`" + ` field discriminates six operations, all targeting the per-Plan workspace: ` + "`exec`" + ` runs a shell command (args ` + "`{cmd, cwd?, env?, stdin?, timeoutSec?}`" + `); ` + "`fs_read`" + ` reads a file as text (args ` + "`{path, maxBytes?}`" + `); ` + "`fs_write`" + ` writes a file, auto-creating parent directories (args ` + "`{path, content, mode?}`" + `); ` + "`fs_list`" + ` enumerates entries in a directory (args ` + "`{path}`" + `); ` + "`fs_stat`" + ` returns size / mode / mtime / isDir / exists (args ` + "`{path}`" + `); ` + "`http_fetch`" + ` makes an HTTP request from the workbench (args ` + "`{url, method?, headers?, body?, timeoutSec?}`" + `). All paths are RELATIVE to the workspace root; absolute paths (` + "`/etc/passwd`" + `) and ` + "`..`" + ` traversal are rejected. Prefer ` + "`fs_write`" + ` for producing a structured deliverable (the user can later retrieve the file via the cockpit); prefer ` + "`exec`" + ` for "do this then capture the output" work where the file artifact isn't the goal.`,
	},
	{
		SourceRef: "workbench:environment",
		Text:      `The workbench runs LINUX, not macOS. Write your shell with ` + "`apt`" + ` / ` + "`pip`" + ` / ` + "`npm`" + ` rather than ` + "`brew`" + `. The workspace starts empty -- no user files, no user environment variables, no home directory. There is no ` + "`/etc`" + ` to inspect, no ` + "`~/Library`" + ` to read, no ` + "`/Applications`" + ` to launch. The cwd defaults to the workspace root; if you need a subdirectory, create it with ` + "`fs_write`" + ` (parent dirs auto-create) or ` + "`exec`" + ` ` + "`mkdir -p`" + ` and then pass ` + "`cwd`" + ` to subsequent exec calls. Available tooling includes common Unix utilities and the runtimes seeded by the workbench image -- assume curl / git / a Python interpreter / a Node interpreter are present; verify with ` + "`which X`" + ` if uncertain. Anything you ` + "`apt install`" + ` mid-Plan stays for subsequent calls within the same Plan because the workspace persists.`,
	},
	{
		SourceRef: "workbench:persistence",
		Text:      `The workspace persists for the LIFE of the parent Plan. A file you ` + "`fs_write`" + ` on Task 1 is still there when Task 2 ` + "`fs_read`" + `s it -- this is how agents collaborate on the same Plan without re-persisting through chat. Use this: write notes to ` + "`notes.md`" + `, scratch files to ` + "`tmp/`" + `, deliverables to a clean filename at the workspace root. When the parent Plan reaches a terminal status (succeeded / failed / cancelled), the workspace is torn down and the files go away -- if you want the user to have any of them, surface them via ` + "`canvasPublish`" + ` (kind=document with the file contents as markdown, or kind=card pointing at the artifact) BEFORE the Plan ends. Don't assume the user will go fetch a file from a workspace they can't browse.`,
	},
	{
		SourceRef: "workbench:failureFallback",
		Text:      `When the workbench can't do the job (the task genuinely requires macOS / Xcode / a GUI app on the user's machine / a file the user has locally), DON'T silently switch to computer-use. The clean signal: end your turn with a ` + "`respondToUser`" + ` that names the limitation concretely ("I can't build the iOS app from the workbench -- that needs Xcode on a Mac. To do this we'd need to use Computer Use to drive your machine."). The planner sees the turn outcome and decides whether to re-route the task with computer-use capabilities granted. Same pattern for ambiguous failures: an ` + "`exec`" + ` that exits with a missing-binary error is a "workbench can't do this" signal -- surface it honestly rather than retrying with random variations. If you have BOTH workbench-use and computer-use slugs, the prompt will tell you; in that case you can choose at call time, but still prefer workbench first.`,
	},
}

// recentChatSeedCorpus is the operational manual for the single-chat
// architecture (one v1:cognition:utterance stream per space) plus the
// assistant/specialist split. Ingested into the recent-chat knowledge
// domain at startup. Each chunk is intentionally short and self-
// contained -- RAG retrieval surfaces the chunk closest to the agent's
// current question, so a single chunk should be readable in isolation.
//
// Authoring rule: use the umbrella tool name ("recentChat") in user-
// facing language. Never expose the operations as standalone tool
// names -- they are arguments to the umbrella tool, not separate
// tools.
var recentChatSeedCorpus = []struct {
	SourceRef string
	Text      string
}{
	{
		SourceRef: "recentChat:single-thread-model",
		Text:      `Every space has ONE chat thread (v1:cognition:utterance), visible to every space participant. Composition: the space owner, every invited human, and the owner's assistant. Specialist agents never write into the chat directly -- they communicate only with the assistant via the askSpecialist tool and return structured JSON results. External guests (token-invited, no account) participate as humans-only with the same visibility as anyone else in the space.`,
	},
	{
		SourceRef: "recentChat:speaker-rules",
		Text:      `Only the assistant speaks to humans. Specialists never publish utterances; their responses flow back to the assistant via the askSpecialist tool result, and the assistant composes the human-facing reply. If a user asks "what does the HR specialist think?" the assistant calls askSpecialist({role: "human-resources", query: ...}), receives a structured JSON object, and synthesizes the reply itself. Never paste a specialist's raw JSON into chat.`,
	},
	{
		SourceRef: "recentChat:canvas-not-chat",
		Text:      `Lifecycle / room-state / system events are CANVAS CARDS, not inline chat utterances. "<X> joined the space" -- canvas. "<X> left" -- canvas. "Mic warning: input is too quiet" -- canvas. The canvas state (v1:copresent:canvasState) carries visibility (public / private), forUserId, actor.kind (system / agent / user), and importance (notify / ambient). The chat thread is for UTTERANCES (what someone said), not for state. As an agent: never emit a chat utterance whose only purpose is to announce a system event. Use canvas.publish or accept that the system itself will land the canvas card.`,
	},
	{
		SourceRef: "recentChat:tool-usage",
		Text:      `The recentChat tool gives you READ-ONLY access to the space chat + space context. Five operations: (1) readRecent({count}) -- last N utterances; (2) readByKeyword({keyword}) -- most recent utterances containing a substring; (3) readByTime({fromTime, toTime}) -- utterances in a time window (ISO-8601); (4) getSpaceContext() -- the space's title, goal, and active participants; (5) listParticipants() -- humans + agents currently active. Each utterance result has speakerId, speakerName, speakerKind, timestamp, content, utteranceId. When you quote prior content, attach a citation with the utteranceId; the frontend renders a click-to-jump chip. NEVER invent the existence of an utterance you didn't actually retrieve.`,
	},
}

// seedStandardDomainsHandler creates the shipped knowledge domains and
// ingests the initial copresent-ui + computer-use corpora. Idempotent:
// skips any domain whose id is already present; re-ingests corpus
// chunks whose content has changed (different text -> different chunk
// id).
func (i *Integration) seedStandardDomainsHandler(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if i.engine == nil {
		return nil, fmt.Errorf("knowledge.seedStandardDomains: engine not configured")
	}

	forceIngest, _ := args["forceIngest"].(bool)

	created := 0
	skipped := 0
	for _, d := range standardDomains {
		if d.RelevantForRoles == nil {
			d.RelevantForRoles = rolesForDomain(d.ID)
		}
		if i.domainExists(ctx, d.ID) {
			skipped++
			continue
		}
		// `source` distinguishes how the domain's chunks get cited at
		// chat time. copresent-ui + computer-use are operator/internal
		// documentation (chunks shape the agent's behavior; not
		// audibly cited). Every other catalog domain is LLM-seeded
		// subject-matter expertise (cited as "your X training" in
		// agent replies). See the citation registry in
		// integrations/agent/replier.go (appStructureDomainIds).
		domainSource := "llmSeeded"
		if d.ID == "copresent-ui" || d.ID == "computer-use" || d.ID == "recent-chat" {
			domainSource = "appStructure"
		}
		// `predefined: true` is the marker the frontend reads to
		// render the row LOCKED in the Knowledge panel -- only the
		// refreshCadenceDays picker stays editable; name /
		// description / category / scope / source are read-only;
		// the row can't be deleted; file upload / drop is disabled
		// (predefined domains carry only LLM-seeded chunks). User-
		// created domains via the modal default the field to false
		// and stay fully editable.
		// lockedForRoles is intentionally NOT populated by this seeder.
		// The v1:agents:agentRole catalog (declared as seeds under
		// dsl/agents/roles/) is the source of truth for which domains
		// each role locks. Enforcement reads from the role row, not
		// from this mirror; a future startup hook may populate the
		// mirror for cheap inverse lookups, but the inversion isn't
		// required for correctness.
		insertQuery := fmt.Sprintf(
			`mutationCreateKnowledgeDomain({domainId: %s, name: %s, description: %s, category: %s, relevantForRoles: %s, requiredByToolSlugs: %s, active: true, tier: %s, source: %s, predefined: true})`,
			quoteString(d.ID),
			quoteString(d.Name),
			quoteString(d.Description),
			quoteString(coalesceCategory(d.Category)),
			jsonArray(d.RelevantForRoles),
			jsonArray(d.RequiredByToolSlugs),
			quoteString(effectiveTier(d)),
			quoteString(domainSource),
		)
		if _, err := i.engine.Execute(ctx, insertQuery); err != nil {
			return nil, fmt.Errorf("knowledge.seedStandardDomains: insert %q: %w", d.ID, err)
		}
		created++
	}

	// Ingest the per-domain seed corpora. Each helper iteration:
	//   - Skips if a chunk with this EXACT content already exists --
	//     the chunk id is a sha256 over domain+sourceRef+seq+text, so
	//     an identical text produces an identical id. Source-edit
	//     detection comes for free: if the corpus text changes, the
	//     new hash won't match anything in the DB and we re-ingest.
	//   - When we DO re-ingest, purges any stale rows for the same
	//     sourceRef first so the new version is the only one live --
	//     otherwise RAG keeps surfacing the old text alongside the new
	//     ("which one is right?"). forceIngest:true bypasses the
	//     skip-if-unchanged check and forces a purge + re-embed.
	ingested := 0
	type seedEntry struct {
		SourceRef string
		Text      string
	}
	ingestCorpus := func(domainId string, entries []seedEntry) {
		for _, entry := range entries {
			expectedId := chunkIdFor(domainId, entry.SourceRef, 0, entry.Text)
			if !forceIngest && i.chunkExistsById(ctx, expectedId) {
				continue
			}
			if err := i.purgeChunksForSource(ctx, domainId, entry.SourceRef); err != nil {
				i.Logger.Warn("knowledge.seedStandardDomains: purge stale chunks failed",
					"domainId", domainId, "sourceRef", entry.SourceRef, "error", err)
				// Continue anyway -- ingestion will still add the new
				// version; stale rows just linger until next purge.
			}
			ingestArgs := map[string]any{
				"domainId":  domainId,
				"text":      entry.Text,
				"source":    "appStructure",
				"sourceRef": entry.SourceRef,
			}
			if _, err := i.ingestHandler(ctx, ingestArgs, 0); err != nil {
				i.Logger.Warn("knowledge.seedStandardDomains: corpus ingest failed",
					"domainId", domainId, "sourceRef", entry.SourceRef, "error", err)
				continue
			}
			ingested++
		}
	}

	copresentEntries := make([]seedEntry, 0, len(copresentUISeedCorpus))
	for _, e := range copresentUISeedCorpus {
		copresentEntries = append(copresentEntries, seedEntry{SourceRef: e.SourceRef, Text: e.Text})
	}
	ingestCorpus("copresent-ui", copresentEntries)

	computerUseEntries := make([]seedEntry, 0, len(computerUseSeedCorpus))
	for _, e := range computerUseSeedCorpus {
		computerUseEntries = append(computerUseEntries, seedEntry{SourceRef: e.SourceRef, Text: e.Text})
	}
	ingestCorpus("computer-use", computerUseEntries)

	workbenchEntries := make([]seedEntry, 0, len(workbenchSeedCorpus))
	for _, e := range workbenchSeedCorpus {
		workbenchEntries = append(workbenchEntries, seedEntry{SourceRef: e.SourceRef, Text: e.Text})
	}
	ingestCorpus("workbench", workbenchEntries)

	recentChatEntries := make([]seedEntry, 0, len(recentChatSeedCorpus))
	for _, e := range recentChatSeedCorpus {
		recentChatEntries = append(recentChatEntries, seedEntry{SourceRef: e.SourceRef, Text: e.Text})
	}
	ingestCorpus("recent-chat", recentChatEntries)

	i.Logger.Info("knowledge.seedStandardDomains: complete",
		"domainsCreated", created,
		"domainsSkipped", skipped,
		"corpusIngested", ingested,
	)

	// Return an empty slice rather than a synthetic-concept result
	// node: the automation step that calls this function doesn't care
	// about the return value, and emitting a fake concept makes the
	// automation engine try to shape it against the registered concept
	// list and fail with "unable to resolve concept".
	return nil, nil
}

// domainExists returns true if a knowledge-domain row with the given
// id is already present. Uses direct SQL rather than going through
// engine.Execute + shape parsing because the seed path runs at every
// startup and needs to be cheap + unambiguous.
func (i *Integration) domainExists(ctx context.Context, domainId string) bool {
	if i.db() == nil {
		return false
	}
	partition := i.resolvePartition(ctx)
	var count int
	sqlText := `
		SELECT COUNT(1) FROM "MemoryNodes"
		WHERE partition = $1
		  AND concept = 'v1:common:knowledgeDomain'
		  AND (payload->>'active' = 'true' OR payload->>'active' IS NULL)
		  AND id LIKE $2
	`
	likePattern := "%:" + domainId
	if err := i.db().QueryRowContext(ctx, sqlText, partition, likePattern).Scan(&count); err != nil {
		return false
	}
	return count > 0
}

// chunkExistsForSource returns true if at least one chunk row exists
// for the given domain+sourceRef. Uses direct SQL because we don't have
// a .memql query for this lookup and it's a ~microsecond check.
func (i *Integration) chunkExistsForSource(ctx context.Context, domainId, sourceRef string) bool {
	if i.db() == nil {
		return false
	}
	partition := i.resolvePartition(ctx)
	var count int
	sqlText := `
		SELECT COUNT(1) FROM "MemoryNodes"
		WHERE partition = $1
		  AND concept = 'v1:common:documentChunk'
		  AND (payload->>'domainId') = $2
		  AND (payload->>'sourceRef') = $3
	`
	if err := i.db().QueryRowContext(ctx, sqlText, partition, domainId, sourceRef).Scan(&count); err != nil {
		return false
	}
	return count > 0
}

// purgeChunksForSource hard-deletes every v1:common:documentChunk
// row (and its node_vectors row) for a given (domain, sourceRef)
// pair in the active partition. Called from the seed right before
// a re-ingest when a text change is detected, so the new version is
// the only live copy for its sourceRef. Direct SQL (not a DSL
// mutation) because seeds run before automations are scheduled and
// we're already doing direct SQL for the sibling lookups.
func (i *Integration) purgeChunksForSource(ctx context.Context, domainId, sourceRef string) error {
	if i.db() == nil {
		return nil
	}
	// Delete embeddings first so we never leave node_vectors rows
	// dangling against a missing MemoryNodes chunk. The subquery
	// picks up every chunk id matching the (domain, sourceRef) pair
	// regardless of version/text hash.
	vecSQL := `
		DELETE FROM node_vectors
		WHERE id IN (
		    SELECT id FROM "MemoryNodes"
		    WHERE concept = 'v1:common:documentChunk'
		      AND (payload->>'domainId') = $1
		      AND (payload->>'sourceRef') = $2
		)
	`
	if _, err := i.db().ExecContext(ctx, vecSQL, domainId, sourceRef); err != nil {
		return fmt.Errorf("delete node_vectors: %w", err)
	}
	chunkSQL := `
		DELETE FROM "MemoryNodes"
		WHERE concept = 'v1:common:documentChunk'
		  AND (payload->>'domainId') = $1
		  AND (payload->>'sourceRef') = $2
	`
	if _, err := i.db().ExecContext(ctx, chunkSQL, domainId, sourceRef); err != nil {
		return fmt.Errorf("delete MemoryNodes: %w", err)
	}
	return nil
}

// chunkExistsById returns true if a chunk row with the given chunk id
// already exists. Used by the seed to skip re-ingesting identical
// content while still allowing source-text edits to flow through: if
// the text in copresentUISeedCorpus changes, chunkIdFor produces a
// different hash that won't match, and the chunk is ingested fresh.
// Chunk ids are stored raw (NOT partition-prefixed) because the
// mutationCreateDocumentChunk mutation uses `id = args.chunkId`
// directly -- see capabilities.go. So we match on exact equality.
func (i *Integration) chunkExistsById(ctx context.Context, chunkId string) bool {
	if i.db() == nil {
		return false
	}
	partition := i.resolvePartition(ctx)
	var count int
	sqlText := `
		SELECT COUNT(1) FROM "MemoryNodes"
		WHERE partition = $1
		  AND concept = 'v1:common:documentChunk'
		  AND id = $2
	`
	if err := i.db().QueryRowContext(ctx, sqlText, partition, chunkId).Scan(&count); err != nil {
		return false
	}
	return count > 0
}

// rolesForDomain inverts roleDomainMap to get the list of role slugs
// that should see a given domain in their picker.
func rolesForDomain(domainId string) []string {
	if domainId == "business-administration" {
		// business-administration is the always-visible catalog
		// baseline -- empty role list means "show in every picker".
		return []string{}
	}
	var roles []string
	for role, list := range roleDomainMap {
		for _, id := range list {
			if id == domainId {
				roles = append(roles, role)
				break
			}
		}
	}
	return roles
}

func hasResult(result any) bool {
	if result == nil {
		return false
	}
	// The engine wraps shape output in an ExecuteResult; we just look
	// for any non-empty collection or non-nil node. JSON-round-trip to
	// normalise then inspect.
	raw, err := json.Marshal(result)
	if err != nil {
		return false
	}
	trimmed := strings.TrimSpace(string(raw))
	// Common empty shapes: "[]", "null", "{}".
	switch trimmed {
	case "", "null", "[]", "{}", "[null]", `[{}]`:
		return false
	}
	// If it's an array, make sure it has at least one populated entry.
	var arr []any
	if err := json.Unmarshal(raw, &arr); err == nil {
		for _, item := range arr {
			if item == nil {
				continue
			}
			if m, ok := item.(map[string]any); ok && len(m) == 0 {
				continue
			}
			return true
		}
		return false
	}
	return true
}

func coalesceCategory(c string) string {
	if c == "" {
		return "business"
	}
	return c
}

func jsonArray(items []string) string {
	if len(items) == 0 {
		return "[]"
	}
	out, _ := json.Marshal(items)
	return string(out)
}
