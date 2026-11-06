package jx

import (
	"github.com/go-faster/errors"
)

// Num decodes number.
//
// Do not retain returned value, it references underlying buffer.
func (d *Decoder) Num() (Num, error) {
	return d.num(nil, false)
}

// NumAppend appends number.
func (d *Decoder) NumAppend(v Num) (Num, error) {
	return d.num(v, true)
}

// num decodes number.
func (d *Decoder) num(v Num, forceAppend bool) (Num, error) {
	switch d.Next() {
	case String:
		offset := d.offset()
		start := d.head

		str, err := d.str(value{raw: true})
		if err != nil {
			return Num{}, errors.Wrap(err, "str")
		}

		// Validate number.
		{
			d := Decoder{}
			d.ResetBytes(str.buf)

			c, err := d.next()
			if err != nil {
				return Num{}, err
			}
			switch c {
			case '0', '1', '2', '3', '4', '5', '6', '7', '8', '9', '-':
				d.unread()

				if err := d.skipNumber(); err != nil {
					return Num{}, errors.Wrap(err, "skip number")
				}
			default:
				return nil, badToken(c, offset)
			}
		}

		// If string is escaped or decoder is streaming, copy it.
		if !str.raw || forceAppend {
			v = append(v, '"')
			v = append(v, str.buf...)
			v = append(v, '"')
			return v, nil
		}
		return d.buf[start:d.head], nil
	case Number: // float or integer
		if forceAppend {
			raw, err := d.RawAppend(Raw(v))
			if err != nil {
				return nil, err
			}
			return Num(raw), nil
		}

		raw, err := d.Raw()
		if err != nil {
			return nil, err
		}
		return Num(raw), nil
	default:
		return v, errors.Errorf("unexpected %s", d.Next())
	}
}
