package store

import "fmt"

// LimitError names the limit an operation would cross; it matches ErrLimitExceeded.
type LimitError struct {
	Scope      string // "org", else the owner's
	Name, Unit string
	Limit, Got int
}

func (e *LimitError) Error() string {
	owner := "the org's"
	if e.Scope != "org" {
		owner = "the owner's"
	}
	return fmt.Sprintf("%s %s limit of %d%s would be exceeded (%d%s)", owner, e.Name, e.Limit, e.Unit, e.Got, e.Unit)
}

func (e *LimitError) Is(target error) bool { return target == ErrLimitExceeded }
