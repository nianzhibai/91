package jx

import (
	"io"

	"github.com/go-faster/errors"
)

// Validate consumes all input, validating that input is a json object
// without any trialing data.
func (d *Decoder) Validate() error {
	// First encountered value skip should consume all buffer.
	if err := d.Skip(); err != nil {
		return errors.Wrap(err, "consume")
	}
	// Check for any trialing json.
	if err := d.Skip(); err != io.EOF {
		return errors.Wrap(err, "unexpected trialing data")
	}

	return nil
}
