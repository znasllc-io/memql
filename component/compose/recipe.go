package compose

type RenderRecipe struct {
	Name           string     `json:"name"`
	Format         Format     `json:"format"`
	OutputKind     string     `json:"outputKind,omitempty"`
	DeployableKind string     `json:"deployableKind,omitempty"`
	Draft          Draft      `json:"draft"`
	Provenance     Provenance `json:"provenance"`
}

func RenderRecipeBytes(r RenderRecipe) (Result, error) {
	if r.OutputKind == "email_template" {
		return RenderEmailTemplate(r.Draft.Body)
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
