package accountexport

import (
	"fmt"
	"strings"
	"time"
)

// renderReadme writes README.txt in plain words, from the manifest and the
// report and nothing else, so it says only what the file can back up.
func renderReadme(stem string, m Manifest, r Report) string {
	var b strings.Builder
	p := func(format string, a ...any) { fmt.Fprintf(&b, format+"\n", a...) }

	which := "an account"
	if m.Account.Primary {
		which = "the primary account"
	}
	p("This folder holds %s of a MemQL cluster: %s (%s).", which, m.Account.Name, m.Account.ID)
	p("")
	p("It was taken from %s at %s, by engine %s, as export %s.",
		m.Source.Domain, m.CreatedAt.UTC().Format(time.RFC3339), m.EngineVersion, m.ExportID)
	p("Every line in it comes from that one instant. File format version: %d.", m.FormatVersion)
	p("")

	p("WHAT IS IN IT")
	p("")
	if len(r.Kinds) == 0 {
		p("  No rows.")
	}
	for _, k := range r.Kinds {
		p("  %-40s %s, %s", k.Concept, count(k.WrittenRows, "row"), count(k.WrittenVersions, "version"))
	}
	p("")
	p("  Bytes: %s, %s.", count(r.Bytes.Objects, "object"), count(r.Bytes.Bytes, "byte"))
	if r.Bytes.Missing > 0 {
		p("  Missing at the source before this file was taken: %s, each named in bytes.jsonl.", count(r.Bytes.Missing, "object"))
	}
	for _, mc := range r.Members {
		p("  %s: %s.", mc.Name, count(mc.Lines, "line"))
	}
	p("")

	p("WHAT IS LEFT OUT")
	p("")
	if len(r.LeftOut) == 0 && len(r.InNoAccount) == 0 && len(r.ScrubbedFields) == 0 && r.Bytes.NotCarriedObjects == 0 {
		p("  Nothing the report counts.")
	}
	for _, l := range r.LeftOut {
		p("  %-40s %s stayed on the cluster (%s: %s).", l.Concept, count(l.Rows, "row"), l.Reason, leftOutWords[l.Reason])
	}
	for _, k := range r.InNoAccount {
		p("  %-40s %s in no account, which belong to nobody.", k.Concept, count(k.Rows, "row"))
	}
	for _, f := range r.ScrubbedFields {
		p("  %s.%s is a secret; its value was removed from every version.", f.Concept, f.Field)
	}
	if r.Bytes.NotCarriedObjects > 0 {
		p("  Counted, not carried (only older versions or package snapshots point at them): %s, %s where the size is known, %d of unknown size.",
			count(r.Bytes.NotCarriedObjects, "object"), count(r.Bytes.NotCarriedBytes, "byte"), r.Bytes.NotCarriedUnknownSize)
	}
	if len(r.Rebuilt) > 0 {
		p("  Rebuilt on the cluster that takes this file, from what is in it: %s.", strings.Join(r.Rebuilt, ", "))
	}
	p("")

	p("CHECKING IT")
	p("")
	p("  SHA256SUMS lists every other file in this folder with its SHA-256. To check them all,")
	p("  from the folder that holds this one:")
	p("")
	p("    cd %s && sha256sum -c SHA256SUMS", stem)
	p("")
	p("  On macOS, use `shasum -a 256 -c SHA256SUMS` in place of sha256sum.")
	p("")

	p("OPENING A PROTECTED FILE")
	p("")
	if m.Protected {
		p("  This file was protected with a passphrase. It was opened with age before you could read this.")
	} else {
		p("  This file was not protected.")
	}
	p("  A protected file ends in .tar.age. Open it with age (https://age-encryption.org) and its passphrase, then unpack it:")
	p("")
	p("    age --decrypt -o %s.tar %s.tar.age && tar -xf %s.tar", stem, stem, stem)
	return b.String()
}

func count(n int64, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

var leftOutWords = map[string]string{
	LeftOutSignin:  "sign-in material and visits",
	LeftOutTransit: "work in transit",
	LeftOutMachine: "bound to a machine",
}
