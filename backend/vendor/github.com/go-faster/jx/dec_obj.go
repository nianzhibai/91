package jx

import (
	"github.com/go-faster/errors"
)

// ObjBytes calls f for every key in object, using byte slice as key.
//
// The key value is valid only until f is not returned.
func (d *Decoder) ObjBytes(f func(d *Decoder, key []byte) error) error {
	if err := d.consume('{'); err != nil {
		return errors.Wrap(err, `"{" expected`)
	}
	if f == nil {
		return d.skipObj()
	}
	if err := d.incDepth(); err != nil {
		return err
	}
	c, err := d.more()
	if err != nil {
		return errors.Wrap(err, `'"' or "}" expected`)
	}
	if c == '}' {
		return d.decDepth()
	}
	d.unread()
	// Do not reference internal buffer for key if decoder is not buffered.
	//
	// Otherwise, subsequent reads may overwrite the key.
	//
	// See https://github.com/go-faster/jx/pull/62.
	isBuffer := d.reader == nil

	k, err := d.str(value{raw: isBuffer})
	if err != nil {
		return errors.Wrap(err, "field name")
	}
	if err := d.consume(':'); err != nil {
		return errors.Wrap(err, `":" expected`)
	}
	// Skip whitespace.
	if _, err = d.more(); err != nil {
		return err
	}
	d.unread()
	if err := f(d, k.buf); err != nil {
		return errors.Wrap(err, "callback")
	}

	c, err = d.more()
	if err != nil {
		return errors.Wrap(err, `"," or "}" expected`)
	}
	for c == ',' {
		k, err := d.str(value{raw: isBuffer})
		if err != nil {
			return errors.Wrap(err, "field name")
		}
		if err := d.consume(':'); err != nil {
			return errors.Wrap(err, `":" expected`)
		}
		// Check that value exists.
		if _, err = d.more(); err != nil {
			return err
		}
		d.unread()
		if err := f(d, k.buf); err != nil {
			return errors.Wrap(err, "callback")
		}
		if c, err = d.more(); err != nil {
			return err
		}
	}
	if c != '}' {
		err := badToken(c, d.offset()-1)
		return errors.Wrap(err, `"}" expected`)
	}
	return d.decDepth()
}

// Obj reads json object, calling f on each field.
//
// Use ObjBytes to reduce heap allocations for keys.
func (d *Decoder) Obj(f func(d *Decoder, key string) error) error {
	if f == nil {
		// Skipping object.
		return d.ObjBytes(nil)
	}
	return d.ObjBytes(func(d *Decoder, key []byte) error {
		return f(d, string(key))
	})
}
