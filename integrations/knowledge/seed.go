package knowledge

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/core/id"
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
	// Source distinguishes how the domain's chunks get cited at chat
	// time. "" (the default) => llmSeeded: subject-matter expertise cited
	// as "your X training" in agent replies. "appStructure" =>
	// operator/internal documentation whose chunks shape the agent's
	// behavior and are NOT audibly cited (see the citation registry in
	// integrations/agent/replier.go appStructureDomainIds).
	Source string
	// Tier drives the seeder's content strategy per
	// docs/internal/planning/knowledge-seeder.md:
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

// standardDomains is the engine-shipped knowledge-domain catalog. We seed
// it into the database on startup so any client can query
// v1:knowledge:knowledgeDomain instead of carrying the list in-bundle. The
// RelevantForRoles slice drives the role picker: a domain whose
// RelevantForRoles contains role "X" surfaces in role X's picker.
//
// This catalog is engine-generic -- product-specific domains + their seed
// corpora register from a pack via RegisterSeedDomain (see registry.go) and
// are merged in by allSeedDomains(); the engine never hardcodes them here.
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

	// --- Computer Use -----------------------------------------------------
	// Operational manual for the Computer Use capability. Tool requires
	// domain, domain doesn't require tool. Any agent given the Computer
	// Use capability (slug "computer-use") gets this knowledge
	// auto-attached so the generic agentReply template stays agnostic;
	// everything specific to scope tiers, the per-task approval gate, the
	// post-approval dispatch flow, and the planner's outcome-detection
	// semantics lives here as RAG-retrievable chunks. Seeded below from
	// computerUseSeedCorpus.
	//
	// Visibility: every role -- a knowledge specialist (e.g. a
	// research agent) might want to be able to TALK about Computer
	// Use even without holding the capability themselves. Source
	// "appStructure" marks the chunks as operator/internal docs (not
	// audibly cited).
	{ID: "computer-use", Name: "Computer Use", Category: "internal", Source: "appStructure",
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

	// --- Recent Chat ------------------------------------------------------
	// Operational manual for the two-thread chat model (Phase 5 of the
	// chat-architecture plan). Distinct from the operator UI domains
	// (which cover app surfaces) and computer-use (which covers off-app
	// machine driving): recent-chat is the contract for how an agent
	// behaves INSIDE the chat, given that there are now two threads
	// (Group + per-user Team) with hard isolation between them.
	//
	// Auto-attached at agent-prompt-assembly time whenever the agent is
	// dispatching for a non-empty partitionId -- i.e., the agent is acting
	// as a space participant. See replier.go for the auto-injection.
	// 1-on-1 / direct interactions (no partitionId) skip the domain so we
	// don't pay retrieval cost when chat-thread context is irrelevant.
	// Source "appStructure" marks the chunks as operator/internal docs
	// (not audibly cited).
	{ID: "recent-chat", Name: "Recent Chat", Category: "internal", Source: "appStructure",
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
	// CATALOG EXPANSION (per docs/internal/planning/knowledge-seeder.md)
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

// computerUseSeedCorpus is the operational manual for the
// `computer-use` capability. A SeedCorpusEntry list (sourceRef + text)
// ingested through the seedStandardDomains handler. Each
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
var computerUseSeedCorpus = []SeedCorpusEntry{
	{
		SourceRef: "computerUse:overview",
		Text:      `Computer Use is the capability that lets you drive the user's own computer outside the product app. It is distinct from Takeover/Guide: Takeover (autopilot) + Guide (voice Scenes) drive the product SPA you're embedded in (DOM op-ids, takeovers, walkthroughs), while Computer Use drives the user's MACHINE (shell commands, files, mouse, keyboard, screenshot) via a paired cockpit-gui worker process running on their hardware. The capability fans out into four primitive tools when an agent picks it: ` + "`workerHost`" + ` (shell exec / filesystem / HTTP fetch -- headless), ` + "`workerComputer`" + ` (mouse / keyboard / screenshot -- GUI-driving), ` + "`workerStatus`" + ` (cheap connectivity probe), and ` + "`requestComputerUseScope`" + ` (the per-task approval gate). When you mention this skill to the user, call it "Computer Use" (the umbrella name). Never expose the four primitives by name in user-facing text -- they are wire-level internals.`,
	},
	{
		SourceRef: "computerUse:toolSurfaces",
		Text:      `Two execution surfaces sit under Computer Use, each with a distinct shape: ` + "`workerHost`" + ` is HEADLESS -- it runs shell commands, reads/writes files, fetches URLs. Use it when the task has a one-shot command equivalent (filesystem operations, package installs, HTTP fetches, anything you'd type at a terminal). It's faster, more reliable, and easier to verify than scripted GUI input. ` + "`workerComputer`" + ` is GUI-DRIVING -- it moves the mouse, types on the keyboard, scrolls, takes screenshots. Use it when the task genuinely requires looking at or driving the user's screen (interacting with a native app that has no CLI, taking a screenshot of the desktop, clicking through a UI flow inside an app outside the product). When the SAME task is achievable on either surface, prefer ` + "`workerHost`" + ` for the smaller blast radius and the cleaner contract -- unless the user explicitly asked for the cursor / keyboard path. ` + "`workerStatus`" + ` is a cheap probe with no side effects; call it when the cockpit's connectivity may have aged (the user told you mid-turn they started their cockpit; you're about to kick off a long workerHost / workerComputer flow). Don't spam it -- the per-turn computerUseStatus prompt seed is already fresh at turn start.`,
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
var workbenchSeedCorpus = []SeedCorpusEntry{
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
		Text:      `When the workbench can't do the job (the task genuinely requires macOS / Xcode / a GUI app on the user's machine / a file the user has locally), DON'T silently switch to computer-use, and DON'T just dead-end with "I can't." Close the loop through the user-gated escalation path: (1) If you HAVE a computer-use slug (workerHost / workerComputer), call ` + "`requestComputerUseScope({intent, requestedScope, summary})`" + ` -- naming the workbench limitation in the summary -- then end your turn with a short ` + "`respondToUser`" + ` ("I can't build the iOS app from the workbench -- that needs Xcode on your Mac. I've put an approval card on your canvas; click Allow and I'll run it via Computer Use."). The user's Allow click dispatches a fresh turn back to you with planApprovedTrigger=true where you run the work on their machine -- that IS the fallback, and it requires the consent card, not a planner guess. (2) If you do NOT have a computer-use slug, end with a ` + "`respondToUser`" + ` that names the limitation concretely AND tells the user computer-use would unblock it ("...that needs Xcode on a Mac; if you enable Computer Use for me I can drive your machine to do it."), so the user can grant the capability. Same pattern for ambiguous failures: an ` + "`exec`" + ` that exits with a missing-binary error is a "workbench can't do this" signal -- surface it honestly (and escalate via requestComputerUseScope if you can) rather than retrying with random variations. If you have BOTH workbench-use and computer-use slugs, still prefer workbench first; reach for the computer-use escalation only when the workbench genuinely cannot do the job.`,
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
var recentChatSeedCorpus = []SeedCorpusEntry{
	{
		SourceRef: "recentChat:single-thread-model",
		Text:      `Every space has ONE chat thread (v1:cognition:utterance), visible to every space participant. Composition: the space owner, every invited human, and the owner's assistant. Specialist agents never write into the chat directly -- they communicate only with the assistant via the askSpecialist tool and return structured JSON results. External guests (token-invited, no account) participate as humans-only with the same visibility as anyone else in the space.`,
	},
	{
		SourceRef: "recentChat:speaker-rules",
		Text:      `Only the assistant speaks to humans. Specialists never publish utterances; their responses flow back to the assistant via the askSpecialist tool result, and the assistant composes the human-facing reply. If a user asks "what does the HR specialist think?" the assistant calls askSpecialist({role: "human-resources", query: ...}), receives a structured JSON object, and synthesizes the reply itself. Never paste a specialist's raw JSON into chat.`,
	},
	{
		SourceRef: "recentChat:tool-usage",
		Text:      `The recentChat tool gives you READ-ONLY access to the space chat + space context. Five operations: (1) readRecent({count}) -- last N utterances; (2) readByKeyword({keyword}) -- most recent utterances containing a substring; (3) readByTime({fromTime, toTime}) -- utterances in a time window (ISO-8601); (4) getSpaceContext() -- the space's title, goal, and active participants; (5) listParticipants() -- humans + agents currently active. Each utterance result has speakerId, speakerName, speakerKind, timestamp, content, utteranceId. When you quote prior content, attach a citation with the utteranceId; the frontend renders a click-to-jump chip. NEVER invent the existence of an utterance you didn't actually retrieve.`,
	},
}

// retiredSeedCorpusPairs lists (domainId, sourceRef) corpus pairs that were
// REMOVED from the seed catalog (entry relocated to another domain or
// deleted outright). seedStandardDomainsHandler purges them on every run so
// already-deployed databases don't keep serving the stale chunk forever;
// keep a pair here until every environment has run the purge at least once.
var retiredSeedCorpusPairs = []struct {
	DomainID  string
	SourceRef string
}{
	// The canvas-vs-chat guidance entry moved out of recent-chat into a
	// product pack's UI corpus (new SourceRef, new domain -> new chunk id).
	{DomainID: "recent-chat", SourceRef: "recentChat:canvas-not-chat"},
}

// seedStandardDomainsHandler creates the shipped knowledge domains
// (engine catalog + any pack-registered seed domains) and ingests their
// seed corpora. Idempotent: skips any domain whose id is already present;
// re-ingests corpus chunks whose content has changed (different text ->
// different chunk id).
func (i *Integration) seedStandardDomainsHandler(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if i.engine == nil {
		return nil, fmt.Errorf("knowledge.seedStandardDomains: engine not configured")
	}

	forceIngest, _ := args["forceIngest"].(bool)

	created := 0
	skipped := 0
	for _, d := range allSeedDomains() {
		if d.RelevantForRoles == nil {
			d.RelevantForRoles = rolesForDomain(d.ID)
		}
		if i.domainExists(ctx, d.ID) {
			skipped++
			continue
		}
		// `source` distinguishes how the domain's chunks get cited at
		// chat time. StandardDomain.Source carries this per-entry:
		// "appStructure" domains are operator/internal documentation
		// (chunks shape the agent's behavior; not audibly cited), while
		// "" defaults to "llmSeeded" -- subject-matter expertise cited as
		// "your X training" in agent replies. See the citation registry
		// in integrations/agent/replier.go (appStructureDomainIds).
		domainSource := d.Source
		if domainSource == "" {
			domainSource = "llmSeeded"
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
			`mutation createKnowledgeDomain(domainId: %s, name: %s, description: %s, category: %s, relevantForRoles: %s, requiredByToolSlugs: %s, active: true, tier: %s, source: %s, predefined: true)`,
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
	ingestCorpus := func(domainId string, entries []SeedCorpusEntry) {
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

	// Engine-shipped corpora, ingested directly.
	ingestCorpus("computer-use", computerUseSeedCorpus)
	ingestCorpus("workbench", workbenchSeedCorpus)
	ingestCorpus("recent-chat", recentChatSeedCorpus)

	// Pack-registered corpora (e.g. the product pack's UI domain). The
	// engine carries none of these; they arrive via RegisterSeedDomain.
	for _, reg := range RegisteredSeedDomains() {
		ingestCorpus(reg.Domain.ID, reg.Corpus)
	}

	// One-time cleanup of retired (domainId, sourceRef) corpus pairs.
	// ingestCorpus's purge path only fires for pairs present in a CURRENT
	// corpus, so an entry that leaves the catalog (moved to another
	// domain, or deleted) would otherwise leave its chunk + embedding
	// orphaned in deployed databases forever -- still retrievable as a
	// stale duplicate. Idempotent: the DELETE is a no-op once the pair is
	// gone.
	for _, pair := range retiredSeedCorpusPairs {
		if err := i.purgeChunksForSource(ctx, pair.DomainID, pair.SourceRef); err != nil {
			i.Logger.Warn("knowledge.seedStandardDomains: retired-pair purge failed",
				"domain", pair.DomainID, "sourceRef", pair.SourceRef, "error", err)
		}
	}

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
	var count int
	sqlText := `
		SELECT COUNT(1) FROM "MemoryNodes"
		WHERE concept = 'v1:knowledge:knowledgeDomain'
		  AND (payload->>'active' = 'true' OR payload->>'active' IS NULL)
		  AND id = $1
	`
	canonicalId := id.BuildNodeId("v1:knowledge:knowledgeDomain", domainId)
	if err := i.db().QueryRowContext(ctx, sqlText, canonicalId).Scan(&count); err != nil {
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
	var count int
	sqlText := `
		SELECT COUNT(1) FROM "MemoryNodes"
		WHERE concept = 'v1:knowledge:documentChunk'
		  AND (payload->>'domainId') = $1
		  AND (payload->>'sourceRef') = $2
	`
	if err := i.db().QueryRowContext(ctx, sqlText, domainId, sourceRef).Scan(&count); err != nil {
		return false
	}
	return count > 0
}

// purgeChunksForSource hard-deletes every v1:knowledge:documentChunk
// row (and its node_vectors row) for a given (domain, sourceRef)
// pair. Called from the seed right before
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
		    WHERE concept = 'v1:knowledge:documentChunk'
		      AND (payload->>'domainId') = $1
		      AND (payload->>'sourceRef') = $2
		)
	`
	if _, err := i.db().ExecContext(ctx, vecSQL, domainId, sourceRef); err != nil {
		return fmt.Errorf("delete node_vectors: %w", err)
	}
	chunkSQL := `
		DELETE FROM "MemoryNodes"
		WHERE concept = 'v1:knowledge:documentChunk'
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
// a seed corpus text changes, chunkIdFor produces a
// different hash that won't match, and the chunk is ingested fresh.
// chunkIdFor returns the BARE hash; the stored row id is the
// concept-qualified form the engine composes at insert
// (v1:knowledge:documentChunk:<hash>), so match on that.
func (i *Integration) chunkExistsById(ctx context.Context, chunkId string) bool {
	if i.db() == nil {
		return false
	}
	var count int
	sqlText := `
		SELECT COUNT(1) FROM "MemoryNodes"
		WHERE concept = 'v1:knowledge:documentChunk'
		  AND id = $1
	`
	canonicalId := id.BuildNodeId("v1:knowledge:documentChunk", chunkId)
	if err := i.db().QueryRowContext(ctx, sqlText, canonicalId).Scan(&count); err != nil {
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
