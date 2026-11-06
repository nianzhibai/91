package jx

import (
	"github.com/go-faster/errors"
)

// Elem skips to the start of next array element, returning true boolean
// if element exists.
//
// Can be called before or in Array.
func (d *Decoder) Elem() (ok bool, err error) {
	c, err := d.next()
	if err != nil {
		return false, err
	}
	switch c {
	case '[':
		c, err := d.more()
		if err != nil {
			return false, err
		}
		if c != ']' {
			d.unread()
			return true, nil
		}
		return false, nil
	case ']':
		return false, nil
	case ',':
		return true, nil
	default:
		return false, errors.Wrap(badToken(c, d.offset()), `"[", "," or "]" expected`)
	}
}

// Arr decodes array and invokes callback on each array element.
func (d *Decoder) Arr(f func(d *Decoder) error) error {
	if err := d.consume('['); err != nil {
		return errors.Wrap(err, `"[" expected`)
	}
	if f == nil {
		return d.skipArr()
	}
	if err := d.incDepth(); err != nil {
		return err
	}
	c, err := d.more()
	if err != nil {
		return errors.Wrap(err, `value or "]" expected`)
	}
	if c == ']' {
		return d.decDepth()
	}
	d.unread()
	if err := f(d); err != nil {
		return errors.Wrap(err, "callback")
	}

	c, err = d.more()
	if err != nil {
		return errors.Wrap(err, `"," or "]" expected`)
	}
	for c == ',' {
		// Skip whitespace before reading element.
		if _, err := d.next(); err != nil {
			return err
		}
		d.unread()
		if err := f(d); err != nil {
			return errors.Wrap(err, "callback")
		}
		if c, err = d.next(); err != nil {
			return err
		}
	}
	if c != ']' {
		err := badToken(c, d.offset()-1)
		return errors.Wrap(err, `"]" expected`)
	}
	return d.decDepth()
}
