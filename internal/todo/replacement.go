package todo

// ReplacementItem is a requested item before call-scoped identity normalization.
type ReplacementItem struct {
	ID       *string
	Content  string
	Status   Status
	Priority Priority
}
