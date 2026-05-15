package knowledge

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	memorynodes "github.com/visionarys-io/memql/component/database/memory-nodes"
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
	// quantum_mechanics) keep the 30 default.
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
	"med_surgery_general":   {"General surgery", "Surgery", "Surgical specialty"},
	"med_surgery_orthopedic": {"Orthopedic surgery", "Joint replacement", "Fracture"},
	"med_surgery_cardiac":    {"Cardiac surgery", "Coronary artery bypass surgery", "Heart valve repair and replacement"},
	"med_surgery_neuro":      {"Neurosurgery", "Craniotomy", "Spinal surgery"},

	// Clinical specialties -- overview + a couple anchors.
	"med_internal_medicine": {"Internal medicine", "Diagnosis", "Clinical reasoning"},
	"med_cardiology":        {"Cardiology", "Cardiovascular disease", "Echocardiography"},
	"med_neurology":         {"Neurology", "Neurological examination", "Stroke"},
	"med_pediatrics":        {"Pediatrics", "Child development", "Vaccination schedule"},
	"med_geriatrics":        {"Geriatrics", "Frailty syndrome", "Polypharmacy"},
	"med_psychiatry":        {"Psychiatry", "Diagnostic and Statistical Manual of Mental Disorders", "Cognitive behavioral therapy"},
	"med_dentistry":         {"Dentistry", "Oral hygiene", "Periodontology"},
	"med_ophthalmology":     {"Ophthalmology", "Cataract surgery", "Glaucoma"},
	"med_dermatology":       {"Dermatology", "Skin cancer", "Eczema"},
	"med_radiology":         {"Radiology", "Medical imaging", "Computed tomography"},
	"med_anesthesiology":    {"Anesthesiology", "General anaesthesia", "Pain management"},
	"med_veterinary":        {"Veterinary medicine", "Veterinary surgery", "Animal welfare"},
	"med_nursing":           {"Nursing", "Nursing process", "Nursing diagnosis"},
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
// copresent_ui (bottom of this list) is new -- it's the app-knowledge
// domain required by the copresent_control tool so agents given that
// tool automatically pick up CoPresent UI knowledge for walkthroughs.
var standardDomains = []StandardDomain{
	// --- Core --------------------------------------------------------------
	// business_administration was previously called general_business AND
	// auto-attached + locked to every agent in the picker. Now it's a
	// regular trainable catalog domain that any agent can opt into; the
	// only special case is the General Assistant, which still gets it
	// auto-attached + locked-on by the provisionGeneralAssistant
	// automation. Specialists pick it up from the picker like any other
	// domain.
	{ID: "business_administration", Name: "Business Administration", Category: "core",
		Description: "Core business literacy: org structure, workflows, everyday terminology, basic financial concepts.",
		RelevantForRoles: []string{"general_assistant", "accounting_finance", "human_resources", "customer_service", "quality_assurance", "sales_marketing", "it_support", "legal_compliance", "operations", "project_management", "research_development", "training_education"},
	},

	// --- Original pre-existing domains ------------------------------------
	{ID: "inventory_supply_chain", Name: "Inventory & Supply Chain", Description: "Stock levels, supplier management, procurement, logistics."},
	{ID: "financial_data", Name: "Financial Data", Description: "Financial statements, ledgers, transactions, accounts."},
	{ID: "employee_records", Name: "Employee Records", Description: "HR files, roles, compensation, organisational directory."},
	{ID: "customer_relations", Name: "Customer Relations", Description: "Customer accounts, contact history, engagement records."},
	{ID: "product_catalog", Name: "Product Catalog", Description: "Product SKUs, specifications, lifecycle, pricing tiers."},
	{ID: "quality_metrics", Name: "Quality Metrics", Description: "Quality KPIs, defect tracking, inspection data, compliance audits."},
	{ID: "legal_documents", Name: "Legal Documents", Description: "Contracts, policies, agreements, legal correspondence."},
	{ID: "project_data", Name: "Project Data", Description: "Project plans, milestones, deliverables, resourcing."},
	{ID: "technical_documentation", Name: "Technical Documentation", Category: "technical", Description: "System architecture, APIs, runbooks, engineering references."},

	// --- Accounting & Finance ---------------------------------------------
	{ID: "accounting_principles", Name: "Accounting Principles", Description: "GAAP/IFRS fundamentals, journal entries, closing processes."},
	{ID: "tax_regulations", Name: "Tax Regulations", Description: "Federal, state, and international tax codes and filings."},
	{ID: "budgeting_forecasting", Name: "Budgeting & Forecasting", Description: "Budget cycles, variance analysis, rolling forecasts."},
	{ID: "payroll_benefits", Name: "Payroll & Benefits", Description: "Payroll processing, benefits administration, compensation rules."},

	// --- Human Resources --------------------------------------------------
	{ID: "talent_acquisition", Name: "Talent Acquisition", Description: "Recruiting pipelines, interviewing, sourcing, onboarding."},
	{ID: "labor_law", Name: "Labor Law", Description: "Employment regulations, workplace compliance, labor relations."},
	{ID: "training_development", Name: "Training & Development", Description: "Training programs, skill development, continuing education."},
	{ID: "organizational_design", Name: "Organizational Design", Description: "Org structure, team topology, reporting lines, role design."},

	// --- Sales & Marketing ------------------------------------------------
	{ID: "sales_pipeline", Name: "Sales Pipeline", Description: "Leads, opportunities, pipeline stages, forecasting."},
	{ID: "marketing_analytics", Name: "Marketing Analytics", Description: "Campaign metrics, attribution, conversion analysis."},
	{ID: "brand_strategy", Name: "Brand Strategy", Description: "Brand positioning, messaging, identity, tone guidelines."},
	{ID: "lead_generation", Name: "Lead Generation", Description: "Prospecting, outbound strategies, top-of-funnel tactics."},

	// --- Customer Service -------------------------------------------------
	{ID: "service_level_agreements", Name: "Service Level Agreements", Description: "SLA definitions, response-time commitments, escalation policies."},
	{ID: "ticket_management", Name: "Ticket Management", Description: "Ticketing workflows, triage, resolution SLAs."},

	// --- IT ---------------------------------------------------------------
	{ID: "network_infrastructure", Name: "Network Infrastructure", Category: "technical", Description: "Network topology, firewalls, routing, VPN configuration."},
	{ID: "cybersecurity", Name: "Cybersecurity", Category: "technical", Description: "Threat models, access control, incident response, encryption."},
	{ID: "software_development", Name: "Software Development", Category: "technical", Description: "Engineering practices, languages, SDLC, version control."},
	{ID: "cloud_platforms", Name: "Cloud Platforms", Category: "technical", Description: "AWS, GCP, Azure services, deployment, cost optimisation."},

	// --- Legal ------------------------------------------------------------
	{ID: "contracts_agreements", Name: "Contracts & Agreements", Description: "Contract drafting, review, negotiation, standard clauses."},
	{ID: "regulatory_compliance", Name: "Regulatory Compliance", Description: "Industry regulations, compliance frameworks, audit readiness."},
	{ID: "intellectual_property", Name: "Intellectual Property", Description: "Patents, trademarks, copyrights, trade secrets, licensing."},

	// --- Operations -------------------------------------------------------
	{ID: "process_optimization", Name: "Process Optimization", Description: "Lean, Six Sigma, workflow efficiency, bottleneck analysis."},
	{ID: "logistics_distribution", Name: "Logistics & Distribution", Description: "Warehousing, shipping, fleet management, last-mile delivery."},
	{ID: "vendor_management", Name: "Vendor Management", Description: "Vendor selection, contracts, performance, relationships."},

	// --- Research & Development -------------------------------------------
	{ID: "research_methodology", Name: "Research Methodology", Description: "Experimental design, statistical analysis, peer review."},
	{ID: "data_analysis", Name: "Data Analysis", Description: "Quantitative analysis, dashboards, A/B testing, statistics."},
	{ID: "innovation_management", Name: "Innovation Management", Description: "Idea pipelines, R&D investment, innovation portfolios."},

	// --- Training & Education ---------------------------------------------
	{ID: "curriculum_design", Name: "Curriculum Design", Description: "Learning objectives, course structure, instructional design."},
	{ID: "performance_assessment", Name: "Performance Assessment", Description: "Evaluation methods, rubrics, performance metrics."},

	// --- Executive / Strategic --------------------------------------------
	{ID: "strategic_planning", Name: "Strategic Planning", Description: "Long-horizon planning, OKRs, scenario modelling."},
	{ID: "risk_management", Name: "Risk Management", Description: "Risk registers, mitigation planning, BCP / DR."},
	{ID: "stakeholder_communications", Name: "Stakeholder Communications", Description: "Executive reporting, board updates, investor relations."},

	// --- CoPresent UI (NEW) -----------------------------------------------
	// Visible for every role so any agent (GA or specialist) can opt in to
	// app-knowledge. Auto-attached to any agent whose tool list includes
	// the copresent_control bundle (see RequiredByToolSlugs) so picking
	// the tool implies the domain. Document chunks for this domain are
	// seeded below from copresentUISeedCorpus.
	{ID: "copresent_ui", Name: "CoPresent UI", Category: "internal",
		Description: "Knowledge of the CoPresent application layout, panels, modals, and interactive op-id targets. Auto-attached to any agent given the CoPresent Control tool so walkthroughs and explanations are anchored to the real UI rather than guessed from training data.",
		RelevantForRoles: []string{"general_assistant", "accounting_finance", "human_resources", "customer_service", "quality_assurance", "sales_marketing", "it_support", "legal_compliance", "operations", "project_management", "research_development", "training_education", "personal_finance_advisor", "household_manager", "parenting_coach", "health_wellness_coach", "meal_planning_chef", "travel_planner", "creative_companion", "learning_companion", "relationships_social", "pet_care_specialist", "home_improvement_diy", "personal_legal_advisor", "mindfulness_coach", "entertainment_curator", "senior_care_advisor", "real_estate_advisor"},
		RequiredByToolSlugs: []string{"copresent_control"},
	},

	// --- Computer Use -----------------------------------------------------
	// Operational manual for the Computer Use capability. Mirrors the
	// copresent_ui pattern: tool requires domain, domain doesn't
	// require tool. Any agent given the Computer Use capability (slug
	// "computer_use") gets this knowledge auto-attached so the
	// generic agentReply template stays agnostic; everything specific
	// to scope tiers, the per-task approval gate, the post-approval
	// dispatch flow, and the planner's outcome-detection semantics
	// lives here as RAG-retrievable chunks. Seeded below from
	// computerUseSeedCorpus.
	//
	// Visibility: every role -- a knowledge specialist (e.g. a
	// research agent) might want to be able to TALK about Computer
	// Use even without holding the capability themselves, just like
	// copresent_ui is attachable without copresent_control.
	{ID: "computer_use", Name: "Computer Use", Category: "internal",
		Description: "Operational manual for the Computer Use capability: tool surfaces (workerHost / workerComputer), scope tiers (observe / full), per-task approval flow (requestComputerUseScope -> canvas card -> Allow / Deny), post-approval execution semantics, and the planner's success-vs-failure detection. Auto-attached to any agent given the Computer Use capability so the generic prompt template stays agnostic.",
		RelevantForRoles:    []string{"general_assistant", "accounting_finance", "human_resources", "customer_service", "quality_assurance", "sales_marketing", "it_support", "legal_compliance", "operations", "project_management", "research_development", "training_education", "personal_finance_advisor", "household_manager", "parenting_coach", "health_wellness_coach", "meal_planning_chef", "travel_planner", "creative_companion", "learning_companion", "relationships_social", "pet_care_specialist", "home_improvement_diy", "personal_legal_advisor", "mindfulness_coach", "entertainment_curator", "senior_care_advisor", "real_estate_advisor"},
		RequiredByToolSlugs: []string{"computer_use"},
	},

	// --- CoPresent Conversation -------------------------------------------
	// Operational manual for the two-thread chat model (Phase 5 of the
	// chat-architecture plan). Distinct from copresent_ui (which covers
	// app surfaces) and computer_use (which covers off-app machine
	// driving): copresent_conversation is the contract for how an agent
	// behaves INSIDE the chat, given that there are now two threads
	// (Group + per-user Team) with hard isolation between them.
	//
	// Auto-attached at agent-prompt-assembly time whenever the agent is
	// dispatching for a non-empty spaceId -- i.e., the agent is acting
	// as a space participant. See replier.go for the auto-injection.
	// 1-on-1 / direct interactions (no spaceId) skip the domain so we
	// don't pay retrieval cost when chat-thread context is irrelevant.
	{ID: "copresent_conversation", Name: "CoPresent Conversation", Category: "internal",
		Description: "Operational manual for the two-thread chat model in CoPresent: Group + per-user Team threads, hard visibility isolation, voice migration on second-active-human, discussion-mode behavior in private, misroute safety net, canvas-not-chat for system events, and the copresentConversation tool for read-only group access. Auto-attached to any agent participating in a space.",
		RelevantForRoles:    []string{"general_assistant", "accounting_finance", "human_resources", "customer_service", "quality_assurance", "sales_marketing", "it_support", "legal_compliance", "operations", "project_management", "research_development", "training_education", "personal_finance_advisor", "household_manager", "parenting_coach", "health_wellness_coach", "meal_planning_chef", "travel_planner", "creative_companion", "learning_companion", "relationships_social", "pet_care_specialist", "home_improvement_diy", "personal_legal_advisor", "mindfulness_coach", "entertainment_curator", "senior_care_advisor", "real_estate_advisor"},
		RequiredByToolSlugs: []string{"copresent_conversation"},
	},

	// --- Personal Finance -------------------------------------------------
	{ID: "personal_finance", Name: "Personal Finance", Category: "product",
		Description: "Personal budgeting, expense tracking, net-worth, financial goal setting, debt management, savings strategies."},
	{ID: "personal_taxes", Name: "Personal Taxes", Category: "product",
		Description: "Personal income tax: filing, deductions, credits, withholdings, IRS procedures, state-tax variations, estimated payments."},
	{ID: "personal_investing", Name: "Personal Investing", Category: "product",
		Description: "Brokerage accounts, retirement accounts (IRA / 401(k) / Roth), index funds, asset allocation, rebalancing, tax-loss harvesting."},
	{ID: "personal_insurance", Name: "Personal Insurance", Category: "product",
		Description: "Health, auto, home/renters, life, umbrella, disability insurance: shopping, claims, coverage analysis."},
	{ID: "personal_budgeting", Name: "Personal Budgeting", Category: "product",
		Description: "Monthly budget templates, envelope methods, zero-based budgeting, irregular-income budgeting, cash-flow planning."},

	// --- Household Management ---------------------------------------------
	{ID: "household_maintenance", Name: "Household Maintenance", Category: "product",
		Description: "Routine maintenance schedules: HVAC filter changes, gutter cleaning, water heater flush, smoke detector batteries, appliance servicing."},
	{ID: "home_inventory", Name: "Home Inventory", Category: "product",
		Description: "Tracking owned items for insurance / warranty: serial numbers, purchase dates, receipts, photos, replacement values."},
	{ID: "household_chores", Name: "Household Chores & Routines", Category: "product",
		Description: "Cleaning routines, family chore charts, weekly / monthly household tasks, supply inventory."},

	// --- Parenting --------------------------------------------------------
	{ID: "parenting", Name: "Parenting", Category: "product",
		Description: "Daily parenting routines, discipline approaches, age-appropriate guidance, family activities, parent-child communication."},
	{ID: "child_development", Name: "Child Development", Category: "product",
		Description: "Developmental milestones (motor, language, social-emotional, cognitive) by age range; warning signs; resources."},
	{ID: "school_education_personal", Name: "School & Education (Personal)", Category: "product",
		Description: "K-12 school logistics: calendars, parent-teacher conferences, IEP/504, homework support, college prep."},

	// --- Health & Wellness ------------------------------------------------
	{ID: "nutrition", Name: "Nutrition", Category: "product",
		Description: "Macronutrients, micronutrients, dietary patterns (Mediterranean, plant-based, low-carb), reading nutrition labels, recipe substitutions."},
	{ID: "fitness", Name: "Fitness", Category: "product",
		Description: "Workout programming (strength, cardio, mobility), exercise form, training periodization, injury prevention, home-gym basics."},
	{ID: "mental_health", Name: "Mental Health", Category: "product",
		Description: "Stress management, anxiety / depression awareness, therapy modalities, when to seek professional help, mental-health first aid."},
	{ID: "sleep_hygiene", Name: "Sleep Hygiene", Category: "product",
		Description: "Sleep cycles, light exposure, bedtime routines, common sleep disorders, evidence-based interventions for sleep quality."},
	{ID: "medical_records_personal", Name: "Personal Medical Records", Category: "product",
		Description: "Personal medication list, allergy log, vaccination history, prior procedures, family-history relevant conditions."},

	// --- Meal Planning & Recipes ------------------------------------------
	{ID: "recipes", Name: "Recipes", Category: "product",
		Description: "Personal recipe collection: ingredients, instructions, prep / cook time, servings, dietary tags, favorites."},
	{ID: "meal_planning", Name: "Meal Planning", Category: "product",
		Description: "Weekly meal planning, batch cooking, freezer meals, leftover strategy, themed weeks (taco Tuesday, etc.)."},
	{ID: "dietary_restrictions", Name: "Dietary Restrictions", Category: "product",
		Description: "Allergens, food intolerances, religious / ethical restrictions, medical diets (low-sodium, diabetic, kidney-friendly)."},

	// --- Travel -----------------------------------------------------------
	{ID: "travel_planning", Name: "Travel Planning", Category: "product",
		Description: "Trip itineraries, flights, hotels, activities, packing lists, travel insurance, budget per destination."},
	{ID: "travel_documents", Name: "Travel Documents", Category: "product",
		Description: "Passports, visas, vaccination requirements, TSA Pre-Check / Global Entry, international driver's permit, travel insurance docs."},
	{ID: "restaurant_dining", Name: "Restaurant & Dining", Category: "product",
		Description: "Restaurant favorites, dietary preferences, reservation history, regional specialties, dining budgets."},

	// --- Creative ---------------------------------------------------------
	{ID: "creative_arts", Name: "Creative Arts", Category: "product",
		Description: "Visual arts (drawing, painting), music, writing, crafts; technique references, materials, project history."},
	{ID: "music_appreciation", Name: "Music Appreciation", Category: "product",
		Description: "Personal music library, artists, genres, concert history, playlists, learning instruments."},
	{ID: "photography", Name: "Photography", Category: "product",
		Description: "Camera settings, composition principles, post-processing, photo organization, gear inventory."},

	// --- Learning ---------------------------------------------------------
	{ID: "language_learning", Name: "Language Learning", Category: "product",
		Description: "Vocabulary, grammar references, learning resources, conversation practice, immersion strategies for spoken languages."},
	{ID: "online_courses", Name: "Online Courses", Category: "product",
		Description: "MOOC enrollments (Coursera, edX, Udemy), course progress, certificates, learning notes, study schedule."},
	{ID: "book_summaries", Name: "Books & Reading", Category: "product",
		Description: "Personal reading list, book summaries, annotations, recommendations, reading goals."},

	// --- Relationships & Social -------------------------------------------
	{ID: "relationships_communication", Name: "Relationships & Communication", Category: "product",
		Description: "Communication frameworks (NVC, active listening), conflict resolution, healthy relationship patterns, attachment styles."},
	{ID: "life_events_celebrations", Name: "Life Events & Celebrations", Category: "product",
		Description: "Birthdays, anniversaries, holidays, gift histories, party planning, traditions."},
	{ID: "gifts", Name: "Gifts & Recommendations", Category: "product",
		Description: "Gift ideas tailored per person (preferences, history of gifts given/received), occasions, budgets."},

	// --- Pet Care ---------------------------------------------------------
	{ID: "pet_care", Name: "Pet Care", Category: "product",
		Description: "Vet schedules, medication, food, grooming, daily care routines per species / breed."},
	{ID: "pet_training", Name: "Pet Training", Category: "product",
		Description: "Positive-reinforcement training, behavior modification, basic commands, common problem behaviors."},
	{ID: "pet_health", Name: "Pet Health", Category: "product",
		Description: "Common health conditions per species / breed, emergency signs, preventive care, vaccinations."},

	// --- DIY & Home Improvement -------------------------------------------
	{ID: "diy_repairs", Name: "DIY & Repairs", Category: "product",
		Description: "Common home repairs (plumbing, electrical, drywall, painting), tool basics, when to DIY vs hire, safety."},
	{ID: "gardening", Name: "Gardening", Category: "product",
		Description: "Plant care by zone, watering schedules, pest management, seasonal planting, vegetable gardening."},
	{ID: "sustainability", Name: "Sustainability & Eco-Living", Category: "product",
		Description: "Energy efficiency, recycling, composting, sustainable shopping, low-waste living."},

	// --- Personal Legal ---------------------------------------------------
	{ID: "personal_legal", Name: "Personal Legal Matters", Category: "product",
		Description: "Tenant rights, consumer protection, small claims, traffic, employment law for individuals (NOT a substitute for an attorney)."},
	{ID: "estate_planning_personal", Name: "Estate Planning (Personal)", Category: "product",
		Description: "Wills, living trusts, healthcare directives, power of attorney, beneficiary designations."},
	{ID: "contracts_personal", Name: "Personal Contracts", Category: "product",
		Description: "Leases, employment offers, freelance agreements, NDAs, service contracts -- review checklists for non-lawyers."},

	// --- Mindfulness & Personal Growth ------------------------------------
	{ID: "mindfulness", Name: "Mindfulness & Meditation", Category: "product",
		Description: "Meditation techniques (focused attention, open monitoring, loving-kindness), breathwork, common challenges."},
	{ID: "journaling", Name: "Journaling", Category: "product",
		Description: "Journaling prompts, gratitude practices, morning pages, reflective writing, journal organization."},
	{ID: "personal_growth", Name: "Personal Growth", Category: "product",
		Description: "Goal-setting frameworks (SMART, OKR-personal), habit formation, accountability systems, self-reflection prompts."},

	// --- Entertainment ----------------------------------------------------
	{ID: "entertainment_media", Name: "Entertainment & Media", Category: "product",
		Description: "Movies, TV shows, podcasts, video games -- watched / unwatched lists, ratings, recommendations from sources."},

	// --- Senior Care ------------------------------------------------------
	{ID: "elder_care", Name: "Elder Care", Category: "product",
		Description: "Aging-in-place, in-home care services, assisted living, Medicare basics, caregiver burnout, family communication."},
	{ID: "end_of_life_planning", Name: "End-of-Life Planning", Category: "product",
		Description: "Hospice / palliative care, advance directives, funeral planning, legacy projects, grief support."},

	// --- Real Estate ------------------------------------------------------
	// Personal-tier real-estate domains. Cover the buy / sell / rent
	// research surface for an individual transacting one residential
	// property at a time. Commercial real estate / property management
	// would warrant a separate set of business-category domains if we
	// ever need them; these intentionally stay personal.
	{ID: "real_estate_listings", Name: "Real Estate Listings", Category: "product",
		Description: "Active for-sale + for-rent listings, MLS search, comparable sales (comps), price-history tracking, saved searches, listing alerts."},
	{ID: "mortgage_shopping", Name: "Mortgage Shopping", Category: "product",
		Description: "Loan types (conventional, FHA, VA, jumbo), interest rates, points, pre-approval, lender comparison, down-payment strategies, PMI, refinancing."},
	{ID: "neighborhood_research", Name: "Neighborhood Research", Category: "product",
		Description: "School-district ratings, crime stats, walkability + transit scores, commute times, amenities, future development, HOA / zoning notes."},
	{ID: "home_inspection", Name: "Home Inspection", Category: "product",
		Description: "Pre-purchase + pre-sale inspection checklists, common findings (roof, foundation, electrical, plumbing, HVAC, pests, radon), inspector-shopping, repair-estimate negotiation."},
	{ID: "property_taxes", Name: "Property Taxes", Category: "product",
		Description: "County assessment cycles, tax rates by jurisdiction, homestead / senior / veteran exemptions, assessment appeals, escrow vs direct payment."},
	{ID: "lease_agreements", Name: "Lease Agreements", Category: "product",
		Description: "Residential lease review, tenant + landlord rights, security deposits, rent escalation, renewal + termination clauses, common red flags."},
	{ID: "closing_process", Name: "Closing Process", Category: "product",
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
	// among them (personal_finance, personal_taxes, mental_health, etc.)
	// get explicit Tier "B" stamped via the tierOverride map below
	// rather than touching every existing line.
	// =========================================================================

	// --- Physics --------------------------------------------------------------
	{ID: "physics_classical_mechanics", Name: "Classical Mechanics", Category: "science", Tier: "A",
		Description: "Newtonian mechanics, kinematics, dynamics, conservation laws, Lagrangian + Hamiltonian formulations, rotational dynamics, oscillations."},
	{ID: "physics_thermodynamics", Name: "Thermodynamics", Category: "science", Tier: "A",
		Description: "Laws of thermodynamics, heat engines, entropy, statistical mechanics fundamentals, phase transitions."},
	{ID: "physics_electromagnetism", Name: "Electromagnetism", Category: "science", Tier: "A",
		Description: "Maxwell's equations, electric + magnetic fields, electromagnetic waves, circuits, optics."},
	{ID: "physics_quantum_mechanics", Name: "Quantum Mechanics", Category: "science", Tier: "A",
		Description: "Wave functions, Schrödinger equation, operators, uncertainty principle, entanglement, superposition, quantum measurement."},
	{ID: "physics_relativity", Name: "Relativity", Category: "science", Tier: "A",
		Description: "Special + general relativity, spacetime, Lorentz transformations, gravity as curvature, black holes, gravitational waves."},
	{ID: "physics_particle", Name: "Particle Physics", Category: "science", Tier: "A",
		Description: "Standard Model, quarks + leptons, gauge bosons, Higgs mechanism, particle accelerators, fundamental forces."},
	{ID: "physics_astrophysics", Name: "Astrophysics", Category: "science", Tier: "A",
		Description: "Stellar structure + evolution, nucleosynthesis, galactic dynamics, supernovae, neutron stars, exoplanets, observational techniques."},
	{ID: "physics_cosmology", Name: "Cosmology", Category: "science", Tier: "A",
		Description: "Big Bang model, cosmic microwave background, inflation, dark matter + dark energy, large-scale structure, expansion of the universe."},

	// --- Chemistry ------------------------------------------------------------
	{ID: "chemistry_organic", Name: "Organic Chemistry", Category: "science", Tier: "A",
		Description: "Carbon-based molecules, functional groups, reaction mechanisms, stereochemistry, synthesis pathways, spectroscopy."},
	{ID: "chemistry_inorganic", Name: "Inorganic Chemistry", Category: "science", Tier: "A",
		Description: "Periodic trends, ionic + covalent bonding, coordination chemistry, transition metals, ceramics + solid-state materials."},
	{ID: "chemistry_biochemistry", Name: "Biochemistry", Category: "science", Tier: "A",
		Description: "Proteins, enzymes, lipids, carbohydrates, nucleic acids, metabolism, biomolecular structure + function."},
	{ID: "chemistry_analytical", Name: "Analytical Chemistry", Category: "science", Tier: "A",
		Description: "Spectroscopic techniques (NMR, IR, mass spec, UV-Vis), chromatography, electrochemical analysis, sample prep, quantification."},
	{ID: "chemistry_physical", Name: "Physical Chemistry", Category: "science", Tier: "A",
		Description: "Quantum chemistry, kinetics, thermodynamics of reactions, molecular spectroscopy, statistical mechanics applied to chemistry."},

	// --- Biology --------------------------------------------------------------
	{ID: "biology_molecular", Name: "Molecular Biology", Category: "science", Tier: "A",
		Description: "DNA / RNA structure + replication, transcription + translation, gene regulation, molecular cloning techniques."},
	{ID: "biology_genetics", Name: "Genetics", Category: "science", Tier: "A",
		Description: "Mendelian + non-Mendelian inheritance, gene mapping, mutation, population genetics, genome sequencing."},
	{ID: "biology_cell", Name: "Cell Biology", Category: "science", Tier: "A",
		Description: "Cell organelles, membrane transport, cell cycle, signal transduction, cytoskeleton, apoptosis, organelle biogenesis."},
	{ID: "biology_ecology", Name: "Ecology", Category: "science", Tier: "A",
		Description: "Ecosystem dynamics, population biology, biodiversity, community interactions, biogeochemical cycles, conservation biology."},
	{ID: "biology_evolution", Name: "Evolution", Category: "science", Tier: "A",
		Description: "Natural selection, speciation, phylogenetics, adaptation, genetic drift, evolutionary development, evidence + mechanisms."},
	{ID: "biology_neuroscience", Name: "Neuroscience", Category: "science", Tier: "A",
		Description: "Neuron structure + function, synaptic transmission, brain anatomy, sensory + motor systems, learning + memory, neural development."},
	{ID: "biology_microbiology", Name: "Microbiology", Category: "science", Tier: "A",
		Description: "Bacteria, viruses, fungi, archaea, microbial physiology, pathogenesis, microbial ecology, applied microbiology."},
	{ID: "biology_botany", Name: "Botany", Category: "science", Tier: "A",
		Description: "Plant anatomy + physiology, photosynthesis, plant reproduction, plant taxonomy, plant ecology, agriculture-relevant biology."},
	{ID: "biology_zoology", Name: "Zoology", Category: "science", Tier: "A",
		Description: "Animal taxonomy, comparative anatomy + physiology, behavior, conservation status, evolutionary relationships across kingdoms."},
	{ID: "biology_immunology", Name: "Immunology", Category: "science", Tier: "A",
		Description: "Innate + adaptive immunity, antibodies, T-cell + B-cell biology, vaccines, immune disorders, transplantation immunology."},

	// --- Earth Sciences -------------------------------------------------------
	{ID: "earthsci_geology", Name: "Geology", Category: "science", Tier: "A",
		Description: "Plate tectonics, mineralogy + petrology, stratigraphy, geomorphology, earthquakes + volcanism, geological time scale."},
	{ID: "earthsci_meteorology", Name: "Meteorology", Category: "science", Tier: "A",
		Description: "Atmospheric physics, weather systems, forecasting, severe weather, climate fundamentals, atmospheric chemistry."},
	{ID: "earthsci_oceanography", Name: "Oceanography", Category: "science", Tier: "A",
		Description: "Ocean circulation, marine geology, marine biology basics, ocean chemistry, coastal processes, marine resources."},
	{ID: "earthsci_climate", Name: "Climate Science", Category: "science", Tier: "B",
		Description: "Climate system, anthropogenic + natural drivers, climate models, paleoclimate, regional climate impacts, mitigation + adaptation. Disclaimer: scientific consensus topic; recommend authoritative sources for policy advice."},
	{ID: "earthsci_environmental", Name: "Environmental Science", Category: "science", Tier: "A",
		Description: "Environmental systems, pollution, biodiversity loss, sustainability metrics, environmental policy basics, ecotoxicology."},

	// --- Mathematics ----------------------------------------------------------
	{ID: "math_algebra", Name: "Algebra", Category: "science", Tier: "A",
		Description: "Linear + abstract algebra, polynomials, equations + systems, group / ring / field theory basics, vector spaces."},
	{ID: "math_calculus", Name: "Calculus", Category: "science", Tier: "A",
		Description: "Limits, derivatives, integrals, multivariable calculus, vector calculus, differential equations basics, applied techniques."},
	{ID: "math_statistics", Name: "Statistics", Category: "science", Tier: "A",
		Description: "Descriptive + inferential statistics, hypothesis testing, regression, ANOVA, Bayesian methods, experimental design."},
	{ID: "math_probability", Name: "Probability", Category: "science", Tier: "A",
		Description: "Probability axioms, random variables, distributions, expectation + variance, Markov chains, stochastic processes."},
	{ID: "math_linear_algebra", Name: "Linear Algebra", Category: "science", Tier: "A",
		Description: "Vector spaces, matrices, eigenvalues + eigenvectors, linear transformations, decompositions, applications."},
	{ID: "math_discrete", Name: "Discrete Mathematics", Category: "science", Tier: "A",
		Description: "Combinatorics, graph theory, recurrence relations, propositional + predicate logic, set theory, number theory basics."},
	{ID: "math_topology", Name: "Topology", Category: "science", Tier: "A",
		Description: "Point-set + algebraic topology, continuity, compactness, connectedness, topological invariants, manifolds intro."},
	{ID: "math_number_theory", Name: "Number Theory", Category: "science", Tier: "A",
		Description: "Divisibility, primes, modular arithmetic, Diophantine equations, cryptographic applications, analytic number theory intro."},

	// --- Computer Science (beyond technical_documentation) --------------------
	{ID: "cs_algorithms", Name: "Algorithms", Category: "technical", Tier: "A",
		Description: "Sorting + searching, divide-and-conquer, greedy, dynamic programming, graph algorithms, string algorithms, complexity analysis."},
	{ID: "cs_data_structures", Name: "Data Structures", Category: "technical", Tier: "A",
		Description: "Arrays, lists, trees (BST, B-tree, trie, heap), hash tables, graphs, persistent structures, amortised analysis."},
	{ID: "cs_complexity_theory", Name: "Computational Complexity", Category: "technical", Tier: "A",
		Description: "P vs NP, NP-completeness, polynomial-time hierarchy, space complexity, approximation + randomised complexity classes."},
	{ID: "cs_distributed_systems", Name: "Distributed Systems", Category: "technical", Tier: "A",
		Description: "CAP theorem, consensus (Paxos / Raft), replication, distributed transactions, fault tolerance, distributed data stores."},
	{ID: "cs_databases", Name: "Databases", Category: "technical", Tier: "A",
		Description: "Relational + NoSQL models, SQL, normalization, indexing, transactions + ACID, query optimization, MVCC."},
	{ID: "cs_networking", Name: "Networking", Category: "technical", Tier: "A",
		Description: "OSI + TCP/IP layers, routing, congestion control, DNS, HTTP / TLS, BGP, modern transport (QUIC)."},
	{ID: "cs_security", Name: "Security & Cryptography", Category: "technical", Tier: "A",
		Description: "Threat models, symmetric + asymmetric cryptography, hashing + MACs, TLS, key exchange, common vulnerabilities (OWASP), secure coding patterns."},
	{ID: "cs_machine_learning", Name: "Machine Learning", Category: "technical", Tier: "A",
		Description: "Supervised + unsupervised learning, neural networks (CNN/RNN/Transformer), training pipelines, evaluation, common architectures + use cases."},
	{ID: "cs_computer_graphics", Name: "Computer Graphics", Category: "technical", Tier: "A",
		Description: "Rasterization + raytracing, shaders, 3D math, lighting models, animation, GPU pipelines, real-time techniques."},
	{ID: "cs_compilers", Name: "Compilers + Languages", Category: "technical", Tier: "A",
		Description: "Lexing + parsing, ASTs, type systems, semantic analysis, intermediate representations, optimization, code generation."},

	// --- Engineering ----------------------------------------------------------
	{ID: "eng_mechanical", Name: "Mechanical Engineering", Category: "science", Tier: "A",
		Description: "Statics + dynamics, fluid mechanics, heat transfer, machine design, manufacturing processes, materials selection."},
	{ID: "eng_electrical", Name: "Electrical Engineering", Category: "science", Tier: "A",
		Description: "Circuit analysis, signals + systems, control theory, power systems, semiconductors, embedded systems."},
	{ID: "eng_civil", Name: "Civil Engineering", Category: "science", Tier: "A",
		Description: "Structural analysis, geotechnical engineering, transportation, water resources, construction management, infrastructure."},
	{ID: "eng_chemical", Name: "Chemical Engineering", Category: "science", Tier: "A",
		Description: "Mass + energy balances, reactor design, separation processes, process control, chemical plant design, safety."},
	{ID: "eng_aerospace", Name: "Aerospace Engineering", Category: "science", Tier: "A",
		Description: "Aerodynamics, propulsion, orbital mechanics, aircraft + spacecraft design, materials for extreme environments."},
	{ID: "eng_biomedical", Name: "Biomedical Engineering", Category: "science", Tier: "A",
		Description: "Medical devices, biomaterials, biomechanics, medical imaging, prosthetics, tissue engineering."},
	{ID: "eng_materials", Name: "Materials Science", Category: "science", Tier: "A",
		Description: "Crystal structures, metals + alloys, polymers, ceramics, composites, nanomaterials, characterization techniques."},
	{ID: "eng_software_architecture", Name: "Software Architecture", Category: "technical", Tier: "A",
		Description: "Architectural patterns, microservices vs monolith, scalability + reliability, event-driven design, API design, DDD."},
	{ID: "eng_control_systems", Name: "Control Systems", Category: "science", Tier: "A",
		Description: "Feedback control, PID, state-space, stability analysis, frequency-domain methods, modern + adaptive control."},

	// --- Medicine & Health ---------------------------------------------------
	// Tier C across the clinical specialties: don't auto-seed actionable
	// medical content. Foundational basic-science domains (anatomy,
	// pharmacology fundamentals) are Tier B with disclaimers.
	{ID: "med_anatomy_physiology", Name: "Anatomy & Physiology", Category: "science", Tier: "B",
		Description: "Human anatomical systems, organ function, homeostasis, physiology of major systems. General reference only; not medical advice."},
	{ID: "med_pharmacology_basics", Name: "Pharmacology Basics", Category: "science", Tier: "B",
		Description: "Drug classifications, mechanisms of action, pharmacokinetics + pharmacodynamics. Educational reference; consult a licensed prescriber for medication decisions."},
	{ID: "med_internal_medicine", Name: "Internal Medicine", Category: "science", Tier: "C",
		Description: "Adult clinical medicine across organ systems. High-stakes domain -- not auto-seeded; upload authoritative clinical references if needed."},
	{ID: "med_surgery_general", Name: "General Surgery", Category: "science", Tier: "C",
		Description: "Surgical techniques + management. High-stakes domain -- not auto-seeded; consult surgical textbooks + licensed practitioners."},
	{ID: "med_surgery_orthopedic", Name: "Orthopedic Surgery", Category: "science", Tier: "C",
		Description: "Musculoskeletal surgical techniques. Tier C -- not auto-seeded."},
	{ID: "med_surgery_cardiac", Name: "Cardiac Surgery", Category: "science", Tier: "C",
		Description: "Cardiothoracic surgical techniques. Tier C -- not auto-seeded."},
	{ID: "med_surgery_neuro", Name: "Neurosurgery", Category: "science", Tier: "C",
		Description: "Neurosurgical techniques. Tier C -- not auto-seeded."},
	{ID: "med_cardiology", Name: "Cardiology", Category: "science", Tier: "C",
		Description: "Diagnosis + management of cardiovascular disease. Tier C -- not auto-seeded."},
	{ID: "med_neurology", Name: "Neurology", Category: "science", Tier: "C",
		Description: "Diagnosis + management of neurological conditions. Tier C -- not auto-seeded."},
	{ID: "med_pediatrics", Name: "Pediatrics", Category: "science", Tier: "C",
		Description: "Clinical pediatric care. Tier C -- not auto-seeded."},
	{ID: "med_geriatrics", Name: "Geriatrics", Category: "science", Tier: "C",
		Description: "Clinical care of older adults. Tier C -- not auto-seeded."},
	{ID: "med_psychiatry", Name: "Psychiatry", Category: "science", Tier: "C",
		Description: "Psychiatric diagnosis + treatment. Tier C -- not auto-seeded."},
	{ID: "med_public_health", Name: "Public Health", Category: "science", Tier: "B",
		Description: "Population-level health, health systems, prevention, health-policy basics. Educational reference."},
	{ID: "med_epidemiology", Name: "Epidemiology", Category: "science", Tier: "A",
		Description: "Study designs, measures of disease frequency, causal inference in observational data, outbreak investigation methods."},
	{ID: "med_dentistry", Name: "Dentistry", Category: "science", Tier: "C",
		Description: "Clinical dentistry. Tier C -- not auto-seeded."},
	{ID: "med_ophthalmology", Name: "Ophthalmology", Category: "science", Tier: "C",
		Description: "Eye care + surgery. Tier C -- not auto-seeded."},
	{ID: "med_dermatology", Name: "Dermatology", Category: "science", Tier: "C",
		Description: "Skin diagnosis + treatment. Tier C -- not auto-seeded."},
	{ID: "med_radiology", Name: "Radiology", Category: "science", Tier: "C",
		Description: "Medical imaging interpretation + technique. Tier C -- not auto-seeded."},
	{ID: "med_anesthesiology", Name: "Anesthesiology", Category: "science", Tier: "C",
		Description: "Perioperative management + anesthesia. Tier C -- not auto-seeded."},
	{ID: "med_veterinary", Name: "Veterinary Medicine", Category: "science", Tier: "C",
		Description: "Animal clinical care + surgery. Tier C -- not auto-seeded."},
	{ID: "med_sports", Name: "Sports Medicine", Category: "science", Tier: "B",
		Description: "Exercise physiology, common athletic injuries, rehab basics, performance science. Educational reference; not a substitute for clinical evaluation."},
	{ID: "med_nursing", Name: "Nursing", Category: "science", Tier: "C",
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
	{ID: "hist_ancient", Name: "Ancient History", Category: "humanities", Tier: "A", BroadSurvey: true,
		Description: "Mesopotamia, Egypt, Greece, Rome, ancient China, India, Mesoamerica through ~500 CE."},
	{ID: "hist_medieval", Name: "Medieval History", Category: "humanities", Tier: "A", BroadSurvey: true,
		Description: "Europe + Mediterranean ~500-1500 CE, Byzantine empire, Islamic world, medieval Asia + Africa."},
	{ID: "hist_early_modern", Name: "Early Modern History", Category: "humanities", Tier: "A", BroadSurvey: true,
		Description: "Renaissance, Reformation, age of exploration, scientific revolution, early colonial empires, ~1500-1800."},
	{ID: "hist_modern", Name: "Modern History", Category: "humanities", Tier: "A", BroadSurvey: true,
		Description: "Industrial revolution, world wars, Cold War, decolonization, late-20th + 21st century, global politics + culture."},
	{ID: "hist_us", Name: "U.S. History", Category: "humanities", Tier: "A", BroadSurvey: true,
		Description: "Colonial period through present day -- founding, expansion, Civil War, industrialization, civil rights, contemporary."},
	{ID: "hist_world_civilizations", Name: "World Civilizations", Category: "humanities", Tier: "A", BroadSurvey: true,
		Description: "Comparative history of major civilizations -- Africa, Asia, Americas, Pacific -- their interactions, technologies, cultures."},

	{ID: "phil_ethics", Name: "Ethics", Category: "humanities", Tier: "A",
		Description: "Normative ethics (consequentialism, deontology, virtue ethics), metaethics, applied ethics, contemporary ethical debates."},
	{ID: "phil_metaphysics", Name: "Metaphysics", Category: "humanities", Tier: "A",
		Description: "Existence, identity, time, causality, free will, mind-body problem, modal realism, ontology fundamentals."},
	{ID: "phil_epistemology", Name: "Epistemology", Category: "humanities", Tier: "A",
		Description: "Knowledge + belief, justification, skepticism, theories of truth, social epistemology, virtue epistemology."},
	{ID: "phil_logic", Name: "Logic", Category: "humanities", Tier: "A",
		Description: "Propositional + predicate logic, modal + temporal logic, formal proof systems, argument analysis, common fallacies."},
	{ID: "phil_political", Name: "Political Philosophy", Category: "humanities", Tier: "A",
		Description: "Justice, liberty, equality, the state, democratic theory, liberalism + alternatives, contemporary political theory."},
	{ID: "phil_mind", Name: "Philosophy of Mind", Category: "humanities", Tier: "A",
		Description: "Consciousness, intentionality, qualia, functionalism, dualism vs physicalism, AI + machine consciousness debates."},
	{ID: "phil_science", Name: "Philosophy of Science", Category: "humanities", Tier: "A",
		Description: "Scientific method, theory choice, paradigm shifts, scientific realism vs antirealism, demarcation problem."},

	{ID: "lit_genres", Name: "Literature: Genres + Forms", Category: "humanities", Tier: "A",
		Description: "Poetry, fiction (novel + short story), drama, non-fiction, essay; key forms, conventions, historical development."},
	{ID: "lit_world", Name: "World Literature", Category: "humanities", Tier: "A",
		Description: "Major literary traditions across cultures + centuries -- canonical works + their context."},
	{ID: "lit_criticism", Name: "Literary Criticism", Category: "humanities", Tier: "A",
		Description: "Major critical schools (formalism, structuralism, post-structuralism, feminist, postcolonial), close-reading techniques."},

	{ID: "linguistics", Name: "Linguistics", Category: "humanities", Tier: "A",
		Description: "Phonology, morphology, syntax, semantics, pragmatics, sociolinguistics, historical + comparative linguistics."},
	{ID: "anthropology", Name: "Anthropology", Category: "humanities", Tier: "A",
		Description: "Cultural anthropology, archaeology, biological anthropology, linguistic anthropology, ethnographic methods."},
	{ID: "sociology", Name: "Sociology", Category: "humanities", Tier: "A",
		Description: "Social structure, institutions, stratification, social change, classical + contemporary theory, methods."},

	{ID: "psych_cognitive", Name: "Cognitive Psychology", Category: "science", Tier: "A",
		Description: "Attention, perception, memory, learning, language processing, reasoning + decision-making, cognitive neuroscience overlap."},
	{ID: "psych_developmental", Name: "Developmental Psychology", Category: "science", Tier: "A",
		Description: "Lifespan development -- infancy through old age, cognitive + social + emotional development, developmental theories."},
	{ID: "psych_social", Name: "Social Psychology", Category: "science", Tier: "A",
		Description: "Attitudes, conformity, group dynamics, persuasion, intergroup relations, social cognition, classic experiments."},
	{ID: "psych_behavioral", Name: "Behavioral Psychology", Category: "science", Tier: "A",
		Description: "Classical + operant conditioning, behavior modification, applied behavior analysis, learning theory."},
	{ID: "psych_clinical_basics", Name: "Clinical Psychology Basics", Category: "science", Tier: "B",
		Description: "Diagnostic frameworks (DSM-5 overview), major therapy modalities (CBT, psychodynamic, humanistic). Educational reference; not a substitute for licensed clinical care."},

	{ID: "econ_micro", Name: "Microeconomics", Category: "humanities", Tier: "A",
		Description: "Supply + demand, consumer + producer theory, market structures, game theory basics, externalities + public goods."},
	{ID: "econ_macro", Name: "Macroeconomics", Category: "humanities", Tier: "A",
		Description: "GDP + national accounts, monetary + fiscal policy, business cycles, inflation, employment, growth theory."},
	{ID: "econ_behavioral", Name: "Behavioral Economics", Category: "humanities", Tier: "A",
		Description: "Cognitive biases, prospect theory, nudges, intertemporal choice, behavioral game theory, applications to policy."},
	{ID: "econ_development", Name: "Development Economics", Category: "humanities", Tier: "A",
		Description: "Growth + poverty, institutional economics, foreign aid effectiveness, RCT methodology, comparative development."},

	{ID: "polisci_us", Name: "U.S. Political Science", Category: "humanities", Tier: "A",
		Description: "U.S. constitutional structure, federalism, branches of government, parties + elections, contemporary political institutions."},
	{ID: "polisci_comparative", Name: "Comparative Politics", Category: "humanities", Tier: "A",
		Description: "Political systems across countries, regime types, democratization, comparative institutions, party systems."},
	{ID: "polisci_international", Name: "International Relations", Category: "humanities", Tier: "A",
		Description: "Theories of IR (realism, liberalism, constructivism), international institutions, security studies, IPE, foreign policy."},

	{ID: "religious_studies", Name: "Religious Studies", Category: "humanities", Tier: "A",
		Description: "World religions (their texts, practices, history), comparative religion, religious philosophy, secular study of religion."},

	// --- Arts & Design --------------------------------------------------------
	{ID: "art_drawing_painting", Name: "Drawing & Painting", Category: "creative", Tier: "A",
		Description: "Drawing fundamentals, color theory, composition, mediums (oil, acrylic, watercolor, ink), historical movements + techniques."},
	{ID: "art_sculpture", Name: "Sculpture", Category: "creative", Tier: "A",
		Description: "Traditional + modern sculpture, materials (clay, stone, metal, wood), techniques (carving, modeling, assemblage), installation."},
	{ID: "art_digital", Name: "Digital Art", Category: "creative", Tier: "A",
		Description: "Vector + raster workflows, common tools (Procreate, Photoshop, Figma), 3D modeling basics, AI-assisted art."},
	{ID: "art_photography_advanced", Name: "Advanced Photography", Category: "creative", Tier: "A",
		Description: "Exposure + lens choice mastery, manual workflows, lighting setups, advanced post-processing, portfolio building, commercial vs editorial."},
	{ID: "art_history", Name: "Art History", Category: "humanities", Tier: "A",
		Description: "Major movements from antiquity to contemporary -- Renaissance, Baroque, Impressionism, Modernism, Postmodernism."},

	{ID: "music_theory", Name: "Music Theory", Category: "creative", Tier: "A",
		Description: "Notation, scales + modes, harmony + chord progressions, counterpoint, form + analysis, ear training basics."},
	{ID: "music_composition", Name: "Music Composition", Category: "creative", Tier: "A",
		Description: "Compositional techniques across genres, melodic + harmonic development, orchestration, songwriting craft, recording basics."},
	{ID: "music_performance", Name: "Music Performance", Category: "creative", Tier: "A",
		Description: "Practice methodologies, performance anxiety management, ensemble playing, instrument-specific technique principles."},
	{ID: "performing_arts", Name: "Performing Arts", Category: "creative", Tier: "A",
		Description: "Theater, dance, opera; performance traditions, training methodologies, production practice."},

	{ID: "architecture", Name: "Architecture", Category: "creative", Tier: "A",
		Description: "Architectural history + theory, design process, structural fundamentals, materials, sustainability, contemporary practice."},
	{ID: "design_industrial", Name: "Industrial Design", Category: "creative", Tier: "A",
		Description: "Product design process, ergonomics, manufacturing constraints, sustainability, prototyping, design + brand integration."},
	{ID: "design_graphic", Name: "Graphic Design", Category: "creative", Tier: "A",
		Description: "Typography, layout, color, identity systems, print + digital media, design principles + history."},
	{ID: "design_ux", Name: "UX Design", Category: "creative", Tier: "A",
		Description: "User research methods, information architecture, interaction design, prototyping, usability testing, design systems."},
	{ID: "design_fashion", Name: "Fashion Design", Category: "creative", Tier: "A",
		Description: "Garment construction, pattern-making, textile knowledge, fashion history, sustainable fashion, industry workflows."},
	{ID: "film_production", Name: "Film + Video Production", Category: "creative", Tier: "A",
		Description: "Camera + lens fundamentals, cinematography, editing, sound design, color grading, production workflows, screenwriting basics."},
	{ID: "game_design", Name: "Game Design", Category: "creative", Tier: "A",
		Description: "Game mechanics, level design, narrative design, playtesting, balance, common engines (Unity / Unreal / Godot) basics."},

	// --- Specialized Fields ---------------------------------------------------
	{ID: "law_constitutional", Name: "Constitutional Law", Category: "specialized", Tier: "B",
		Description: "Foundational constitutional principles + case law (focused on U.S.). Educational reference; consult a licensed attorney for legal advice."},
	{ID: "law_criminal", Name: "Criminal Law", Category: "specialized", Tier: "B",
		Description: "Substantive + procedural criminal law fundamentals. Educational reference; not legal advice."},
	{ID: "law_civil_procedure", Name: "Civil Procedure", Category: "specialized", Tier: "B",
		Description: "Civil litigation procedure, jurisdiction, pleadings, discovery, judgment + appeals. Educational reference; not legal advice."},
	{ID: "law_intellectual_property", Name: "Intellectual Property Law", Category: "specialized", Tier: "B",
		Description: "Copyright, trademark, patent, trade-secret fundamentals. Educational reference; consult an IP attorney for filings + enforcement."},
	{ID: "law_international", Name: "International Law", Category: "specialized", Tier: "B",
		Description: "Public + private international law fundamentals, treaties, human rights, international courts. Educational reference."},
	{ID: "law_environmental", Name: "Environmental Law", Category: "specialized", Tier: "B",
		Description: "Environmental regulatory frameworks (NEPA, CWA, CAA, Superfund), permitting, enforcement basics. Educational reference."},
	{ID: "law_tax", Name: "Tax Law", Category: "specialized", Tier: "B",
		Description: "Federal tax law fundamentals, tax research, common entity-tax topics. Educational reference; consult a CPA / tax attorney for planning + filing."},

	{ID: "edu_pedagogy", Name: "Pedagogy", Category: "humanities", Tier: "A",
		Description: "Teaching methodologies, learning theories (constructivism, behaviorism), classroom management, lesson planning."},
	{ID: "edu_assessment", Name: "Assessment + Evaluation", Category: "humanities", Tier: "A",
		Description: "Formative + summative assessment, rubrics, validity + reliability, standardized testing critique, alternative assessment."},
	{ID: "edu_special", Name: "Special Education", Category: "humanities", Tier: "B",
		Description: "Learning disabilities overview, IEP/504 frameworks, inclusion strategies. Educational reference; consult specialists for individual planning."},
	{ID: "edu_edtech", Name: "Educational Technology", Category: "humanities", Tier: "A",
		Description: "Learning platforms, digital tools, blended + flipped classroom models, accessibility considerations, contemporary EdTech."},

	{ID: "journalism", Name: "Journalism", Category: "humanities", Tier: "A",
		Description: "Reporting practice, interviewing, fact-checking, investigative methods, ethics + standards, media literacy."},

	// --- Sports (per-sport) ---------------------------------------------------
	{ID: "sport_football_american", Name: "Football (American)", Category: "hobby", Tier: "A",
		Description: "Rules, positions, schemes (offensive + defensive), notable history, fantasy basics, NFL + NCAA structure."},
	{ID: "sport_soccer", Name: "Soccer / Football", Category: "hobby", Tier: "A",
		Description: "Rules, positions, formations, major leagues + tournaments (FIFA, UEFA, MLS), notable history."},
	{ID: "sport_basketball", Name: "Basketball", Category: "hobby", Tier: "A",
		Description: "Rules, positions, offensive + defensive systems, NBA + NCAA structure, advanced stats, notable history."},
	{ID: "sport_baseball", Name: "Baseball", Category: "hobby", Tier: "A",
		Description: "Rules, positions, in-game strategy, sabermetrics intro, MLB + minor leagues, notable history."},
	{ID: "sport_tennis", Name: "Tennis", Category: "hobby", Tier: "A",
		Description: "Rules, scoring, technique fundamentals, ATP + WTA tour structure, Grand Slams, notable history."},
	{ID: "sport_golf", Name: "Golf", Category: "hobby", Tier: "A",
		Description: "Rules + etiquette, club selection, course management, scoring formats, PGA + LPGA structure, equipment."},

	// --- Games + Recreation ---------------------------------------------------
	{ID: "games_board", Name: "Board Games", Category: "hobby", Tier: "A",
		Description: "Modern board game design, classic games (chess, Go, backgammon), strategy + Eurogames, social deduction, game-night planning."},
	{ID: "games_video", Name: "Video Games", Category: "hobby", Tier: "A",
		Description: "Major genres + franchises, current platforms, gaming history, e-sports basics, accessibility + parental considerations."},
	{ID: "games_card", Name: "Card Games", Category: "hobby", Tier: "A",
		Description: "Traditional card games (poker, bridge, hearts), trading card games (Magic, Pokemon), solitaire variants, card-game strategy basics."},
	{ID: "games_chess", Name: "Chess", Category: "hobby", Tier: "A",
		Description: "Opening principles + named openings, middlegame strategy, endgame fundamentals, common tactics, ratings + tournament play."},

	{ID: "outdoor_hiking", Name: "Hiking & Backpacking", Category: "hobby", Tier: "B",
		Description: "Trail planning, gear selection, backcountry navigation, weather assessment, basic wilderness safety. Refer to authoritative outdoor-safety sources for emergencies."},
	{ID: "outdoor_camping", Name: "Camping", Category: "hobby", Tier: "A",
		Description: "Site selection, gear, cooking outdoors, weather considerations, family vs solo + car-camping vs backcountry."},
	{ID: "outdoor_climbing", Name: "Climbing", Category: "hobby", Tier: "B",
		Description: "Indoor + outdoor climbing styles (top-rope, lead, bouldering, trad), gear basics, technique, safety fundamentals. Take in-person instruction for outdoor climbing."},
	{ID: "outdoor_fishing", Name: "Fishing", Category: "hobby", Tier: "A",
		Description: "Freshwater + saltwater techniques, gear selection, regulations + licensing, common species, fly-fishing intro."},
	{ID: "outdoor_hunting", Name: "Hunting", Category: "hobby", Tier: "B",
		Description: "Game species, regulations + licensing, firearm + bow safety basics, scouting + field-dressing fundamentals. In-person hunter-safety courses required in most jurisdictions."},

	{ID: "collecting_coins", Name: "Coin Collecting", Category: "hobby", Tier: "A",
		Description: "Numismatics intro, grading, U.S. + world coin identification, storage + protection, key dates, market basics."},
	{ID: "collecting_stamps", Name: "Stamp Collecting", Category: "hobby", Tier: "A",
		Description: "Philately intro, identification, country + topical collecting, condition + grading, market basics."},
	{ID: "collecting_antiques", Name: "Antiques Collecting", Category: "hobby", Tier: "A",
		Description: "Identifying authentic antiques, eras + styles, valuation basics, restoration vs preservation, market dynamics."},
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
	"personal_finance":    "B",
	"personal_taxes":      "B",
	"personal_investing":  "B",
	"personal_insurance":  "B",
	"personal_budgeting":  "B",

	// Personal legal matters + estate planning -- disclaimer required.
	"personal_legal":            "B",
	"estate_planning_personal":  "B",
	"contracts_personal":        "B",

	// Health + medical -- general info, not professional advice.
	"mental_health":            "B",
	"medical_records_personal": "B",
	"sleep_hygiene":            "B",
	"dietary_restrictions":     "B",

	// Parenting + child development -- safety-relevant content
	// (developmental milestones, when-to-call-a-doctor signals).
	"parenting":         "B",
	"child_development": "B",

	// Tax regulations + labor law in the business set.
	"tax_regulations": "B",
	"labor_law":       "B",
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
	"general_assistant": {"inventory_supply_chain", "financial_data", "employee_records", "customer_relations", "product_catalog", "quality_metrics", "legal_documents", "project_data", "technical_documentation", "strategic_planning", "stakeholder_communications"},
	"accounting_finance": {"financial_data", "accounting_principles", "tax_regulations", "budgeting_forecasting", "payroll_benefits", "inventory_supply_chain", "contracts_agreements", "risk_management", "regulatory_compliance"},
	"human_resources":    {"employee_records", "talent_acquisition", "labor_law", "training_development", "organizational_design", "payroll_benefits", "performance_assessment", "legal_documents", "regulatory_compliance"},
	"customer_service":   {"customer_relations", "product_catalog", "service_level_agreements", "ticket_management", "quality_metrics", "sales_pipeline", "training_development"},
	"quality_assurance":  {"quality_metrics", "product_catalog", "process_optimization", "technical_documentation", "regulatory_compliance", "service_level_agreements", "data_analysis", "research_methodology"},
	"sales_marketing":    {"sales_pipeline", "marketing_analytics", "brand_strategy", "lead_generation", "customer_relations", "product_catalog", "contracts_agreements", "data_analysis", "stakeholder_communications"},
	"it_support":         {"technical_documentation", "network_infrastructure", "cybersecurity", "software_development", "cloud_platforms", "ticket_management", "process_optimization", "vendor_management"},
	"legal_compliance":   {"legal_documents", "contracts_agreements", "regulatory_compliance", "intellectual_property", "labor_law", "risk_management", "tax_regulations", "stakeholder_communications"},
	"operations":         {"process_optimization", "logistics_distribution", "vendor_management", "inventory_supply_chain", "quality_metrics", "product_catalog", "budgeting_forecasting", "risk_management"},
	"project_management": {"project_data", "process_optimization", "budgeting_forecasting", "stakeholder_communications", "risk_management", "quality_metrics", "vendor_management", "data_analysis", "organizational_design"},
	"research_development": {"research_methodology", "data_analysis", "innovation_management", "technical_documentation", "intellectual_property", "product_catalog", "software_development", "budgeting_forecasting"},
	"training_education":   {"curriculum_design", "performance_assessment", "training_development", "employee_records", "organizational_design", "data_analysis", "technical_documentation", "stakeholder_communications"},

	// --- Personal-category role -> domain mappings ---
	// Per the v1 brainstorm + the personal-tier expansion: knowledge
	// domains can encapsulate either broad themes (reference content
	// like recipes / how-to guides) OR granular data (validated
	// records like a household inventory or a personal medical log).
	// Mapping below mixes both shapes per role -- the SAME domain
	// concept holds both whether agents read it as RAG content
	// (documentChunk text) or query it as records (typed concepts
	// like SpreadsheetRow).
	"personal_finance_advisor": {"personal_finance", "personal_taxes", "personal_investing", "personal_insurance", "personal_budgeting", "estate_planning_personal"},
	"household_manager":        {"household_maintenance", "home_inventory", "household_chores", "personal_finance", "personal_budgeting"},
	"parenting_coach":          {"parenting", "child_development", "school_education_personal", "nutrition", "mental_health"},
	"health_wellness_coach":    {"nutrition", "fitness", "mental_health", "sleep_hygiene", "medical_records_personal", "mindfulness"},
	"meal_planning_chef":       {"recipes", "meal_planning", "dietary_restrictions", "nutrition"},
	"travel_planner":           {"travel_planning", "travel_documents", "restaurant_dining", "personal_finance"},
	"creative_companion":       {"creative_arts", "music_appreciation", "photography", "personal_growth"},
	"learning_companion":       {"language_learning", "online_courses", "book_summaries", "personal_growth"},
	"relationships_social":     {"relationships_communication", "life_events_celebrations", "gifts"},
	"pet_care_specialist":      {"pet_care", "pet_training", "pet_health"},
	"home_improvement_diy":     {"diy_repairs", "gardening", "sustainability", "home_inventory", "household_maintenance"},
	"personal_legal_advisor":   {"personal_legal", "estate_planning_personal", "contracts_personal"},
	"mindfulness_coach":        {"mindfulness", "journaling", "personal_growth", "mental_health"},
	"entertainment_curator":    {"entertainment_media", "music_appreciation", "book_summaries", "restaurant_dining"},
	"senior_care_advisor":      {"elder_care", "end_of_life_planning", "medical_records_personal", "estate_planning_personal", "personal_legal"},

	// Real-estate advisor draws on the seven dedicated real-estate
	// domains plus tangential personal domains: personal_finance +
	// personal_taxes (mortgage cost + tax implications),
	// personal_insurance (homeowners / renters coverage during the
	// transaction), home_inventory (rolls forward into the new
	// property), personal_legal (purchase contract review backstop),
	// and contracts_personal (lease + offer-letter review patterns
	// transfer cleanly to real-estate paperwork).
	"real_estate_advisor":      {"real_estate_listings", "mortgage_shopping", "neighborhood_research", "home_inspection", "property_taxes", "lease_agreements", "closing_process", "personal_finance", "personal_taxes", "personal_insurance", "home_inventory", "personal_legal", "contracts_personal"},
}

// copresentUISeedCorpus is the initial content that populates the
// copresent_ui domain so the first walkthrough has something to work
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
		Text: `CoPresent is a real-time collaboration app where humans and AI agents share "spaces" for conversation. There is ONE real route -- /space -- with a single ?panel= query axis that drives the right column. Every other historical path (/dashboard, /spaces, /agents, /settings, /profile) redirects to /space (+ the matching panel query when applicable). The top-level layout: a LEFT presence panel (auto-shows when the user is joined to the active space; collapses when they aren't), a CENTRE canvas (a quiet liquid-glass surface that hosts the per-space card timeline; the legacy R3F welcome scene + stereoscopic / particle stack was retired) that expands into the left region when no presence is mounted, and a RIGHT column that renders when ?panel= is set. Header elements, left-to-right: the app logo, a space-context pill (shows the active space's title; clicking routes to ?panel=chat so the chat elevates into the right column) or a dashed "Select a space" placeholder when none is active, a segmented nav toggle with SIX tiles ([Spaces | Agents | Knowledge | Training | Tasks | Settings]; writes the ?panel= query), an optional **Computer Use pill** (data-op-id=header.computer-use-pill; only renders when the user has a paired worker -- see surface:header.computerUsePill), and a Profile pill on the far right (initials avatar + first + last name; opens the Profile MODAL, not a route). The PROFILE modal (data-op-id=header.profile) replaces the old /profile page; it holds account details, the CoPresent + MemQL versions, and sign-out actions. CHAT has no tile in the header nav anymore: the space-context pill already routes to ?panel=chat, and when joined chat also floats as a widget over the canvas, so the duplicate was retired. Common user paths: "open spaces" -> uiClick nav.spaces; "manage agents" -> uiClick nav.agents; "manage knowledge / domains / library / sources" -> uiClick nav.knowledge; "train an agent / change an agent's knowledge or skills" -> uiClick nav.training (this is the ONLY surface for editing knowledge + skills now -- the Create Agent modal no longer carries those tabs); "see what tasks are running / done" -> uiClick nav.tasks; "change settings" -> uiClick nav.settings; "pause computer-use globally / open computer-use settings" -> uiClick header.computer-use-pill; "view profile / sign out" -> uiClick header.profile; "return to the active conversation" -> uiClick the space-context pill (data-op-id=nav.currentSpace).`,
	},
	{
		SourceRef: "surface:agents",
		Text: `The Agents right panel (opId=agents.listPanel, opens at ?panel=agents via uiClick nav.agents; the legacy /agents URL redirects to /space?panel=agents) lists every AI agent the user has. The list is sorted in two buckets: the auto-seeded General Assistant (Sofia) is PINNED at the top, then a hairline "Specialists" divider (opId=agents.list.divider), then the rest of the specialists alphabetically. The General Assistant is a per-user singleton and cannot be created from the picker -- "General Assistant" is explicitly excluded from the role dropdown in the Create Agent modal. The "+" button (opId=agents.new) opens the Create Agent modal for creating specialist agents only. ACTION BUTTONS PER ROW are STATE-DEPENDENT -- this is the most common place agents get confused, read this carefully: (a) General Assistant row: Edit ONLY (agents.row.edit.<id>). There is NO deactivate and NO delete button -- the GA is a structural requirement for space creation, agent auto-join, and other guarded flows. Do not try to click a delete button on Sofia; it does not exist. (b) Active specialist row: Edit (agents.row.edit.<id>) + Deactivate (agents.row.deactivate.<id>). NO delete button yet. (c) Inactive specialist row: Activate (agents.row.activate.<id>) + Delete (agents.row.delete.<id>). NO edit button here -- inactive agents must be reactivated first if you want to edit them. So to DELETE A SPECIALIST, you always need TWO clicks: first uiClick agents.row.deactivate.<id> (the row re-renders into the inactive shape), then uiClick agents.row.delete.<id> (opens a confirm dialog). To EDIT, just uiClick agents.row.edit.<id> -- it opens the CreateAgentModal in edit mode, pre-populated with the agent's current name, role, gender, personality. ROLE-MOVED FIELDS (read this carefully): KNOWLEDGE DOMAINS and SKILLS are NO LONGER part of the Create Agent or Edit Agent modal -- they moved to the Training panel (?panel=training, see surface:training). When a user asks to "give my Operations agent the email skill" or "add HR knowledge to Cleo", DO NOT open the Edit modal -- open Training instead. The Edit modal carries Personality + basic fields only; the Role dropdown is locked for the GA (cannot demote Sofia from general_assistant) but editable for specialists. Per-agent skill chips on the agent CARD (in the AgentsListPanel rows) hide bundle primitives -- when an agent has the Computer Use umbrella the chip reads "Computer Use", not "workerHost" / "workerComputer" / "workerStatus" (those are internal fan-out names; the user picks the umbrella).`,
	},
	{
		SourceRef: "concept:agentLifecycle.deleteFlow",
		Text: `Deleting a specialist agent is a GATED two-step flow because Delete only appears on an INACTIVE row. The correct walkthrough cadence when a user says "delete <name>": (1) uiReadState -- confirm the row shows Edit + Deactivate (active) or Activate + Delete (already inactive). (2) If active: uiHighlight agents.row.deactivate.<id>, narrate "We'll deactivate <name> first so the Delete button appears", uiAskUser "Ready to deactivate <name>?" with ["Yes, deactivate", "Cancel"]. On Yes, uiClick agents.row.deactivate.<id> -- the row re-renders, Edit/Deactivate disappear and Activate/Delete appear. (3) uiHighlight agents.row.delete.<id>, uiAskUser "Ready to delete <name>? This cannot be undone." with ["Yes, delete <name>", "Cancel"]. On Yes, uiClick agents.row.delete.<id>. (4) A confirm dialog mounts (agents.deleteConfirm.*); if present, uiClick agents.deleteConfirm.confirm to finalize. COMMON MISSTEPS to avoid: do NOT click agents.row.edit.<id> expecting a Delete option inside the edit modal -- there is no Delete inside the CreateAgentModal, delete lives only on the inactive-row action bar. Do NOT try to delete the General Assistant; its row has no Delete/Deactivate buttons at all (see surface:agents). If the user asks to delete the GA, narrate that it's structurally required and offer to edit it instead.`,
	},
	{
		SourceRef: "concept:agentLifecycle.editFlow",
		Text: `Editing an agent opens the CreateAgentModal in EDIT mode (same component as Create, toggled by passing the agent prop). Entry: uiClick agents.row.edit.<id>. The modal pre-populates every field (Gender, Name, Role, Intelligence policy, Personality styles, Knowledge Domains, Skills) from the agent's stored values, skips the Describe phase entirely (lands directly on Configure), and the submit button reads "Save Changes" instead of "Create Agent". The pinned describe strip (textarea + "Set up my agent" button) is hidden in edit mode -- there's no natural-language re-description step. All Configure-form fields behave identically to Create; the Role dropdown is the only field with an EDIT-mode nuance: when editing the General Assistant (id startsWith 'ga-' or role=='general_assistant'), the Role dropdown is DISABLED so the user cannot demote Sofia away from general_assistant and break the per-user singleton invariant -- every other field (including name!) is still editable for the GA. If the agent has an explicit model pin saved, the "Advanced" collapsible at the bottom of the card auto-opens so the user sees the current override without having to click through. Editing an inactive specialist requires REACTIVATING it first (Activate button on the row) because the row in inactive state shows Activate + Delete, no Edit. Walkthrough cadence for edit: (1) uiClick agents.row.edit.<id>. (2) uiReadState to confirm the modal is open (look for createAgent.submit). (3) Make the requested change (uiType for Name, uiSelect for Role or Intelligence policy, uiClick for list rows inside Personality / Knowledge / Skills tabs). (4) Commit confirmation: uiHighlight createAgent.submit, uiAskUser "Ready to save changes to <Name>?" with ["Yes, save", "Cancel"]. On Yes, uiClick createAgent.submit.`,
	},
	{
		SourceRef: "surface:createAgentModal",
		Text: `The Create Agent modal (route modal:createAgent) is a two-phase form. Phase 1 (Describe) is a pinned textarea at the top of the modal (createAgent.description) with a voice-input toggle (createAgent.voice) and a "Set up my agent" button (createAgent.generate) that fires AI suggestion. Phase 2 (Configure) appears below once a suggestion lands -- the same pinned strip stays visible but the button morphs into "Start over" (createAgent.startOver). The footer's secondary button switches from "Configure manually" (createAgent.configureManually; skips AI generation and opens Phase 2 with defaults) in Describe phase to the primary Create button (createAgent.submit) in Configure phase. Phase 2 is laid out as a single card. TOP of the card -- BASIC FIELDS (pinned, always visible): (1) Gender pill toggle (createAgent.gender.female / createAgent.gender.male), (2) Name input (createAgent.name) with random-name button (createAgent.randomName), (3) Role <select> (createAgent.role), (4) Intelligence policy <select> (createAgent.policy). BOTTOM of the card -- PERSONALITY PICKER ONLY: a search input (createAgent.tab.personality.search) above a sub-category pill strip (createAgent.tab.personality.subcategory.<key> with keys 'all' | 'warm' | 'direct' | 'thoughtful'), then a scrollable list of style rows (createAgent.style.<slug>). KNOWLEDGE AND SKILLS TABS WERE REMOVED from this modal -- they live exclusively on the Training panel (?panel=training, see surface:training). The Create Agent modal's purpose is now "name + role + voice"; everything else happens after creation, in Training. BOTTOM-MOST: an "Advanced -- pin a specific model" collapsible (createAgent.advancedToggle) that expands to expose createAgent.explicitModel. Use the COMMIT CONFIRMATION pattern (concept:commitConfirmation) before pressing createAgent.submit: highlight the submit button, uiAskUser to confirm, and on "Yes" uiClick it yourself. After creation, if the user asked for specific knowledge or skills in the original request, narrate "Now let's open Training to give <Name> her knowledge and skills" and uiClick nav.training to chain into the Training flow.`,
	},
	{
		SourceRef: "surface:createAgentModal.personalityPicker",
		Text: `The Configure card's lower half is the PERSONALITY PICKER (knowledge and skills moved to Training -- they are NOT here anymore). Layout top-to-bottom: (A) a search input (createAgent.tab.personality.search) for filtering personality styles by typed text. (B) a SUB-CATEGORY pill strip (createAgent.tab.personality.subcategory.<key>) with keys 'all' | 'warm' | 'direct' | 'thoughtful' for narrowing. (C) a scrollable list of style rows -- each row is a full-width button with a checkbox indicator on the left, a label in the middle, and a lock glyph on the right if locked. Row op-ids: createAgent.style.<slug>. The Aligned style (createAgent.style.aligned) is ALWAYS LOCKED-ON -- the organisation-wide baseline voice every agent carries; it cannot be deselected (see concept:lockedAlignedStyle). To toggle an additional style use uiClick createAgent.style.<slug>; aria-pressed signals current state. To switch sub-category use uiClick createAgent.tab.personality.subcategory.<key>. The scrollable list auto-scrolls when uiClick/uiPointerTo targets a row that isn't visible. There is no Knowledge tab and no Skills tab here -- if a walkthrough plan tries to click createAgent.tab.knowledge or createAgent.tab.skills.<anything>, those op-ids do not exist; reroute the agent's flow to the Training panel after createAgent.submit.`,
	},
	{
		SourceRef: "surface:createAgentModal.fieldSemantics",
		Text: `CRITICAL disambiguation on the Configure Agent form: Name and Role are TWO DIFFERENT FIELDS. NAME (op-id createAgent.name, a single-line input) is the agent's DISPLAY NAME -- one word, like "Zeus", "Astra", "Iris", "Felix". It's how the user will address the agent in conversation. NAME IS NOT THE ROLE. When the user says "create an IT Support agent", that's the ROLE, not the name; the agent still needs a name. ROLE (op-id createAgent.role, a <select> dropdown) is the agent's domain specialty. THE EXACT ROLE SLUGS ARE (USE THESE VERBATIM): general_assistant, accounting_finance, human_resources, customer_service, quality_assurance, sales_marketing, it_support, legal_compliance, operations, project_management, research_development, training_education. Common user phrase -> slug: "Operations" / "Operations Manager" / "ops" -> "operations" (NOT "operations_manager"); "IT support" -> "it_support"; "HR" / "Human Resources" -> "human_resources"; "sales" / "marketing" -> "sales_marketing"; "R&D" / "research" -> "research_development"; "legal" -> "legal_compliance"; "project manager" / "PM" -> "project_management"; "QA" / "quality" -> "quality_assurance"; "training" -> "training_education"; "accounting" / "finance" -> "accounting_finance"; "customer service" / "support" -> "customer_service". Set role with uiSelect(createAgent.role, value=<slug>), NOT uiType. Never type "IT Support" into NAME. Never type a role slug into NAME. Always ask the user what NAME they want via uiAskUser. If they say "any" or "random" or "you pick", click createAgent.randomName (a button next to the name input) which generates a locale+gender-appropriate random name in the field. If uiSelect returns "no option matching" with an Available list, retry uiSelect using an EXACT slug from that list. INTELLIGENCE (op-id createAgent.policy, a <select>) is a separate basic field that picks the SI Router policy the agent runs on. Policy slugs: balancedChat (default), strongReasoning, fastCoding, lowLatencyVoice, cheapestCapable. The Advanced collapsible at the bottom of the card (createAgent.advancedToggle) is where the rare case of pinning a specific model lives -- uiClick it to expand, then uiSelect createAgent.explicitModel. Most walkthroughs never touch Advanced; only mention it if the user explicitly asks to pin a model.`,
	},
	{
		SourceRef: "surface:createAgentModal.walkthroughCadence",
		Text: `When walking a user through Create Agent, pace the flow field-by-field. After opening the modal and clicking createAgent.configureManually (which skips the AI-suggest Describe step and jumps to Phase 2), the standard cadence is: (1) BASIC FIELDS FIRST. uiAskUser gender (Female / Male) and uiClick createAgent.gender.{female|male}. (2) uiAskUser "What name would you like for your agent?" offering options like ["Zeus (male)", "Aria (female)", "Pick for me"]. If they pick "Pick for me", uiClick createAgent.randomName. If they give a name, uiType it into createAgent.name. (3) uiSelect createAgent.role with the role slug derived from their original request ("IT support" -> "it_support"). (4) INTELLIGENCE policy: uiSelect createAgent.policy (default "balancedChat" is fine for almost every walkthrough; only ask if the user mentions needing fast coding, low-latency voice, strong reasoning, or cheap). SKIP the Advanced pin-a-model collapsible unless explicitly asked. (5) PERSONALITY PICKER. Before pointing at the personality list, emit uiPointerTo on a representative style row so the modal scrolls and the user sees what you're describing (see concept:scrollIntoView). Only ask about ADDITIONAL styles -- Aligned is locked-on. Optionally narrow via uiClick createAgent.tab.personality.subcategory.{warm|direct|thoughtful} if the user's description leans one way. Then uiClick createAgent.style.<slug>. (6) NO KNOWLEDGE STEP HERE. NO SKILLS STEP HERE. The Create Agent modal does not carry knowledge or skills tabs anymore -- those moved to Training. Do NOT try to click createAgent.tab.knowledge / createAgent.tab.skills / createAgent.domain.<slug> / createAgent.capability.<slug> / createAgent.integration.<slug>; they don't exist on this modal. (7) COMMIT CONFIRMATION: uiHighlight createAgent.submit, then uiAskUser({question: "Ready to create <Name>, your <role> agent?", options: ["Yes, create <Name>", "Cancel"], allowFreeForm: false}). On "Yes" uiClick createAgent.submit yourself. (8) IF the user's original request mentioned specific knowledge or skills ("an HR agent who handles email", "Operations agent that knows our finance docs"), CHAIN INTO TRAINING: narrate "Now let's open Training to give <Name> the knowledge and skills you asked for" and uiClick nav.training. Then drag <Name> from the agents palette onto the Studio's Agent slot, drag the requested domains + skills into Knowledge and Skills zones, uiHighlight training.train, COMMIT CONFIRMATION, uiClick training.train. See surface:training and surface:trainingStudio for the full Training flow.`,
	},
	{
		SourceRef: "concept:walkthroughRules",
		Text: `Walkthrough mode = teaching, not execution. Rules: (1) NARRATE before each meaningful action using uiNarrate when interactivity is 'conversational'. One short sentence per step, present tense: "Opening the Agents panel now." / "Asking for a name next." (2) ASK before filling fields with values the user didn't explicitly provide. Use uiAskUser with 2-3 concrete options plus free-form. Do NOT derive a name, style, or gender from the user's request text — those were role/context descriptions, not field values. (3) PAUSE between meaningful steps: use uiPointerTo or uiHighlight to draw the user's eye before uiClick. (4) BE PATIENT with your iteration budget. Each uiAskUser round-trip is one of your turns; budget accordingly. If you run out, the user sees you freeze. Prefer fewer smarter asks over many tiny ones. (5) Never click commit (Create, Save, Delete, Send). Always highlight + release + summarise.`,
	},
	{
		SourceRef: "concept:scrollIntoView",
		Text: `SCROLL THE USER TO WHAT YOU'RE TALKING ABOUT. Modal forms (Create Agent, Create Space, Settings) scroll internally, and fields below the fold are INVISIBLE until the section is scrolled into view. Narration alone does not scroll the page -- the user reads your message and looks at whatever was already visible, missing the section you described. Every cursor-moving primitive auto-scrolls its target into view via scrollIntoView: uiPointerTo, uiHighlight, uiClick, uiSelect, uiType. So the discipline is: before narrating about a section you haven't touched yet, emit a uiPointerTo on a representative op-id in that section. This pulls the section into the viewport and gives the user a cursor anchor pointing at what you're describing. Correct pattern: uiPointerTo(createAgent.domain.business_administration) -> uiNarrate("Next is Knowledge Domains -- Cleo's subject areas. I've picked the ones IT Support maps to."). WRONG pattern: uiNarrate("Knowledge Domains are below, you can scroll to see them.") -- the user doesn't scroll, they wait for you to scroll. When highlighting a commit button at the end (uiHighlight createAgent.submit) the highlight itself scrolls, so no extra pointer needed. SPECIAL CASE -- TABBED PICKER NESTED SCROLL: the Create Agent Personality / Knowledge / Skills tab panels each contain their OWN overflow-y-auto list that is typically only ~250px tall. A target row can have pixel-coordinates that fall inside the browser viewport while still being clipped by the tab panel's own scroll boundary. The auto-scroll primitive handles this by walking scrollable ancestors, but you should STILL emit uiPointerTo on the target row BEFORE narrating or clicking, especially when the row is not at the top of the list. Correct cadence for adding "Creative" to Personality: uiClick createAgent.tab.personality -> uiPointerTo createAgent.style.creative (this scrolls the row into the panel's visible area) -> uiNarrate "I'll add Creative to the mix so she can brainstorm freely." -> uiClick createAgent.style.creative. For sections you've scrolled past without interacting -- Personality list, Knowledge list, Skills list -- always pointer-in first, then narrate, then act.`,
	},
	{
		SourceRef: "surface:spaces",
		Text: `The Spaces right panel (opens at ?panel=spaces via uiClick nav.spaces) lists the user's conversation spaces and is the user's primary surface for picking, creating, and managing them. Every space is a multi-participant room with configurable slot caps (default 5 humans, 3 agents); the owner's General Assistant auto-joins every space they create. THREE-TAB LIFECYCLE: Active (status=active; the working set), Saved (status=saved; the user explicitly chose to keep these forever), and Archived (status=archived; auto-deletes after the retention window). Tab buttons: spaces.tab.active, spaces.tab.saved, spaces.tab.archived. Active also shows a count badge. Each row shows its name + last-activity timestamp + per-state action buttons; the row body itself is spaces.row.select.<id> for opening the space (auto-joins on click; no manual Join button anymore). Per-state row actions: ACTIVE rows have spaces.row.rename.<id>, spaces.row.save.<id>, spaces.row.archive.<id>. SAVED rows have spaces.row.rename.<id>, spaces.row.archive.<id> (saved -> archived restarts the retention countdown). ARCHIVED rows have spaces.row.save.<id> (rescue from purge), spaces.row.restore.<id> (back to active), spaces.row.delete-now.<id> (skip the wait, hard-delete now); they also show a small "expires in N days" countdown badge. The "+" button (top right) opens Create Space (modal:createSpace). DAILY SPACE: when User.preferences.dailySpaceEnabled is on (default), a private auto-provisioned per-user-per-day singleton is pinned at the TOP of the Active tab with a "Daily" badge. The pinned daily row has NO per-row action buttons -- the rollover is automation-managed (yesterday's daily archives or saves at the next day boundary based on User.preferences.dailySpaceRolloverAction). Non-daily Active rows still get the full Rename / Save / Archive triplet. When the user clicks any space row, the chat widget floats on the right side of the canvas (or elevates into the right column if the user clicks the space-context pill to route to ?panel=chat); the presence panel shows up on the left automatically. Auto-join replaced the legacy "Join this space" empty-state -- clicking a row IS the join; there is no manual Join button.`,
	},
	{
		SourceRef: "surface:settings",
		Text: `The Settings right panel (opens at ?panel=settings via uiClick nav.settings; the legacy /settings URL redirects to /space?panel=settings) has sections for General (theme switcher, language selector, replay-intro button), CoPresent Control (the per-mode settings for agent takeovers -- see surface:settings.copresentControl), Spaces (lifecycle preferences -- archive retention 30/60 days at settings.spaces.retention.<value>, IANA timezone for the daily-space rollover at settings.spaces.timezone, daily-space enable toggle at settings.spaces.dailyEnabled, daily-rollover action archive/save at settings.spaces.rollover.<value>), Groups (admin view of groups the user belongs to, with invite flows), Devices (microphone / camera / audio output selectors, accessed via the presence.devices button that opens a session-settings panel), and Sessions (active session list, sign-out-all-sessions). EVERY setting on this panel persists to the user's record in memQL (v1:identity:user.preferences) -- not browser localStorage. That means preferences follow the user across browsers / devices / fresh sessions; clearing browser data does NOT reset them. The legacy 3D Glasses / stereoscopic-rendering toggle (settings.stereo3D) was retired along with the R3F welcome-scene stack and is no longer present.`,
	},
	{
		SourceRef: "surface:settings.copresentControl",
		Text: `The CoPresent Control section on the Settings page (data-op-id=settings.copresentControl) is the home for all preferences that shape how an agent drives the UI on the user's behalf. It splits into two subsections that mirror the two CoPresent Control modes: Standard Mode (data-op-id=settings.copresentControl.standard) for transactional one-shot sessions, and Interactive Mode (data-op-id=settings.copresentControl.interactive) for walkthroughs / demos / teaching flows where the agent narrates each step. Which mode applies at runtime is decided per session by the agent based on the user's request shape -- it is NOT a global toggle. Standard Mode owns: Appearance (clean / dim) and a continuous Cursor Speed slider. Interactive Mode owns: a Pace preset (quick / steady / deliberate) that maps to a fixed cursor speed (500 / 1000 / 1500ms per move). The two are independent -- changing one never affects the other. All four values persist to v1:identity:user.preferences (cursorTweenMs, takeoverMode, interactivePace) on the server, so they follow the user across browsers and devices.`,
	},
	{
		SourceRef: "surface:profile",
		Text: `The Profile MODAL (data-op-id=header.profile trigger; the modal itself carries profile.modal.* op-ids) shows the user's account information: first name, last name, email, phone, gender, date of birth, role in the organisation, the CoPresent app version, and the MemQL version. All editable fields use inline edit controls; the app + MemQL versions are read-only. Sign-out and sign-out-all-sessions live in the modal footer. The modal opens by clicking the header's Profile pill on the far right of the nav row (initials avatar + user name). The legacy /profile URL redirects to /space -- there is no longer a dedicated profile route or right-panel Profile tab. To open programmatically during a takeover: uiClick header.profile. To close: uiClick profile.modal.close.`,
	},
	{
		SourceRef: "concept:controlSession",
		Text: `A Control Session is the bounded window during which an agent drives the UI on the user's behalf via the CoPresent Control feature. Visual signals while a session is active: a soft theme-coloured cursor (~28px) animates to target elements with an ease-in-out-sine curve; an optional spotlight pulses around highlighted targets; two liquid-glass surfaces float over the app -- the **CoPresent Control Widget** (data-op-id=copresentControl.widget; the agent's narration transcript + options + free-form input; same widget family as Chat / Presence widgets) and the Take-Back Control button (data-op-id=control.take-back; bottom-right). In Standard Mode the widget is a one-shot card that only appears when the agent is asking; in Interactive Mode it persists for the full session and accumulates a transcript. Sessions are begun with uiRequestControl({reason, interactivity}) and ended with uiReleaseControl({summary}). The session persists across uiNavigate calls AND uiClick calls that trigger route changes -- so a cross-page session is ONE session, not many. Commit buttons (Create/Save/Delete/Send/Invite/Remove/Confirm/Sign-Out) are NOT silent -- before clicking one the agent asks for explicit consent via uiAskUser with options like ["Yes, create it", "Cancel"], and only clicks on "Yes". The user's option pick IS the consent. This replaces the older "never click commit" rule so multi-step walkthroughs (e.g. create agent + create space) can chain without the user having to manually take back control between steps.`,
	},
	{
		SourceRef: "concept:releaseLanding",
		Text: `LAND THE CURSOR ON A NAVIGABLE SURFACE BEFORE uiReleaseControl. The agent cursor fades when the user clicks Take Back Control; whatever element the cursor was last over is where it disappears. Releasing while the cursor is in the middle of a Settings field, a closed modal, or a buried form section leaves the user without an obvious next step -- the takeover ends and they're stranded. The fix is a single uiClick on the header's space-context pill as the LAST action before uiReleaseControl. Two variants render in the header (only one at a time): when an active space is selected the pill carries data-op-id="nav.currentSpace" and routes to ?panel=chat (drops the user into the active conversation); when no active space is selected (typical first-login / fresh-account state) the pill carries data-op-id="nav.noActiveSpaceLabel" and routes to ?panel=spaces (drops the user on the spaces list). Read the surfaces / after-state to pick the right one and uiClick it. Same rule for BOTH CoPresent Control modes -- Standard (minimal interactivity) and Interactive (conversational interactivity). The intro / first-login walkthrough also follows this rule (the runOnboarding orchestrator does it automatically before its own release call), so behaviour stays uniform whether the agent is LLM-driven or curriculum-driven. SKIP the landing click only when (a) your goal already finished on a useful surface (Profile modal open, the row the user asked about is highlighted in a list, the spaces panel is showing the new space you just created), or (b) the user explicitly asked to be left where they are. Forcing a redundant click steals their context. Otherwise: click the pill, then release. The cursor fading on a real button reads as "agent finished and handed me a starting point", not "agent abandoned me mid-form".`,
	},
	{
		SourceRef: "concept:twoModes",
		Text: `CoPresent Control has TWO modes, picked per-session by the agent at uiRequestControl time. STANDARD MODE ('minimal' interactivity): for transactional one-shot actions where every required value is already specified -- "change theme to dark", "delete agent named Test". One-shot uiAskUser card appears only when explicitly asked; agent saves narration for the final uiReleaseControl summary. The user's Settings -> CoPresent Control -> Standard Mode picks the appearance (clean / dim) and the cursor speed (continuous slider). INTERACTIVE MODE ('conversational' interactivity): for create / edit / invite / configure flows with missing required fields, OR any goal with pedagogical phrasing ("walk me through", "teach me", "show me how"). A persistent mini-chat surface mounts for the whole session; the agent streams narration via uiNarrate; the user can interject any time. The user's Settings -> CoPresent Control -> Interactive Mode picks a Pace preset (quick / steady / deliberate) that controls cursor speed during the session. The two modes are independent at the settings level; which one applies at runtime is the agent's call based on the user's request shape.`,
	},
	{
		SourceRef: "concept:alreadySelected",
		Text: `CHECK BEFORE YOU ACT. Every toggle in CoPresent's forms -- gender pills, personality rows, knowledge domain rows, skill rows (capabilities + integrations), tab buttons, sub-category pills, create-space agent-selector rows -- carries aria-pressed="true" when it's the currently selected option. <select> elements (createAgent.role, createAgent.policy, createAgent.explicitModel) carry the current value on el.value. Inputs (createAgent.name, etc.) carry the current text on el.value. BEFORE calling uiClick on a toggle, uiSelect on a dropdown, or uiType on an input, verify the value isn't already what the user asked for. Signals: the uiReadState / after-state summary shows aria-pressed="true" on the matching row, or a previous step returned "already set to X" / "already active". If the value already matches, DO NOT call the primitive -- clicking an already-pressed TOGGLEABLE row DESELECTS it (surprising the user who just said "yes, that one"), retyping the same name churns the input, re-selecting the same option wastes an iteration. Instead emit uiHighlight({target: <field-op-id>}) + uiNarrate({target: <field-op-id>, message: "<field> is already set to <value>"}). SEPARATE CASE -- LOCKED ROWS: some rows are permanently selected and cannot be toggled at all. They look visually identical to a selected toggleable row (checked + primary-tinted border) but carry a small lock glyph on the right edge, plus either a disabled attribute on the <button> or are rendered as a plain <div> (no button element at all, for the pinned auto-join rows). uiReadState marks these as non-interactive / locked. NEVER emit uiClick on a locked row -- the click is a no-op, the cursor flies for nothing, the user sees a pointless move, and you burn an iteration. This is the single most common misfire in walkthroughs because aria-pressed="true" reads the same on both; always check for the lock signal before concluding "already selected, I should click to deselect". See concept:lockedDefaults for the full per-modal roster of locked rows. The backend short-circuits the action if you do call it anyway, returning a text that includes "already set" / "already active" / "locked" -- but that still costs a turn you should have saved.`,
	},
	{
		SourceRef: "concept:highlightBeforeMove",
		Text: `Every cursor-moving primitive (uiClick, uiType, uiSelect, uiPointerTo) now wraps its action in a scoped spotlight: the target ELEMENT is highlighted BEFORE the cursor starts moving, the cursor flies to the highlighted element, the action fires (or short-circuits if already set), a brief dwell lets the user see the result with the ring still on, then the highlight fades. The ring paints directly on the real DOM node via a data-copresent-highlight attribute -- no separate overlay, no tracking loop, no ghost ring when the target re-renders. uiHighlight is still the "leave it on until explicitly cleared" primitive; the others are scoped to their single action. You don't need to call uiHighlight before every uiClick -- the click already carries its own spotlight for the duration of the action. Reserve uiHighlight for the final "here's the commit button, press it" moment or when narrating about a section the user should look at without any action happening on it.`,
	},
	{
		SourceRef: "surface:copresentControlWidget",
		Text: `The **CoPresent Control Widget** (data-op-id=copresentControl.widget) is the agent's conversational surface during a Control Session. It is part of the Canvas widget family (sibling of canvas.chatWidget and canvas.presenceWidget) and uses the same liquid-glass styling, but only mounts while a Control Session is active. Two render shapes, one per CoPresent Control mode: in **Standard Mode** (interactivity=minimal) it appears as a one-shot card at the bottom of the viewport ONLY while a uiAskUser is pending -- question + options + optional free-form, dismisses on answer. In **Interactive Mode** (interactivity=conversational) it persists for the entire session, accumulating a transcript of agent narration (uiNarrate) and user messages, with a free-form input the user can use to interject anytime; collapsible via copresentControl.widget.collapse. Sub-targets all live under the copresentControl.widget container: copresentControl.widget.collapse (collapse/expand toggle), copresentControl.widget.thinking (info-only badge shown while the agent is preparing its first action -- not clickable), copresentControl.widget.resume (explicit "Resume" button that surfaces when the session is paused with done=true; clearing the flag reactivates the widget -- typing in the input resumes implicitly too, this button is the no-typing path), copresentControl.widget.input (free-form input), copresentControl.widget.send (send button). On a long cursor move the widget re-positions to the side opposite the cursor's region (Presence side -> widget flies right; Chat side -> widget flies left) so it never overlaps what the agent is pointing at. Aliases: "control widget", "agent widget", "agent panel", "agent surface".`,
	},
	{
		SourceRef: "concept:canvasWidgets",
		Text: `CoPresent has three floating widgets that share one liquid-glass aesthetic (frosted white-alpha surface, backdrop-blur-2xl + saturate-150, soft inset highlight on top edge, white-alpha text). The Chat WIDGET (right side, 384px wide, data-op-id=canvas.chatWidget) and the Presence WIDGET (left side, 384px wide, data-op-id=canvas.presenceWidget) are HIDDEN BY DEFAULT -- the canvas is clean on entry. Users open each via a corner restore button; closing returns to the clean state. The **CoPresent Control Widget** (data-op-id=copresentControl.widget) only appears DURING a Control Session and disappears on uiReleaseControl+endSession; sub-targets inside it carry the matching prefix (copresentControl.widget.collapse / .input / .send) and declare data-op-container="copresentControl.widget" so the discovery layer knows to require an active session. All three share the same liquid-glass styling (src/lib/theme/liquidGlass.ts -- glassWidgetRoot / glassText / glassAccentButton / glassControlSurface). IMPORTANT: chat + presence widgets are MUTUALLY EXCLUSIVE with their corresponding side panels. Opening the Presence widget COLLAPSES the left panel; canvas expands into that space. Opening the Chat widget COLLAPSES the right column ONLY when chat was the right-column tile (rightView === 'chat'); otherwise the chat widget floats alongside. Entering FOCUS-CANVAS auto-shows both widgets; exiting auto-hides them. During walkthroughs, do NOT open/close widgets -- the user's layout is the starting point. The CoPresent Control Widget is an exception: it only mounts while the session is live, so agents don't manage its visibility directly.`,
	},
	{
		SourceRef: "concept:surfaceAwareness",
		Text: `Every uiReadState response and every tool result's after-state line includes a ` + "`surfaces`" + ` field: a compact list of the canonical top-level regions currently mounted and targetable in the DOM. Canonical surface op-ids: ` + "`presence.panel`" + ` (left presence panel), ` + "`spacePage.chatRightPanel`" + ` (right chat panel), ` + "`canvas.presenceWidget`" + ` (floating presence widget on the canvas), ` + "`canvas.chatWidget`" + ` (floating chat widget on the canvas), ` + "`copresentControl.widget`" + ` (control widget during an active session). CHECK ` + "`surfaces`" + ` BEFORE NARRATING about participants, video/audio controls, chat input, or anything surface-scoped -- narration must match what the user actually sees. Pairings are mutually exclusive: presence.panel XOR canvas.presenceWidget (never both; canvas-expand unmounts the panel entirely), spacePage.chatRightPanel XOR canvas.chatWidget for chat. Phrase narration accordingly: "Presence widget" / "Chat widget on the canvas" when the canvas.* entries are in ` + "`surfaces`" + `, "Presence panel" / "Chat panel" when the panel entries are. Do NOT default to "panel" when the widget is what's on screen -- users find it disorienting ("the cursor is over the widget but the agent is describing the panel"). If the required surface isn't in ` + "`surfaces`" + ` and the task needs it, prefer opening the PANEL via the header nav tile over toggling the widget -- the panel is the primary surface; the widget is a canvas-mode overlay.`,
	},
	{
		SourceRef: "surface:canvas.presenceWidget",
		Text: `The Presence WIDGET (data-op-id=canvas.presenceWidget) is the floating left-side presence surface in FOCUS-CANVAS mode. It is NOT a mirror of the Presence panel -- it REPLACES it. When canvas is expanded, the regular PresencePanel is UNMOUNTED (leftColumnMounted=false on SpacePage); presence.panel is absent from the DOM. The widget is then the only presence surface the user can see, and every presence.* op-id (presence.camera-toggle, presence.mic-toggle, presence.devices, presence.layout.gallery, presence.layout.spotlight, presence.invite, per-participant cards under presence.participant.<id>) is rendered INSIDE the widget -- same underlying MediaControls / GalleryLayout / SpotlightLayout components, just hosted in the widget's 384px liquid-glass frame. HOW TO TELL WHICH SURFACE YOU ARE ON: read the ` + "`surfaces`" + ` list in uiReadState / the after-state line of every tool result. If ` + "`canvas.presenceWidget`" + ` is listed, narrate in terms of "the Presence widget on the canvas"; DO NOT say "the Presence panel" -- the panel does not exist on screen. If ` + "`presence.panel`" + ` is listed instead, narrate in terms of "the Presence panel on the left". These are mutually exclusive -- only one will be in surfaces at a time. Visibility toggle is canvas.widgetsVisibility (same op-id as the chat widget's toggle; subject="presence" differentiates "Hide presence" vs "Show presence"). Walkthrough rule: DON'T open/close the widget as part of the walkthrough -- the user's chosen layout is the starting state; narrate against whatever is already on screen. Only open a panel/widget if the task genuinely requires a surface that isn't currently present.`,
	},
	{
		SourceRef: "surface:canvas.chatWidget",
		Text: `The Chat WIDGET (data-op-id=canvas.chatWidget) is the floating right-side chat surface that appears in FOCUS-CANVAS mode or when the right column is on a non-chat view. It is NOT a mirror of the right-column chat panel -- it REPLACES it. When canvas is expanded or the widget is open over a non-chat right column, the chat right-column panel (data-op-id=spacePage.chatRightPanel) is UNMOUNTED (rightColumnRendered flips false); spacePage.chatRightPanel is absent from the DOM. The widget then hosts every chat.* op-id: chat.input (free-form composer), chat.send (send button -- bottom-row action and inline on the input when the textarea has content), chat.mic (speech-to-text toggle), and the implicit message list (one row per utterance, no single chat.messages op-id). HOW TO TELL WHICH SURFACE YOU ARE ON: read the ` + "`surfaces`" + ` list in uiReadState / after-state. If ` + "`canvas.chatWidget`" + ` is listed, narrate "the Chat widget on the canvas"; if ` + "`spacePage.chatRightPanel`" + ` is listed, narrate "the Chat panel on the right". Mutually exclusive. Primary uses during a takeover: (1) show the user how to send a message (uiHighlight chat.input -> uiAskUser "what would you like me to type?" -> uiType into chat.input -> uiHighlight chat.send -> user confirms -> uiClick chat.send). (2) Show where speech-to-text lives (uiPointerTo chat.mic). Visibility toggle is canvas.widgetsVisibility with subject="chat" ("Hide chat" / "Show chat"); same op-id as the presence widget's toggle, differentiated by subject. Walkthrough rule: DON'T open/close the widget -- narrate against the starting layout.`,
	},
	{
		SourceRef: "concept:lockedAlignedStyle",
		Text: `The Create Agent modal's Personality Style section has a LOCKED default chip called "Aligned" (op-id createAgent.style.aligned) that sits first in the row, is always selected, and cannot be deselected. Aligned represents the organization-wide baseline voice every CoPresent agent carries -- professional, helpful, consistent across conversations. The other chips (Friendly, Professional, Assertive, Empathetic, Analytical, Creative, Patient, Concise) are layered ON TOP of Aligned, not alternatives to it. During a walkthrough, do NOT ask the user whether to include Aligned (it's not a choice) -- only ask which ADDITIONAL styles they want. If the user says "no special personality" or "just default", that's fine: Aligned alone is a complete valid configuration. When narrating this section, mention that Aligned is always on and the user's picks are extras, so they don't feel like the form is missing something.`,
	},
	{
		SourceRef: "concept:lockedDefaults",
		Text: `Across every CoPresent form, the only fields pre-selected by default are LOCKED items -- the user cannot uncheck them, and neither can you. DETECTION: a locked row shows (a) a small lock glyph (LockClosedIcon) on its right edge, and (b) either the disabled attribute on its <button>, or is rendered as a <div> instead of a button at all (the pinned auto-join GA rows follow this pattern and carry data-op-intent="info"). uiReadState flags both shapes as non-interactive / locked. RULE: NEVER emit uiClick on a locked row. The click is swallowed -- the cursor flies for no visible effect, the user loses confidence, and you burn an iteration. Full per-modal roster of locked items:

CREATE AGENT (route modal:createAgent):
- Personality tab: createAgent.style.aligned ("Aligned", the organisation-wide baseline voice; always on; every agent carries this on top of any user picks).
- Knowledge tab (general_assistant role ONLY): createAgent.domain.business_administration ("Business Administration", the org-wide baseline domain; locked-on for the GA. Specialists do NOT have this locked -- the row is opt-in like any other domain).
- Skills tab, capabilities sub-tab (locked for EVERY role -- the universal agent toolkit, six entries): createAgent.capability.data_query, createAgent.capability.document_search, createAgent.capability.calendar_access, createAgent.capability.notifications, createAgent.capability.email_compose, createAgent.capability.task_management.
- Skills tab, integrations sub-tab (locked for General Assistant role ONLY; specialists do not get it locked and can opt in): createAgent.integration.copresent_control (ties the GA to the CoPresent Control feature so she can drive the UI on the user's behalf).

CREATE SPACE (route modal:createSpace): createSpace.generalAssistant -- a PINNED non-clickable row at the top of the agent selector. Shows the owner's GA name plus the subtitle "General Assistant - Joins automatically". It is a <div>, not a button, so it has no aria-pressed, no checkbox, and no onClick -- uiClick is a category error here, not just a no-op. The backend auto-joins the GA via the autoJoinSI automation regardless of what the picker sends, so there is nothing to toggle. The legacy phrasing "the GA does not appear in the selector" is outdated -- it IS visible, just locked.

CREATE GROUP (route modal:group): createGroup.generalAssistant -- a PINNED non-clickable row at the top of the agents picker, rendered ONLY when the signed-in user is a selected member of this group. Subtitle: "General Assistant - Joins because you're a member". Same <div>-not-button semantics as the Create Space pinned row. The GA auto-joins because its owner (the current user) is in the group; if the user removes themselves from the member list, the pinned row disappears and the GA no longer auto-joins.

Everything else in these forms starts UNSELECTED and requires an explicit user pick. This contrasts with many apps that pre-fill "reasonable defaults" for optional fields; CoPresent deliberately doesn't, so the user's choices feel intentional. During walkthroughs, NEVER assume an optional field has a sensible default you can fill without asking -- Gender, Personality styles beyond Aligned, additional Knowledge Domains, additional Skills (capabilities + integrations beyond the locked defaults), additional specialist agents in Create Space, additional members/agents in Create Group all need user input (via uiAskUser with 2-3 concrete options). EXCEPTIONS: (a) role-derived inference -- if the user said "IT support agent", the ROLE field can be populated without asking (it_support), but NAME still needs to be asked. (b) The Intelligence policy stays at "balancedChat" unless the user mentions a specific need (fast coding, strong reasoning, low-latency voice, cheapest). (c) The "Advanced -- pin a specific model" collapsible (createAgent.advancedToggle / createAgent.explicitModel) stays closed and untouched unless the user explicitly asks to pin a model.`,
	},
	{
		SourceRef: "surface:createSpaceModal",
		Text: `The Create Space modal (op-id createSpace) is a two-phase form similar to Create Agent. Phase 1 (Describe) takes a natural-language description; Phase 2 (Configure) lets the user set the title (createSpace.title), description (createSpace.description), and selected specialist agents (createSpace.agent.<id> per row). Every space is multi-participant: the owner's General Assistant auto-joins every space and is rendered as a PINNED locked row at the top of the agent selector (data-op-id=createSpace.generalAssistant) showing the GA's name plus the subtitle "General Assistant - Joins automatically". That row is a <div>, not a button -- it has no checkbox, no aria-pressed, no onClick -- so do NOT uiClick it; see concept:lockedDefaults for detection and handling. Only the specialist agents BELOW the pinned row are pickable (one button per row at createSpace.agent.<id>, up to the remaining agent-slot budget). Specialist rows carry aria-pressed so uiClick detects already-selected state. The "Configure manually" shortcut (createSpace.configureManually) skips Phase 1. Commit: uiHighlight createSpace.submit, then uiAskUser({question: "Ready to create the <Title> space?", options: ["Yes, create it", "Cancel"], allowFreeForm: false}). On "Yes" uiClick createSpace.submit yourself. On "Cancel" release. This is the COMMIT CONFIRMATION pattern (concept:commitConfirmation) -- the user's option pick is the consent signal, replacing the older "never click commit" rule so create-agent -> create-space chains can complete in one session.`,
	},
	{
		SourceRef: "concept:commitConfirmation",
		Text: `COMMIT CONFIRMATION is the required pattern for EVERY mutating click (Create / Save / Delete / Send / Invite / Remove / Confirm / Sign-Out). Before clicking the commit button, the agent MUST: (1) uiHighlight the commit button so the user's eye is on it. (2) uiAskUser({question, options: ["Yes, <verb> <subject>", "Cancel"], allowFreeForm: false}) -- binary consent gate, not open-ended. The question names the specific subject (NAME / TITLE) so the user confirms a SPECIFIC object. (3) On "Yes" -- uiClick the commit button yourself; the user's pick IS the consent. Then narrate and chain into any follow-up step. (4) On "Cancel" or empty answer -- narrate that the walkthrough can't complete without this commit and release. This replaces the older "never click commit" rule. Rationale: requiring the user to manually press Create forced them to take back control, which cancelled the session and broke multi-step walkthroughs like "create an operations agent, then create a weekly meeting space with that agent." The uiAskUser pick is a STRONGER consent than a silent button press because the user read a sentence naming the exact subject. Destructive actions (delete, sign out all, kick participant) use the same pattern but with harder warning copy ("This can't be undone." / "You'll need to log in again elsewhere.").`,
	},
	{
		SourceRef: "surface:settings.copresentControl.standard.cursorSpeed",
		Text: `The Standard Mode Cursor Speed slider (data-op-id=settings.copresentControl.standard.cursorSpeed) lives on the Settings panel under Settings -> CoPresent Control -> Standard Mode. Container chain: nav.settings -> settings.copresentControl -> settings.copresentControl.standard. Only affects Standard Mode (transactional / minimal interactivity) sessions; Interactive Mode reads from the pace preset instead. It has a COUNTER-INTUITIVE inverted mapping you MUST understand before driving it: the slider's INPUT VALUE is NOT the ms duration shown in the readout -- they are inverted via the formula sliderValue = MAX+MIN - cursorTweenMs (MAX=2500, MIN=250). What that means in practice: slider value 250 = visual LEFT = "Slow" label = cursorTween 2500ms (slowest cursor); slider value 2500 = visual RIGHT = "Fast" label = cursorTween 250ms (fastest cursor). The readout next to the slider ("Xms per move") displays the ACTUAL duration -- low ms (250ms) = fast, high ms (2500ms) = slow. COMMON MISTAKE: seeing "2500ms per move" and typing 100 into the slider to "go faster." That clamps to 250 (the slider's min), which leaves the cursor at the SLOWEST setting. CORRECT BEHAVIOUR: To set max speed, uiType('2500') into the slider input. To set min speed, uiType('250'). The confirm happens automatically on change (persisted to v1:identity:user.preferences.cursorTweenMs in memQL on the server, not the browser, so the value follows the user across devices). Alternative phrasings the user might say: "max speed", "fastest cursor", "slow it down", "speed up the agent" -- all map to either 2500 (fast) or 250 (slow) as the VALUE typed into the range input, never the ms duration.`,
	},
	{
		SourceRef: "surface:settings.copresentControl.standard.appearance",
		Text: `The Standard Mode Appearance toggle (data-op-ids: settings.copresentControl.standard.appearance.clean and settings.copresentControl.standard.appearance.dim) lives on the Settings panel under Settings -> CoPresent Control -> Standard Mode. Container chain: nav.settings -> settings.copresentControl -> settings.copresentControl.standard. Two values: 'clean' (default; rest of the app is disabled but not visually darkened -- agent cursor + highlight spotlight are the only takeover cues) and 'dim' (rest of the viewport tinted dark with a cursor-following spotlight; stronger "agent is driving" signal). Clicks are blocked across the entire viewport in BOTH options -- this only changes the visual appearance. ONLY affects Standard Mode (transactional / minimal interactivity) sessions; Interactive Mode is always clean regardless of this preference because the user needs to read what they're being shown. Persists to v1:identity:user.preferences.takeoverMode on the server, so the choice follows the user across browsers and devices. Phrasings: "dim mode", "clean mode", "darken the app when agent drives", "no dim during takeovers" -- all map here. uiClick the matching button to set; aria-pressed marks the active one.`,
	},
	{
		SourceRef: "surface:settings.copresentControl.interactive.pace",
		Text: `The Interactive Mode Pace preset (data-op-ids: settings.copresentControl.interactive.pace.deliberate, settings.copresentControl.interactive.pace.steady, settings.copresentControl.interactive.pace.quick) lives on the Settings panel under Settings -> CoPresent Control -> Interactive Mode. Container chain: nav.settings -> settings.copresentControl -> settings.copresentControl.interactive. Three positions, ordered slow -> fast left to right (matching the Standard Mode cursor speed slider's Slow -> Fast direction): 'deliberate' (1500ms per move; presentation pace, demo audience can follow without strain), 'steady' (1000ms per move; default, visible enough to follow but brisk), 'quick' (500ms per move; snappy, for users who already know the app). Controls cursor speed during Interactive Mode (conversational interactivity) sessions only -- walkthroughs, demos, teaching flows. Has NO effect on Standard Mode -- that uses the continuous slider. The readout next to the picker shows the current ms duration. Persists to v1:identity:user.preferences.interactivePace on the server, so it follows the user across browsers and devices. Phrasings the user might say: "speed up the walkthrough", "slow down demos", "presentation pace", "make the agent quicker when teaching" -- all map to one of the three presets, NOT to the Standard Mode slider.`,
	},
	{
		SourceRef: "concept:multiStepWalkthrough",
		Text: `Multi-step walkthroughs chain two or more create / configure flows in one Control Session. Canonical example: "Create an operations agent, then create a weekly meeting space with that agent." Steps: (1) uiRequestControl({reason: "Setting up your operations agent + meeting space", interactivity: "conversational"}). (2) Drive create-agent form field-by-field (see surface:createAgentModal.walkthroughCadence). (3) At createAgent.submit, COMMIT CONFIRMATION pattern: highlight + uiAskUser("Ready to create <Name>, your Operations agent?") + on "Yes" click submit yourself. Session stays live. (4) Narrate the transition: "Agent created -- now let's make the space that uses <Name>." (5) Navigate to create-space (uiClick nav.spaces or equivalent), click + NEW, click Configure manually, fill title ("Weekly Operations Meeting"), toggle the new agent in createSpace.agent.<newId>. (6) At createSpace.submit, COMMIT CONFIRMATION again: highlight + uiAskUser("Ready to create the Weekly Operations Meeting space?") + on "Yes" click submit. (7) uiReleaseControl with a summary naming both created objects. Critical rules: do NOT uiReleaseControl between steps -- one session covers the whole chain. Do NOT force the user to press the first commit manually (that cancels the session). The uiAskUser confirmation at each commit is the consent gate that lets the chain survive.`,
	},
	{
		SourceRef: "surface:tasks",
		Text: `The Tasks right panel (data-op-id=tasks.listPanel, opens at ?panel=tasks via uiClick nav.tasks) lists every Plan in the active space. A Plan is a unit of work the planner orchestrates -- v0.1's only Plan kind is 'analyzeFile' (created automatically when the user drops a file in chat); future kinds (analyzeText, conductResearch, executeWorkflow, etc.) will land here as the planner gains capabilities. Plans are grouped into THREE collapsible sections: Active (statuses: queued / routing / running), Needs attention (status: failed -- v0.2 also: needsAgent, awaitingFeedback), and Done (statuses: succeeded / cancelled, last 7 days by default). Each row shows: a kind icon, status pill, the Plan's goal text (e.g. "Analyze Q3-headcount.xlsx"), the assigned agent (if any), the Plan's duration once finished, and a relative timestamp. Per-row op-ids: tasks.row.<planId>.toggle (expand drawer with input/output JSON), tasks.row.<planId>.cancel (DISABLED in v0.1 -- the cancel mutation lands in v0.2). Empty state when no Plans exist: "Drop a file in chat to start one." The Tasks page deep-links via ?panel=tasks&plan=<planId> (the plan.completed canvas card's "View task" button drives this) -- the panel auto-expands the matching row. v0.2 adds: per-Plan token-spend bar, needs-feedback amber badge with one-click respond, refinement count, expand-to-Tasks listing under a Plan, time-window filter chip on the Done section.`,
	},
	{
		SourceRef: "concept:planner",
		Text: `The PLANNER is the system that turns "user actions worth tracking" (file drops, chat-triggered analysis requests, future research / workflow goals) into Plan + Task records the user can see and act on. Two concepts model the work: v1:planner:plan (the user-visible unit -- one Plan per dropped file in v0.x) and v1:planner:task (one executable step inside a Plan; v0.x always has exactly one Task per analyzeFile Plan, kind 'fileProcessor'). The frontend surfaces the planner via TWO places: (1) the canvas, which lands a 'plan.created' card at start and a 'plan.completed' card on every Plan terminal transition (succeeded / failed / cancelled) showing the result summary + Validate / Reject / Refine / Attach-to-domain actions, and (2) the Tasks right panel (?panel=tasks) listing every Plan grouped by status. Lifecycle covers: queued -> routing -> running (with sub-states paused / awaitingFeedback / needsAgent) -> succeeded | failed | cancelled. v0.x collapses these transitions into one synchronous pass inside the file-upload handler -- the analysis runs inline, the Plan + Task + Document records are stamped with timestamped transitions, and the canvas cards land. Subsequent rounds lift the analysis itself into a planner-owned async execution surface; the Plan goes 'queued' on file drop, the user can keep chatting, and the plan.completed card lands when the Plan terminates. From the user's vantage the synchronous -> asynchronous jump is invisible: same UI, same shapes, just longer wall-clock between transitions on long-running work. WALKTHROUGH GUIDANCE: when a user asks "what tasks are running" / "what's the planner doing" / "did my file analysis finish" / "show me the analysis history" -> uiClick nav.tasks. When a user asks about a specific file analysis they just dropped -> point them to the plan.completed canvas card on the active space's canvas and offer to open ?panel=tasks for the full list. When a user wants to refine an analysis ("look at column X again", "categorize by department instead") -> open the plan.completed card and uiClick canvas.card.planCompleted.refine.toggle, then narrate the inline composer that spawns a child Plan. The planner is NOT the same thing as the takeover agent (CoPresent Control) -- the planner orchestrates background work the system does on the user's behalf; CoPresent Control is the user authorizing an agent to drive their UI for a bounded session.`,
	},
	{
		SourceRef: "surface:knowledge",
		Text: `The Knowledge right panel (data-op-id=knowledge.listPanel, opens at ?panel=knowledge via uiClick nav.knowledge) is the user's library of validated content the agents draw from when answering. Two-column layout: LEFT is the domain list (grouped by category: Core, Business, Technical, Product, Internal) with a "+ New" pill button at the top right of the panel header (data-op-id=knowledge.new) that opens an inline create form. RIGHT is the selected-domain detail showing the domain's name + description + drop-zone for direct file upload + the list of attached validated Documents. Per Q21: domains carry a SCOPE -- 'workspace' (default; visible to everyone in the workspace) or 'private' (visible only to the creator). The create form's scope picker has two pills: knowledge.new.scope.workspace and knowledge.new.scope.private. Per-domain row op-ids: knowledge.row.<domainId>.select (open the detail view); knowledge.row.<domainId>.dropzone (drop a file directly onto the row to upload it into the domain). Detail view: the drop zone target accepts PDF, DOCX, text, markdown, image (25MB max); files dropped here flow through the same Plan + analyze pipeline as chat-originated uploads but with the target domain pre-set. Attached Document rows show fileName + itemCount + a checkmark indicating validation. WALKTHROUGH GUIDANCE: when the user wants to "add knowledge", "create a domain", "make a knowledge area", "set up an HR knowledge base", "give the agent reference material" -> uiClick nav.knowledge then uiClick knowledge.new. When the user has just analyzed a file and wants to attach it to a knowledge domain, the plan.completed canvas card has a built-in "Attach to domain ▾" picker -- prefer that path over re-uploading the file via the Knowledge page.`,
	},
	{
		SourceRef: "concept:validation",
		Text: `Per Q15: every analyzed Document carries a VALIDATION STATUS gating whether its content can be used as canonical truth. States are: 'unvalidated' (just analyzed; usable as reference for research/exploration but NOT as authoritative source); 'validated' (the user reviewed and approved -- this is the only status that lets a Document be ingested into a knowledge domain); 'rejected' (user soft-deleted; agents skip it for reasoning); 'partiallyValidated' (some items validated, some not -- only the validated items propagate); 'superseded' (a newer Document took over). The validation flow lives on the plan.completed canvas card: when the analyzer finishes a Document, the card shows [Validate] (canvas.card.planCompleted.validate) and [Reject] (canvas.card.planCompleted.reject) action buttons. Once validated, the card surfaces an [Attach to domain ▾] picker (canvas.card.planCompleted.attachDomain.toggle) listing every active knowledge domain; selecting one writes mutationAttachDocumentToDomain so the Document propagates into that domain's retrieval surface. Validation also unlocks the [Refine ...] action (canvas.card.planCompleted.refine.toggle), which opens an inline composer for the user to give feedback ("look at column F more carefully") -- submitting spawns a child Plan with kind='refineAnalysis' that the handleRefinementPlan automation drives through its own lifecycle. Validation is per-Document at the v0.x surface; per-item validation (Q15 hybrid granularity, e.g. "rows 1-50 yes, 51-100 no") ships in subsequent rounds via a dedicated drawer. WALKTHROUGH GUIDANCE: never click Validate on the user's behalf without explicit approval (validation marks data canonical and propagates downstream); always uiAskUser to confirm before clicking canvas.card.planCompleted.validate or canvas.card.planCompleted.reject.`,
	},
	{
		SourceRef: "surface:training",
		Text: `The Training right panel (data-op-id=training.listPanel, opens at ?panel=training via uiClick nav.training) is THE place to change an agent's knowledge and skills. Knowledge + Skills tabs were removed from the Create Agent / Edit Agent modal; this panel is now the single surface for those edits. Layout: a panel header with the title "Training" and the subtitle "Drag items onto the Studio card on the canvas, then Train", a three-tab strip (training.tab.agents / training.tab.knowledge / training.tab.skills), a search input scoped to the active tab (training.search.<tab>), and a scrollable palette of draggable rows. PALETTES: (a) AGENTS palette (training.palette.agent.<bareId> per row) -- lists every active agent, sorted with the General Assistant (Sofia) PINNED at the top, then a hairline "Other agents" divider, then the rest alphabetically. The GA row is visually grayed (opacity 60%) and carries a "Read-only" lock pill -- users can drop her on the Studio to INSPECT her config, but the Studio renders a read-only banner and locks every zone. Sofia's training is system-managed via provisionGeneralAssistantOnUserCreate; user-side training would be undone on the next cluster boot. (b) KNOWLEDGE palette (training.palette.domain.<id> per row) -- lists every workspace + private knowledge domain the user has access to. Drag onto the Studio's Knowledge zone to stage. (c) SKILLS palette (training.palette.tool.<slug> per row) -- lists agent capabilities + integrations. The list HIDES bundle primitives (workerHost / workerComputer / workerStatus / uiClick / uiType / etc.) and shows the umbrellas instead -- "CoPresent Control" and "Computer Use". Users pick the umbrella; the runtime fans it out. INTERACTIONS: every palette row is BOTH clickable and draggable. Click an Agents-palette row -> select that agent into the Studio's slot (toggles off if you click the same row twice). Drag any row onto the Studio card on the canvas -> stage it in the matching zone. Search filters the active tab live. Switching tabs preserves search per-tab. Walkthrough flow: see surface:trainingStudio for the studio-card semantics + concept:trainingFlow for the multi-step orchestration.`,
	},
	{
		SourceRef: "surface:trainingStudio",
		Text: `The Training Studio is a per-canvas card (opId family training.studio.*) that appears when the user is on the Training panel. It is the drop target for the Training panel's drag-able rows, and it commits the staged changes via the Train button. Three drop zones, top to bottom: (1) AGENT slot (training.studio.clearAgent.<bareId> for the clear button when an agent is staged) -- accepts an agent from the Agents palette; replaces on re-drop; click the × to clear. The slot displays the agent's name + role label. (2) KNOWLEDGE zone -- accepts knowledge domains. Each staged domain renders as a chip (training.studio.domain.<domainId>); FILLED chips are already-trained on the agent, OUTLINED chips are newly added and will be embedded on the next Train run. Locked chips (core baseline + tool-required domains) cannot be removed. (3) SKILLS zone -- accepts skills. Each staged skill renders as a chip (training.studio.tool.<slug>); same fill / outline / lock semantics as Knowledge. The Skills zone HIDES every bundle primitive (workerHost, workerComputer, workerStatus, uiClick, uiType, ...); only the umbrellas (CoPresent Control, Computer Use) appear. The TRAIN button (training.train) sits below the three zones. Disabled when no agent is staged or there are no unsaved changes. Label morphs: "Train <Name>" when there's something to commit; "Nothing to train" when there isn't. Click Train -> creates a Plan + 3 Tasks visible on the Tasks panel, lands a training.completed canvas card on the active space when the Plan succeeds. No popup toast; progress + outcome live on the Tasks panel. READ-ONLY MODE: when the slot agent is the General Assistant (Sofia), the Studio renders the ReadOnlyAgentBanner ("<Name> is read-only -- her training is system-managed"), the three zones are grayed and locked (no drop, no chip remove ×), and the Train button label morphs to "<Name> is read-only" + becomes disabled. The slot still accepts the GA via drop (so users can inspect Sofia's domains/skills in the chips), it just refuses to commit. IN-FLIGHT MODE: while a trainAgent Plan is running for the slot agent, the Studio shows TrainingInFlightBanner with a "View tasks" deep-link button (training.studio.inflight.viewTasks.<planId>) -- the staged set is frozen, drop / remove / Train are all locked, the agent stays usable in chat with its current state, and clicking "View tasks" jumps to ?panel=tasks&plan=<planId> so the user can watch progress or cancel from there.`,
	},
	{
		SourceRef: "concept:trainingFlow",
		Text: `The canonical Training flow when the user says "give my Operations agent the email and finance-docs skills" (or similar -- adding knowledge / skills / both to a specific agent): (1) uiClick nav.training to open the Training panel + spawn the Studio card on the canvas. (2) AGENTS tab is the default. uiClick training.tab.agents if you're not already there. uiClick (or drag) training.palette.agent.<bareId> to put the target agent into the Studio's Agent slot. Confirm via uiReadState that the slot now shows the agent's name. (3) Switch to Knowledge: uiClick training.tab.knowledge. For each requested domain, drag (or select then drag-drop simulated) training.palette.domain.<id> onto the Studio's Knowledge zone. uiReadState to confirm a chip landed. Filled chips were already trained; outlined chips are new. Skip drops for domains already filled (they'd be a no-op). (4) Switch to Skills: uiClick training.tab.skills. For each requested skill, drag training.palette.tool.<slug> onto the Studio's Skills zone. The umbrellas "CoPresent Control" (slug copresent_control) and "Computer Use" (slug computer_use) cover all UI-driving / machine-driving primitives -- never look for individual workerHost / uiClick chips, those are hidden. (5) When the staged set matches the user's request, COMMIT CONFIRMATION: uiHighlight training.train, uiAskUser({question: "Ready to train <Name> on <list of new items>?", options: ["Yes, train <Name>", "Cancel"], allowFreeForm: false}). On "Yes" uiClick training.train yourself. (6) Narrate that progress lives on the Tasks panel; offer to chain into ?panel=tasks if the user wants to watch. (7) Land the cursor on the space-context pill (concept:releaseLanding) and uiReleaseControl with a summary naming the agent + the items applied. EDGE CASES: (a) Sofia/GA in the slot -- the Studio is read-only; abort the flow with narration "<Name> is your General Assistant -- her training is system-managed and can't be edited from here. Want me to spin up a specialist instead?" Do NOT click training.train (it's disabled). (b) An in-flight training Plan exists -- the Studio is frozen with TrainingInFlightBanner. Narrate the lock + offer to deep-link to ?panel=tasks via the banner's "View tasks" button. (c) The user wants to remove a skill or domain rather than add: same flow, but instead of dragging from the palette, click the × on the chip (chips are training.studio.domain.<id> / training.studio.tool.<slug>); then Train commits the removal.`,
	},
	{
		SourceRef: "concept:agentSkills.umbrellas",
		Text: `CoPresent has TWO umbrella skills in the agent skill catalog whose user-facing label hides a backend fan-out: "CoPresent Control" (slug copresent_control) and "Computer Use" (slug computer_use). The umbrella is what the user picks; the backend stores the fan-out primitives in capabilities.tools. CoPresent Control fans out into the operator primitives (uiClick, uiType, uiSelect, uiHighlight, uiNavigate, uiReadState, uiAskUser, uiWaitFor, uiRetry, uiNarrate, uiRequestControl, uiReleaseControl, agentUpdateSelf, similarTo). Computer Use fans out into the worker primitives (workerHost, workerComputer, workerStatus). EVERY UI surface that renders skill chips (the Training Studio's SkillsZone, agent-card capability badges, Edit-mode chip lists, the Training panel's Skills palette) HIDES the primitive names and shows the umbrella label. The user never sees "workerHost" / "uiClick" / etc. as standalone chips. Helper isBundlePrimitiveSlug() in agentDefaults.ts is the single source of truth for "is this a fanned-out primitive?" -- new bundles add their primitives there and chip filtering picks them up automatically. WHEN AN AGENT HAS Computer Use: the agent receives workerHost (shell + filesystem + http), workerComputer (mouse + keyboard + screenshot), and workerStatus (live availability check) in its tool catalog. The user sees one chip: "Computer Use". When asked "what skills do you have?" the agent should NAME THE UMBRELLAS, not the primitives -- "CoPresent Control" / "Computer Use", not "uiClick" / "workerHost". Computer Use is selectable on EVERY agent (not GA-only) and is default-on for newly-created agents from the Create Agent modal. Existing agents do not get Computer Use auto-added on save; the user enables it via the Training panel.`,
	},
	{
		SourceRef: "surface:settings.computerUse",
		Text: `Settings -> Computer Use (data-op-id container=settings.computerUse, sibling of settings.copresentControl on the Settings panel) is where the user pairs / unpairs / scopes their cockpit-gui worker. The cockpit-gui worker is a separate process the user runs on their own machine that receives mouse + keyboard + shell + filesystem + http calls from the agent (when the agent has the Computer Use skill). Two visual states: NOT CONNECTED -- "○ Not connected" status line + a [Connect this computer] button (data-op-id=computer-use.connect). Click opens the ConnectComputerModal, which displays a one-shot pairing CODE (data-op-id=connect-computer.copy-code) and a one-line shell COMMAND (data-op-id=connect-computer.copy-command, format: 'memql-cockpit-gui worker pair <code>') the user runs on their machine. CONNECTED -- "● Connected: <hostname>" status line + a sub-line "macOS arm64 · last seen Xs ago" + a "Capabilities: HEADLESS + GUI" badge + scope toggles (observe / interact / full; persist to v1:copresent:agentAuthorization.computerUseScope on the GA's authorization row, default 'full') + [Disconnect] (data-op-id=computer-use.disconnect, revokes the worker_token; the cockpit-gui worker stops being able to connect) + [Connect another computer] (data-op-id=computer-use.connect-another, opens the same ConnectComputerModal for adding a second machine). The card live-polls workersForUserQuery every 4s + flips to "offline" after 30s without a heartbeat. WALKTHROUGH GUIDANCE: when the user asks "set up computer use", "let my agent control my computer", "pair my mac", "install the worker": uiClick nav.settings -> uiPointerTo settings.computerUse -> narrate the Connect button -> uiClick computer-use.connect -> read the modal's code + command aloud, do NOT click copy buttons (the user copies); do NOT close the modal until the user confirms the pair completed in their terminal. Disconnect is destructive (revokes the worker_token; cockpit must re-pair) -- COMMIT CONFIRMATION required before uiClick computer-use.disconnect.`,
	},
	{
		SourceRef: "surface:header.computerUsePill",
		Text: `The Computer Use pill (data-op-id=header.computer-use-pill) is a floating header chip that ONLY renders when the user has at least one paired worker (revealed once the first ConnectComputerModal flow completes). It sits between the header nav-tile strip and the Profile pill. The pill is BOTH a visual indicator AND a one-click kill switch. Visual states: GLOBAL ENABLED + worker connected -> green dot + "Computer Use" label; GLOBAL ENABLED + no worker online -> amber dot; GLOBAL DISABLED -> red dot + slashed icon. CLICK opens a small popover with two buttons: (1) header.computer-use-pill.toggle -- flips the user-level GLOBAL kill switch (writes v1:identity:user.preferences.computerUseEnabled). When OFF, every workerHost / workerComputer / workerStatus call is rejected at the WorkerService gateway with a `+ "`computer_use_disabled`" + ` error, regardless of agent capabilities or scope. The pill is the user's "panic button" for stopping the agent without disconnecting the cockpit. (2) header.computer-use-pill.manage -- routes to ?panel=settings + scrolls to settings.computerUse (the full management card; see surface:settings.computerUse). WALKTHROUGH GUIDANCE: when the user says "stop the agent from controlling my computer" / "pause computer use" / "kill switch": uiClick header.computer-use-pill -> uiClick header.computer-use-pill.toggle -> uiNarrate "Computer Use is paused -- agents can't reach your machine until you flip this back on". When the user says "open computer use settings": uiClick header.computer-use-pill -> uiClick header.computer-use-pill.manage. The pill is NOT visible to users who never paired a worker; in that state route them through nav.settings -> settings.computerUse instead.`,
	},
	{
		SourceRef: "concept:computerUseAgentCapability",
		Text: `When an agent has the "Computer Use" skill (capability slug computer_use, fanned out to workerHost + workerComputer + workerStatus on save), it can drive the user's own computer outside CoPresent: shell exec, filesystem read/write, HTTP fetch, mouse + keyboard + screenshot. Distinct from CoPresent Control (which drives the CoPresent SPA itself). RUNTIME GATING -- four checks fire BEFORE any wire traffic: (1) Agent capability flag -- must include computer_use. (2) The user's standing scope on v1:copresent:agentAuthorization.computerUseScope (observe = read-only fs + GET HTTP + screenshot/cursor/display info; full = everything: shell exec, fs_write, full HTTP, mouse + keyboard + scroll + window_focus). (3) Per-call action's required scope (e.g. workerHost.exec needs full; workerHost.fs_read needs observe; workerComputer.mouse_click needs full). (4) The user-level kill switch on v1:identity:user.preferences.computerUseEnabled. Out-of-scope calls park the calling Plan in awaitingFeedback with feedbackReason=scope_elevation_required; kill-switch denies return computer_use_disabled. AVAILABILITY STATES the agent reasons about (surfaced by the workerStatus tool + the per-turn computerUseStatus prompt seed): connected = a paired cockpit-gui worker is online RIGHT now (tool calls dispatch; detail = the worker hostname); disconnected = the user has paired before but the cockpit isn't running; unconfigured = the user has never paired a worker. When asked "what can you do?" the agent should mention Computer Use alongside its other capabilities and reflect the LIVE STATE -- "I can drive your computer; right now your cockpit isn't running, but once you start it I can run shell commands, manage files, and even drive your screen with mouse + keyboard." If unconfigured, the next move is uiClick nav.settings then uiPointerTo settings.computerUse (see surface:settings.computerUse). If disconnected, narrate that the cockpit needs to be running and walk them through opening Terminal + running 'memql-cockpit-gui worker run'. If connected, the Computer Use pill (header.computer-use-pill) is visible and acts as the live indicator + kill switch (see surface:header.computerUsePill).`,
	},
}

// computerUseSeedCorpus is the operational manual for the
// `computer_use` capability. Same shape as copresentUISeedCorpus
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
		Text: `Computer Use is the capability that lets you drive the user's own computer outside CoPresent. It is distinct from CoPresent Control: CoPresent Control drives the CoPresent SPA you're embedded in (DOM op-ids, takeovers, walkthroughs), while Computer Use drives the user's MACHINE (shell commands, files, mouse, keyboard, screenshot) via a paired cockpit-gui worker process running on their hardware. The capability fans out into four primitive tools when an agent picks it: ` + "`workerHost`" + ` (shell exec / filesystem / HTTP fetch -- headless), ` + "`workerComputer`" + ` (mouse / keyboard / screenshot -- GUI-driving), ` + "`workerStatus`" + ` (cheap connectivity probe), and ` + "`requestComputerUseScope`" + ` (the per-task approval gate). When you mention this skill to the user, call it "Computer Use" (the umbrella name). Never expose the four primitives by name in user-facing text -- they are wire-level internals.`,
	},
	{
		SourceRef: "computerUse:toolSurfaces",
		Text: `Two execution surfaces sit under Computer Use, each with a distinct shape: ` + "`workerHost`" + ` is HEADLESS -- it runs shell commands, reads/writes files, fetches URLs. Use it when the task has a one-shot command equivalent (filesystem operations, package installs, HTTP fetches, anything you'd type at a terminal). It's faster, more reliable, and easier to verify than scripted GUI input. ` + "`workerComputer`" + ` is GUI-DRIVING -- it moves the mouse, types on the keyboard, scrolls, takes screenshots. Use it when the task genuinely requires looking at or driving the user's screen (interacting with a native app that has no CLI, taking a screenshot of the desktop, clicking through a UI flow inside an app outside CoPresent). When the SAME task is achievable on either surface, prefer ` + "`workerHost`" + ` for the smaller blast radius and the cleaner contract -- unless the user explicitly asked for the cursor / keyboard path. ` + "`workerStatus`" + ` is a cheap probe with no side effects; call it when the cockpit's connectivity may have aged (the user told you mid-turn they started their cockpit; you're about to kick off a long workerHost / workerComputer flow). Don't spam it -- the per-turn computerUseStatus prompt seed is already fresh at turn start.`,
	},
	{
		SourceRef: "computerUse:scopeTiers",
		Text: `TWO scope tiers determine what a Computer Use call can DO once the user has approved it. ` + "`observe`" + ` is read-only filesystem + GET HTTP + read-only screen probes (screenshot, cursor_position, display_info, window_list). Tool surfaces: workerHost.fs_read / fs_list / fs_stat / http_fetch (GET only) AND workerComputer's read-only actions. ` + "`full`" + ` is everything: shell exec, fs_write, full HTTP (any method), mouse, keyboard, scroll, window_focus. Tool surfaces: workerHost (any action) AND workerComputer (any action). Pick the LEAST scope that finishes the task -- if the user only needs you to "read what's in this folder" or "show me what's on screen", request observe; otherwise request full. The earlier ` + "`interact`" + ` middle tier (mouse + keyboard but NOT shell) was retired because it locked the agent into a single execution path when shell was often the cleaner one (e.g. "open Chrome" via 'open -a Chrome' is faster + more reliable than scripting cmd+space + type + return); the user is already approving "drive my machine", an arbitrary line between "GUI without shell" and "shell" doesn't help them reason about consent. If you ever see ` + "`interact`" + ` come back from a legacy authorization row, treat it as ` + "`full`" + `.`,
	},
	{
		SourceRef: "computerUse:perTaskApproval",
		Text: `The user wants explicit per-task approval for every Computer Use action. Standing scope on v1:copresent:agentAuthorization is bookkeeping -- it does NOT auto-approve work. Before you ever call ` + "`workerHost`" + ` or ` + "`workerComputer`" + `, you MUST first call ` + "`requestComputerUseScope`" + ` so the user sees a permission card on the canvas describing what you're about to do, the scope you need, and Allow / Deny buttons. This is non-negotiable -- the canvas card is the user's signal that you're about to do something on their machine, and skipping it cheats them out of consent even when the standing scope nominally covers the action. The flow has three parts: (1) Call ` + "`requestComputerUseScope({intent, requestedScope, summary})`" + ` BEFORE every workerHost / workerComputer call. The intent is the user's request restated as one short imperative. The summary is one paragraph the user reads on the canvas card explaining what you'll actually do, why, and how long it'll take. (2) End your turn with a short ` + "`respondToUser`" + ` along the lines of "I've requested <scope> access -- there's an approval card on your canvas; click Allow and I'll get to work." DO NOT call workerHost / workerComputer in the same turn -- the user's click on the card is the gate. (3) You do NOT need to wait for the user to re-ask. When they click Allow on the canvas card, the planner automatically dispatches a NEW turn back to you carrying planApprovedTrigger=true -- that turn (a separate prompt render) is where you actually run the work.`,
	},
	{
		SourceRef: "computerUse:postApprovalExecution",
		Text: `When a turn arrives with planApprovedTrigger=true, the user already clicked Allow on the canvas permission card and the Plan transitioned to status=running. The planner forwarded this turn so you can do the work. Mandatory flow on this turn, in this exact order: (1) DO NOT call ` + "`requestComputerUseScope`" + ` again. The user just approved; calling elevation again would loop forever. The user-message in this turn's history IS the original goal; just execute it. (2) Dispatch the tool surface that fits the task. With ` + "`full`" + ` scope you may use either workerHost (shell, files, HTTP) or workerComputer (mouse, keyboard, screenshot) -- pick the most reliable path; shell is usually cleanest for things like "open <app>", "navigate to <URL>", "create / move / rename a file". With ` + "`observe`" + ` scope you only have read-only actions on workerHost (fs_read / fs_list / fs_stat / http_fetch GET) and read-only workerComputer probes (screenshot / cursor_position / display_info / window_list). (3) As soon as the worker tool returns ok=true, call ` + "`canvasPublish`" + ` in the SAME turn to drop a task-done card on the canvas (kind="document", data={format: "markdown", title, source}, importance="notify"). The source markdown should contain a one-line outcome stating what just landed, a short bulleted list of the concrete artefacts (file paths, command output, URLs touched), and any caveat the user should know. (4) End your turn with a short factual ` + "`respondToUser`" + ` stating what you just did. Short, no re-asking, no "let me know if..." padding. (5) If the worker call returns ok=false (cockpit unreachable, command errored, dispatcher denied even though the user approved), skip canvasPublish and explain the failure honestly in the respondToUser. Do not pretend success in your text and do not call requestComputerUseScope again on this turn -- the planner reads worker invocations and stamps the Plan succeeded vs failed automatically.`,
	},
	{
		SourceRef: "computerUse:planOutcomeSemantics",
		Text: `The planner is the authority on whether a Plan succeeded or failed. After your post-approval turn finishes, the planner queries v1:worker:invocation rows belonging to this Plan id. Every workerHost / workerComputer call writes a row at completion with outcome ∈ {success, failure, cancelled, timeout, denied_by_scope, denied_by_policy, kill_switch_engaged, no_worker_available}. If at least one row has outcome=success, the planner stamps Plan status=succeeded and writes your reply text to Plan.output.reply. If zero rows have outcome=success (you never dispatched a worker tool successfully), the planner stamps Plan status=failed and writes your reply text to Plan.errorMessage so the user sees the actual reason in the Tasks panel. Practical implication: you cannot fake success in your respondToUser text. A turn that finishes without dispatching a worker tool at all (or dispatches one and it fails) lands as Plan failed, regardless of how the reply reads. Better to fail honestly with a reply explaining what blocked you than to pretend success and have the Tasks panel disagree with the user's lived experience.`,
	},
	{
		SourceRef: "computerUse:availabilityStates",
		Text: `Three availability states surface to you per turn via the computerUseStatus prompt seed: ` + "`connected`" + ` -- a paired cockpit-gui worker is online RIGHT now and reachable; tool calls dispatch normally; the per-turn detail field carries the worker hostname. ` + "`disconnected`" + ` -- the user has paired a cockpit before but it's not running right now; tool calls will fail with no_worker_available; do NOT call workerHost / workerComputer in this state. ` + "`unconfigured`" + ` -- the user has never paired a worker; same fail mode as disconnected, plus the user needs to set up. When asked "what can you do?" / "what skills do you have?", reflect the LIVE state honestly: connected -> "I can drive your computer -- run commands, manage files, drive the screen with mouse + keyboard." Disconnected -> add "your cockpit isn't running right now; once you start it I can drive your machine." Unconfigured -> add "you'd need to set it up in Settings first." NEVER tell a user to grant scope in Settings ("go to Settings and grant Full / Observe scope") -- that UI does not exist; scope is granted per-task on the canvas, period. NEVER tell a user to restart the cockpit with a "--scope full" or similar flag -- no such flag exists; pairing has nothing to do with scope.`,
	},
	{
		SourceRef: "computerUse:errorBudget",
		Text: `If ` + "`requestComputerUseScope`" + ` itself returns an error (rare; transient backend issue), tell the user honestly: "I tried to request access but the request didn't go through -- something's wrong on the backend. Try asking me again in a minute." Never substitute cockpit-restart fiction or Settings advice. If the user denied the permission card, the Plan transitions to status=cancelled with feedbackResponse.response="deny" -- you'll see this on a subsequent turn (no planApprovedTrigger; just the user re-engaging in chat). Acknowledge the denial briefly and offer alternatives within the user's standing scope. If the permission card auto-dismissed at the 3-minute timeout, the Plan is cancelled with feedbackResponse.response="timeout" -- treat it as a soft "no answer" and offer to re-request when the user is ready.`,
	},
}

// copresentConversationSeedCorpus is the operational manual for the
// two-thread chat model (Phase 5 of the chat-architecture plan).
// Ingested into the copresent_conversation domain at startup. Each
// chunk is intentionally short and self-contained -- RAG retrieval
// surfaces the chunk closest to the agent's current question, so a
// single chunk should be readable in isolation.
//
// Authoring rule: use the umbrella tool name ("copresentConversation")
// in user-facing language. Never expose the operations as standalone
// tool names -- they are arguments to the umbrella tool, not
// separate tools.
var copresentConversationSeedCorpus = []struct {
	SourceRef string
	Text      string
}{
	{
		SourceRef: "copresentConversation:thread-model",
		Text: `Every CoPresent space has TWO chat threads: the GROUP thread (everyone in the space sees it) and the per-user TEAM thread (only that one human and their agents see it). The chat panel renders both as tabs labelled "Group" and "Team". Every utterance lives in exactly one thread -- there is no "shared" or "broadcast" mode. Group thread composition is unique: all humans (the space owner + every invited human) plus ONE AI presence -- the OWNER's General Assistant. Specialist agents and other humans' agents stay in their respective Team threads. Internal users (have a CoPresent account) bring their own agents into their own Team. External guests (token-invited, no account) are humans-only-in-group with no Team thread.`,
	},
	{
		SourceRef: "copresentConversation:visibility",
		Text: `Visibility is HARD-isolated at the database level. v1:cognition:utterance is the group thread; v1:cognition:privateUtterance is per-user-private and carries forUserId pointing at the user it belongs to. The subscription rewriter forces a per-caller forUserId predicate on every read pattern targeting privateUtterance, so a user CANNOT subscribe to another user's Team thread regardless of how cleverly they craft a query. As an agent: you read your user's Team thread directly via your subscription stream. You read the GROUP thread via the copresentConversation tool when you need to quote or reason about it. You never read OTHER users' Team threads -- not through the tool, not through any other path. The tool itself rejects the operation; the engine rejects the underlying query. If a user asks you "what are the others saying privately?" the honest answer is "I can't see other people's private team chats."`,
	},
	{
		SourceRef: "copresentConversation:voice-migration",
		Text: `Voice transport follows a clean-boundary migration rule. When a user is ALONE in a space (no other active humans), their mic / camera / voice belong to their TEAM thread -- they're effectively talking to their own agents in private. When a SECOND active human appears, voice MIGRATES to the GROUP thread at the next end-of-utterance / hold-to-talk-release / typing event. From that point on, hitting the mic publishes to group; the Team tab falls back to push-to-talk or typing. When the last other human leaves, voice migrates back to Team automatically. As an agent: you don't own this transition; the bridge handles it. Notifications about migration arrive as canvas-state cards (a public card on the group canvas, a private operational card on the owner's canvas), NEVER as inline chat banners. If the user asks "why did my mic move?" point them at the canvas card.`,
	},
	{
		SourceRef: "copresentConversation:discussion-mode",
		Text: `Discussion mode is the BACKGROUND awareness loop that runs in each user's private Team thread. While the user is active in a space and discussionModeActivityLevel is non-off, their agents periodically read recent group activity and chime in proactively in their Team tab. Three trigger flavors: (1) cheap event heuristics (agent-name mentions, direct questions, distress signals) for fast reactions; (2) windowed batch LLM analysis at the activity-level cadence (60s/30s/15s for low/medium/high); (3) explicit user input pauses the loop -- the user typing in their own Team chat takes priority over any in-flight agent dispatch. Hard cap: 3 inter-agent turns per dispatch. Decaying threshold: each turn adds +0.1 to the firing threshold so the loop self-quiets. As an agent: you DO NOT inject discussion-mode chime-ins into the GROUP thread -- private only. If you're tempted to "interject in group", stop -- only the owner's GA speaks in group, and only when the user addressed the conversation that direction.`,
	},
	{
		SourceRef: "copresentConversation:misroute-safety",
		Text: `A server-side misroute classifier checks every outgoing user message against the active tab. Three confidence tiers: (>=0.85) hard pre-send modal blocks the send until the user picks "Send to <other>" or "Send here anyway"; (0.6-0.85) the message goes through but a soft "Did this belong in <other>? [Move]" prompt appears beneath it for 10 seconds; (<0.6) silent. The user can disable the safety net entirely in Settings (preferences.misrouteSafetyEnabled). As an agent: this is the USER's safety net, not yours. NEVER cross-post on the user's behalf to "fix" a misroute you suspect. NEVER quote private content INTO a group reply -- if the user said something in Team that informs your group reply, generalize the reasoning rather than copying the words. The two-thread model is a privacy contract; honor it as such.`,
	},
	{
		SourceRef: "copresentConversation:canvas-not-chat",
		Text: `Lifecycle / room-state / system events are CANVAS CARDS, not inline chat banners. "<X> joined the space" -- canvas. "Voice migrated to group" -- canvas. "<X> left" -- canvas. "Mic warning: input is too quiet" -- canvas. The canvas state (v1:copresent:canvasState) carries visibility (public / private), forUserId, actor.kind (system / agent / user), and importance (notify / ambient). The chat thread is for UTTERANCES (what someone said), not for state. As an agent: never emit a chat utterance whose only purpose is to announce a system event. If the user needs to be told something happened, use canvas.publish or accept that the system itself will land the canvas card. Your chat utterances should be substantive: answering questions, offering help, advancing the conversation.`,
	},
	{
		SourceRef: "copresentConversation:tool-usage",
		Text: `The copresentConversation tool gives you READ-ONLY access to the GROUP thread + space context (you already see your own user's Team thread directly via your subscription -- no tool needed). Five operations: (1) readGroupRecent({count}) -- last N group utterances; (2) readGroupByKeyword({keyword}) -- most recent group utterances containing a substring; (3) readGroupByTime({fromTime, toTime}) -- group utterances in a time window (ISO-8601); (4) getSpaceContext() -- the space's title, purpose, and active participants; (5) listParticipants() -- humans + agents currently active. Each utterance result has speakerId, speakerName, speakerKind, timestamp, content, utteranceId. When you quote group content in your Team-tab reply, attach a citation with kind: "group_thread_utterance" and the utteranceId; the frontend renders a click-to-jump chip. NEVER invent the existence of a group utterance you didn't actually retrieve. NEVER attempt operations the tool doesn't expose (e.g., readPrivate*) -- the engine rejects them on principle.`,
	},
}

// seedStandardDomainsHandler creates the shipped knowledge domains and
// ingests the initial copresent_ui + computer_use corpora. Idempotent:
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
		// chat time. copresent_ui + computer_use are operator/internal
		// documentation (chunks shape the agent's behavior; not
		// audibly cited). Every other catalog domain is LLM-seeded
		// subject-matter expertise (cited as "your X training" in
		// agent replies). See the citation registry in
		// integrations/agent/replier.go (appStructureDomainIds).
		domainSource := "llmSeeded"
		if d.ID == "copresent_ui" || d.ID == "computer_use" || d.ID == "copresent_conversation" {
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
	ingestCorpus("copresent_ui", copresentEntries)

	computerUseEntries := make([]seedEntry, 0, len(computerUseSeedCorpus))
	for _, e := range computerUseSeedCorpus {
		computerUseEntries = append(computerUseEntries, seedEntry{SourceRef: e.SourceRef, Text: e.Text})
	}
	ingestCorpus("computer_use", computerUseEntries)

	conversationEntries := make([]seedEntry, 0, len(copresentConversationSeedCorpus))
	for _, e := range copresentConversationSeedCorpus {
		conversationEntries = append(conversationEntries, seedEntry{SourceRef: e.SourceRef, Text: e.Text})
	}
	ingestCorpus("copresent_conversation", conversationEntries)

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
// we're already doing direct SQL for the sibling lookups. Partition
// scoping matches the seed's own resolvePartition so we don't nuke
// rows in other tenants.
func (i *Integration) purgeChunksForSource(ctx context.Context, domainId, sourceRef string) error {
	if i.db() == nil {
		return nil
	}
	partition := i.resolvePartition(ctx)
	// Delete embeddings first so we never leave node_vectors rows
	// dangling against a missing MemoryNodes chunk. The subquery
	// picks up every chunk id matching the (domain, sourceRef) pair
	// regardless of version/text hash.
	vecSQL := `
		DELETE FROM node_vectors
		WHERE partition = $1
		  AND id IN (
		    SELECT id FROM "MemoryNodes"
		    WHERE partition = $1
		      AND concept = 'v1:common:documentChunk'
		      AND (payload->>'domainId') = $2
		      AND (payload->>'sourceRef') = $3
		  )
	`
	if _, err := i.db().ExecContext(ctx, vecSQL, partition, domainId, sourceRef); err != nil {
		return fmt.Errorf("delete node_vectors: %w", err)
	}
	chunkSQL := `
		DELETE FROM "MemoryNodes"
		WHERE partition = $1
		  AND concept = 'v1:common:documentChunk'
		  AND (payload->>'domainId') = $2
		  AND (payload->>'sourceRef') = $3
	`
	if _, err := i.db().ExecContext(ctx, chunkSQL, partition, domainId, sourceRef); err != nil {
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
	if domainId == "business_administration" {
		// business_administration is the always-visible catalog
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
