package outbox

import "time"

func SetClock(r *Relay, now func() time.Time) { r.now = now }
