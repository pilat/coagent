package tool

// TrailArgument names the call parameter a tool trail line shows.
type TrailArgument struct {
	Param string
	// Path renders the value relative to the working directory when it is inside it.
	Path bool
}

// TrailDeclarer is a tool whose tool trail line shows an argument.
type TrailDeclarer interface { //nolint:iface // Declared by tools elsewhere, consulted by the core.
	Tool
	TrailArgument() TrailArgument
}
