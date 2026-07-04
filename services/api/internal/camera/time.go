package camera

import "time"

// nowFn is an indirection that lets the time package be optional
// from the rest of the registry helpers (and makes tests trivial).
var nowFn = func() time.Time { return time.Now() }
