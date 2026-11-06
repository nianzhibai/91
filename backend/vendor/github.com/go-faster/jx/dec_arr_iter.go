package jx

import (
	"github.com/go-faster/errors"
)

// ArrIter is decoding array iterator.
type ArrIter struct {
	d      *Decoder
	err    error
	closed bool
	comma  bool
}

// ArrIter creates new array iterator.
func (d *Decoder) ArrIter() (ArrIter, error) {
	if err := d.consume('['); err != nil {
		return ArrIter{}, errors.Wrap(err, `"[" expected`)
	}
	if err := d.incDepth(); err != nil {
		return ArrIter{}, err
	}
	if _, err := d.more(); err != nil {
		return ArrIter{}, err
	}
	d.unread()
	return ArrIter{d: d}, nil
}

// Next consumes element and returns false, if there is no elements anymore.
func (i *ArrIter) Next() bool {
	if i.closed || i.err != nil {
		return false
	}

	dec := i.d
	c, err := dec.more()
	if err != nil {
		i.err = err
		return false
	}
	if c == ']' {
		i.closed = true
		i.err = dec.decDepth()
		return false
	}
	if i.comma {
		if c != ',' {
			err := badToken(c, dec.offset()-1)
			i.err = errors.Wrap(err, `"," expected`)
			return false
		}
	} else {
		dec.unread()
	}
	i.comma = true
	return true
}

// Err returns the error, if any, that was encountered during iteration.
func (i *ArrIter) Err() error {
	return i.err
}
