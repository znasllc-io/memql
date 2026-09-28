package bundle

// Area is one section of the published docs: the `area` front-matter value, the
// sidebar title and its place in the order.
type Area struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Order int    `json:"order"`
}

// Areas is the ONE table of public documentation areas, in sidebar order.
// Before it, the bundler and the site each kept a list and the two disagreed
// on the order (build before operate in one, after it in the other). The
// manifest now carries this table, so the site reads the order instead of
// keeping its own.
//
// A public page whose `area` is not here is a violation, not an "other"
// group: the site has nowhere to put it. Adding an area means adding it here
// and to the closed set in docs/DOCS_STANDARD.md section 2 and
// docs_front_matter_test.go, in the same change.
var Areas = []Area{
	{ID: "overview", Title: "Overview", Order: 0},
	{ID: "concepts", Title: "Concepts", Order: 1},
	{ID: "language", Title: "Language", Order: 2},
	{ID: "ai", Title: "AI", Order: 3},
	{ID: "operate", Title: "Operate", Order: 4},
	{ID: "build", Title: "Build", Order: 5},
	{ID: "cockpit", Title: "Cockpit", Order: 6},
	{ID: "reference", Title: "Reference", Order: 7},
}

// areaByID returns the table entry for id.
func areaByID(id string) (Area, bool) {
	for _, a := range Areas {
		if a.ID == id {
			return a, true
		}
	}
	return Area{}, false
}
