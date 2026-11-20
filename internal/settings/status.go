package settings

// Status is what a connector's page shows about how it is doing.
type Status struct {
	// Waiting says, in a sentence, what keeps it from working while it is
	// enabled, or "" when nothing does.
	Waiting string
	// Rows are facts about its work, such as when it last synced.
	Rows []StatusRow
	// Note is a sentence shown below the rows, or "".
	Note string
}

// StatusRow is one fact on a connector's page.
type StatusRow struct {
	Label string
	// Value is a *time.Time, shown as how long ago it was or "never" while
	// it is nil; an int64, a count; or a string.
	Value any
	// Detail follows the value, as in "2h ago · Roadmap", or is "".
	Detail string
}
