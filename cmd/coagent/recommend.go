package main

// recommendation is what a vendor's first provider is set up with: Default for
// sessions, Onboarding for the setup chat. Usually the same job, so usually equal.
type recommendation struct {
	Default    string
	Onboarding string
}

// recommendations maps a catalog section to its picks. The criterion is the
// cheapest sane tool-caller: a first run that stalls beats no cost saved.
var recommendations = map[string]recommendation{
	driverAnthropic:  {Default: "claude-sonnet-5", Onboarding: "claude-sonnet-5"},
	driverOpenRouter: {Default: "anthropic/claude-sonnet-5", Onboarding: "anthropic/claude-sonnet-5"},
	"google-vertex":  {Default: "gemini-3.5-flash", Onboarding: "gemini-3.5-flash"},
	driverOpenAI:     {Default: "gpt-5-mini", Onboarding: "gpt-5-mini"},
}

// recommend returns a catalog section's picks. A section with none — a bare
// openai endpoint, whose vendor is unknowable from the driver name — reports
// false, and the caller has to ask for a model id instead of guessing one.
func recommend(section string) (recommendation, bool) {
	r, ok := recommendations[section]

	return r, ok
}

// models lists a recommendation's ids in enable order: the default first, so it
// lands at index 0 and becomes the config's default, then the onboarding model
// when it is a different one.
func (r recommendation) models() []string {
	if r.Onboarding == "" || r.Onboarding == r.Default {
		return []string{r.Default}
	}

	return []string{r.Default, r.Onboarding}
}
