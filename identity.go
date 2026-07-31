package main

import "strings"

// The Go half of the single identity source.
//
// packaging/identity.sh is the authority for the product's name and everything
// derived from it, and it renders every packaging artifact from `.in`
// templates. The binary cannot be rendered — it is compiled — so the one value
// the Go side needs is restated here, in exactly one place, and pinned to the
// shell side by scripts/check-naming.sh, which fails the build when
// `identity.sh:BINARY_NAME` and `productName` disagree.
//
// Anything in this package that needs the product name (the environment-
// variable prefix, the default Application Support and Logs directory names,
// the temp-file prefix, the advertised device provider, the flag set's program
// name) derives it from this constant rather than spelling it out again.
const productName = "browser-print-agentd"

// envPrefix is the environment-variable namespace the agent reads its
// configuration mirrors from, and it must equal `ENV_PREFIX` in
// packaging/identity.sh — the installer scripts write variables under the same
// namespace. It is COMPUTED from productName rather than written out so the two
// cannot drift: a rename of the product renames the variables with it, in the
// same edit, with nothing left to remember.
var envPrefix = strings.ToUpper(strings.ReplaceAll(productName, "-", "_"))

// tempPrefix is the shared prefix of every temp file this product creates, and
// mirrors `TEMP_PREFIX` in packaging/identity.sh. It is deliberately shorter
// than productName and therefore restated rather than derived: the packaging
// chain's `mktemp` templates and this package's spool files are meant to share
// one visibly common prefix in `ls /tmp`, which is what makes an orphaned file
// attributable at a glance.
const tempPrefix = "browser-print"
