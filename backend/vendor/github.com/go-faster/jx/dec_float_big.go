package jx

import (
	"io"
	"math/big"

	"github.com/go-faster/errors"
)

// BigFloat read big.Float
func (d *Decoder) BigFloat() (*big.Float, error) {
	str, err := d.numberAppend(nil)
	if err != nil {
		return nil, errors.Wrap(err, "number")
	}
	prec := 64
	if len(str) > prec {
		prec = len(str)
	}
	val, _, err := big.ParseFloat(string(str), 10, uint(prec), big.ToZero)
	if err != nil {
		return nil, errors.Wrap(err, "float")
	}
	return val, nil
}

// BigInt read big.Int
func (d *Decoder) BigInt() (*big.Int, error) {
	str, err := d.numberAppend(nil)
	if err != nil {
		return nil, errors.Wrap(err, "number")
	}
	v := big.NewInt(0)
	var ok bool
	if v, ok = v.SetString(string(str), 10); !ok {
		return nil, errors.New("invalid")
	}
	return v, nil
}

func (d *Decoder) number() ([]byte, error) {
	start := d.head
	buf := d.buf[d.head:d.tail]
	for i, c := range buf {
		switch floatDigits[c] {
		case invalidCharForNumber:
			return nil, badToken(c, d.offset()+i)
		case endOfNumber:
			// End of number.
			d.head += i
			return d.buf[start:d.head], nil
		default:
			continue
		}
	}
	// Buffer is number within head:tail.
	d.head = d.tail
	return d.buf[start:d.tail], nil
}

func (d *Decoder) numberAppend(b []byte) ([]byte, error) {
	for {
		r, err := d.number()
		if err != nil {
			return nil, err
		}

		b = append(b, r...)
		if d.head != d.tail {
			return b, nil
		}

		if err := d.read(); err != nil {
			if err == io.EOF {
				return b, nil
			}
			return b, err
		}
	}
}
