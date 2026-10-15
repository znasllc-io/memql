package compose

type RenderRecipe struct {
	Name           string     `json:"name"`
	Format         Format     `json:"format"`
	OutputKind     string     `json:"outputKind,omitempty"`
	DeployableKind string     `json:"deployableKind,omitempty"`
	Draft          Draft      `json:"draft"`
	Provenance     Provenance `json:"provenance"`
	// Blank says this recipe renders its format's empty document, whatever the
	// draft holds. It is ON THE RECIPE, and not a flag the first run kept to
	// itself, because the recipe is what a resumed run restores: the snapshot
	// saved before the file was filed is all a sibling replica has, and without
	// this it would render a blank request as an ordinary document with front
	// matter and a heading. omitempty keeps every other recipe's JSON, and the
	// source packages built from it, byte for byte what it was.
	Blank bool `json:"blank,omitempty"`
}

func RenderRecipeBytes(r RenderRecipe) (Result, error) {
	if r.Blank {
		return RenderBlank(r.Format, r.Name)
	}
	if r.OutputKind == "email_template" {
		return RenderEmailFile(r.Draft.Body)
	}
	if r.DeployableKind != "" {
		kind, err := ParseDeployableKind(r.DeployableKind)
		if err != nil {
			return Result{}, err
		}
		page, err := Render(FormatHTML, r.Draft, r.Provenance)
		if err != nil {
			return Result{}, err
		}
		return BuildPackageSource(PackageSource{Name: r.Name, Deployables: []Deployable{{Name: r.Name, Kind: kind, Files: []DeployableFile{{Path: "index.html", Body: page.Bytes}}}}}, r.Provenance)
	}
	return Render(r.Format, r.Draft, r.Provenance)
}
