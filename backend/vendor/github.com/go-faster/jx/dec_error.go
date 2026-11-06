package jx

import "fmt"

// badTokenErr means that Token was unexpected while decoding.
type badTokenErr struct {
	Token  byte
	Offset int
}

func (e *badTokenErr) Error() string {
	return fmt.Sprintf("unexpected byte %d %q at %d", e.Token, e.Token, e.Offset)
}

func badToken(c byte, offset int) error {
	return &badTokenErr{Token: c, Offset: offset}
}
