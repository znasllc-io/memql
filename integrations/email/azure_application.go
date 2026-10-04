package email

// This public OAuth application identifier belongs to MemQL's multi-tenant
// Entra registration. It is installation metadata, not a credential. Every
// installation uses its own user consent and organization-owned resources;
// never substitute Azure CLI's client ID or another vendor's application.
const defaultAzureApplicationID = "6f3f4b16-dd6b-41b3-a72d-6fc277bd5514"
