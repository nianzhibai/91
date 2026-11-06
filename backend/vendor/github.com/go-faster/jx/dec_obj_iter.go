package jx

import "github.com/go-faster/errors"

// ObjIter is decoding object iterator.
type ObjIter struct {
	d        *Decoder
	key      []byte
	err      error
	isBuffer bool
	closed   bool
	comma    bool
}

// ObjIter creates new object iterator.
func (d *Decoder) ObjIter() (ObjIter, error) {
	if err := d.consume('{'); err != nil {
		return ObjIter{}, errors.Wrap(err, `"{" expected`)
	}
	if err := d.incDepth(); err != nil {
		return ObjIter{}, err
	}
	if _, err := d.more(); err != nil {
		return ObjIter{}, err
	}
	d.unread()
	return ObjIter{d: d, isBuffer: d.reader == nil}, nil
}

// Key returns current key.
//
// Key call must be preceded by a call to Next.
func (i *ObjIter) Key() []byte {
	return i.key
}

// Next consumes element and returns false, if there is no elements anymore.
func (i *ObjIter) Next() bool {
	if i.closed || i.err != nil {
		return false
	}

	dec := i.d
	c, err := dec.more()
	if err != nil {
		i.err = err
		return false
	}
	if c == '}' {
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

	k, err := dec.str(value{raw: i.isBuffer})
	if err != nil {
		i.err = errors.Wrap(err, "field name")
		return false
	}
	if err := dec.consume(':'); err != nil {
		i.err = errors.Wrap(err, `":" expected`)
		return false
	}
	// Skip whitespace.
	if _, err = dec.more(); err != nil {
		err := badToken(c, dec.offset()-1)
		i.err = errors.Wrap(err, `"," or "}" expected`)
		return false
	}
	dec.unread()

	i.comma = true
	i.key = k.buf

	return true
}

// Err returns the error, if any, that was encountered during iteration.
func (i *ObjIter) Err() error {
	return i.err
}
