package llm

import "errors"

// errSectionMissing separates a body that answered without the section from
// a catalog that could not be obtained, which must remain fatal.
var errSectionMissing = errors.New("models.dev has no section")
