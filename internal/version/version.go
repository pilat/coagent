package version

// Version is the binary version, stamped at build time with
// -ldflags "-X github.com/pilat/coagent/internal/version.Version=$(git describe …)".
//
// The fallback is "dev", not a plausible-looking number: a build that forgot the
// flag must be obvious in a skew report, not silently claim to be a release.
var Version = "dev"
