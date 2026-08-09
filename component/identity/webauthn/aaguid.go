package webauthn

import "strings"

// AAGUID -> human authenticator name (memql#3409).
//
// The AAGUID is the authenticator's make/model, and resolving it is what
// turns a list of identical-looking rows into "the YubiKey on my keyring"
// and "the passkey in iCloud". That is the entire purpose of this file,
// and it bounds what the file may be used for:
//
// DISPLAY ONLY. NEVER AUTHORIZATION. The registration ceremony accepts
// `none` attestation, under which the AAGUID is SELF-ASSERTED -- an
// authenticator can claim to be a YubiKey and nothing in the ceremony
// contradicts it. A policy that admitted or refused a credential on the
// strength of this value would be trusting the thing it is trying to
// verify. The concept's own field doc says the same; this comment is
// here so the next reader finds the warning at the table rather than
// only at the schema.
//
// Two more properties that follow from "display only":
//
//   - An UNKNOWN aaguid is not an error and must never be rendered as
//     one. The table is a convenience snapshot of a public dataset that
//     grows every time a vendor ships a model, so "not in the table"
//     means "newer than this table", not "suspicious". Callers render
//     the empty string as nothing at all.
//   - An EMPTY or all-zero aaguid is the normal, correct report from a
//     platform authenticator that deliberately withholds its model.
//     credential.go normalises the all-zero form to "" at registration,
//     so both arrive here as "".
//
// The names below are the vendor-facing product names from the public
// passkey-authenticator-aaguid dataset. The list is deliberately partial:
// the well-known platform providers, the mainstream password managers,
// and the common hardware keys. Adding an entry is a one-line change and
// carries no risk precisely because nothing branches on the result.
var aaguidNames = map[string]string{
	// --- Platform / OS-provided ---------------------------------------
	"fbfc3007-154e-4ecc-8c0b-6e020557d7bd": "iCloud Keychain",
	"dd4ec289-e01d-41c9-bb89-70fa845d4bf2": "iCloud Keychain (managed)",
	"adce0002-35bc-c60a-648b-0b25f1f05503": "Chrome on Mac",
	"ea9b8d66-4d01-1d21-3ce4-b6b48cb575d4": "Google Password Manager",
	"08987058-cadc-4b81-b6e1-30de50dcbe96": "Windows Hello (hardware)",
	"9ddd1817-af5a-4672-a2b9-3e3dd95000a9": "Windows Hello (software)",
	"6028b017-b1d4-4c02-b4b3-afcdafc96bb2": "Windows Hello (VBS)",
	"53414d53-554e-4700-0000-000000000000": "Samsung Pass",

	// --- Password managers --------------------------------------------
	"bada5566-a7aa-401f-bd96-45619a55120d": "1Password",
	"d548826e-79b4-db40-a3d8-11116f7e8349": "Bitwarden",
	"531126d6-e717-415c-9320-3d9aa6981239": "Dashlane",
	"b84e4048-15dc-4dd0-8640-f4f60813c8af": "NordPass",
	"0ea242b4-43c4-4a1b-8b17-dd6d0b6baec6": "Keeper",
	"f3809540-7f14-49c1-a8b3-8f813b225541": "Enpass",
	"b78a0a55-6ef8-d246-a042-ba0f6d55050c": "LastPass",
	"22248c4c-7a12-46e2-9a41-44291b373a4d": "LastPass",
	"39a5647e-1853-446c-a1f6-a79bae9f5bc7": "IDmelon",

	// --- Hardware security keys ---------------------------------------
	"cb69481e-8ff7-4039-93ec-0a2729a154a8": "YubiKey 5 Series",
	"ee882879-721c-4913-9775-3dfcce97072a": "YubiKey 5 Series",
	"de1e552d-db1d-4423-a619-566b625cdc84": "YubiKey 5 Series",
	"a25342c0-3cdc-4414-8e46-f4807fca511c": "YubiKey 5 Series",
	"fa2b99dc-9e39-4257-8f92-4a30d23c4118": "YubiKey 5 Series (NFC)",
	"2fc0579f-8113-47ea-b116-bb5a8db9202a": "YubiKey 5 Series (NFC)",
	"73bb0cd4-e502-49b8-9c6f-b59445bf720b": "YubiKey 5 FIPS Series",
	"c1f9a0bc-1dd2-404a-b27f-8e29047a43fd": "YubiKey 5 FIPS Series (NFC)",
	"d8522d9f-575b-4866-88a9-ba99fa02f35b": "YubiKey Bio Series",
	"b92c3f9a-c014-4056-887f-140a2501163b": "Security Key by Yubico",
	"f8a011f3-8c0a-4d15-8006-17111f9edc7d": "Security Key by Yubico",
	"6d44ba9b-f6ec-2e49-b930-0c8fe920cb73": "Security Key by Yubico (NFC)",
	"149a2021-8ef6-4133-96b8-81f8d5b7f1f5": "Security Key by Yubico (NFC)",
	"a4e9fc6d-4cbe-4758-b8ba-37598bb5bbaa": "Security Key NFC by Yubico",
	"b6ede29c-3772-412c-8a78-539c1f4c62d2": "Feitian BioPass FIDO2",
	"9f77e279-a6e2-4d58-b700-31e5943c6a98": "Hyper FIDO Pro",
}

// AuthenticatorName resolves an AAGUID to a human make/model, or "" when
// the value is absent or not in the table.
//
// The empty return is the interesting case and the caller must handle it
// as ordinary: a platform authenticator that withholds its model and a
// key model newer than this table both land there, and neither is a
// problem worth showing a user.
func AuthenticatorName(aaguid string) string {
	key := strings.ToLower(strings.TrimSpace(aaguid))
	if key == "" || key == zeroAAGUID {
		return ""
	}
	return aaguidNames[key]
}
