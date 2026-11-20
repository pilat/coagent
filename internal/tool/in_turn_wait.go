package tool

import "time"

// InTurnWaitLimit is how long bash and sleep wait inside the turn: a command still running
// then moves to the background, and a longer sleep suspends the session.
const InTurnWaitLimit = 10 * time.Second
