package auth

// MayManageFleet reserves machine enrollment and Fleet management for the
// cluster's owner and developer roles. A grant or a custom role's rank cannot
// turn model inference access into authority over a person's machines.
func MayManageFleet(role Role) bool {
	slug := normalizeSlug(string(role))
	return (slug == string(RoleOwner) || slug == string(RoleDeveloper)) &&
		RoleAccountScope(role) == "" && RoleRank(Role(slug)) > 0
}
