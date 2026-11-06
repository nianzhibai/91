package jx

import (
	"bytes"
	"strconv"

	"github.com/go-faster/errors"
)

var (
	pow10       = [...]uint64{1, 10, 100, 1000, 10000, 100000, 1000000}
	floatDigits = [256]int8{}
)

const (
	dotInNumber int8 = -iota - 1
	expInNumber
	plusInNumber
	minusInNumber
	endOfNumber
	invalidCharForNumber

	maxFloat64 = 1<<63 - 1
)

func init() {
	for i := 0; i < len(floatDigits); i++ {
		floatDigits[i] = invalidCharForNumber
	}
	floatDigits[','] = endOfNumber
	floatDigits[']'] = endOfNumber
	floatDigits['}'] = endOfNumber
	for ch, isSpace := range spaceSet {
		if isSpace == 1 {
			floatDigits[ch] = endOfNumber
		}
	}
	for i := int8('0'); i <= int8('9'); i++ {
		floatDigits[i] = i - int8('0')
	}
	floatDigits['.'] = dotInNumber
	floatDigits['e'] = expInNumber
	floatDigits['E'] = expInNumber
	floatDigits['+'] = plusInNumber
	floatDigits['-'] = minusInNumber
}

// Float32 reads float32 value.
func (d *Decoder) Float32() (float32, error) {
	c, err := d.more()
	if err != nil {
		return 0, err
	}
	if c != '-' {
		d.unread()
	}
	v, err := d.positiveFloat32()
	if err != nil {
		return 0, err
	}
	if c == '-' {
		v *= -1
	}
	return v, nil
}

func (d *Decoder) positiveFloat32() (float32, error) {
	i := d.head
	// First char.
	if i == d.tail {
		return d.float32Slow()
	}
	c := d.buf[i]
	i++
	ind := floatDigits[c]
	switch ind {
	case invalidCharForNumber, endOfNumber:
		return 0, badToken(c, d.offset())
	case dotInNumber, plusInNumber, expInNumber:
		err := badToken(c, d.offset())
		return 0, errors.Wrapf(err, "leading %q", c)
	case minusInNumber: // minus handled by caller
		err := badToken(c, d.offset())
		return 0, errors.Wrap(err, "double minus")
	case 0:
		if i == d.tail {
			return d.float32Slow()
		}
		c = d.buf[i]
		if floatDigits[c] >= 0 {
			err := badToken(c, d.offset()+1)
			return 0, errors.Wrap(err, "leading zero")
		}
	}
	value := uint64(ind)
	// Chars before dot.
NonDecimalLoop:
	for ; i < d.tail; i++ {
		c = d.buf[i]
		ind := floatDigits[c]
		switch ind {
		case invalidCharForNumber:
			return 0, badToken(c, d.offset()+i)
		case endOfNumber:
			d.head = i
			return float32(value), nil
		case dotInNumber, expInNumber:
			break NonDecimalLoop
		}
		if value > uint64SafeToMultiple10 {
			return d.float32Slow()
		}
		value = (value << 3) + (value << 1) + uint64(ind) // value = value * 10 + ind;
	}
	// Chars after dot.
	if c == '.' {
		i++
		decimalPlaces := 0
		if i == d.tail {
			return d.float32Slow()
		}
		for ; i < d.tail; i++ {
			c = d.buf[i]
			ind := floatDigits[c]
			switch ind {
			case endOfNumber:
				if decimalPlaces > 0 && decimalPlaces < len(pow10) {
					d.head = i
					return float32(float64(value) / float64(pow10[decimalPlaces])), nil
				}
				// too many decimal places
				return d.float32Slow()
			case dotInNumber, expInNumber, plusInNumber, minusInNumber:
				return d.float32Slow()
			case invalidCharForNumber:
				return 0, badToken(c, d.offset()+i)
			}
			decimalPlaces++
			if value > uint64SafeToMultiple10 {
				return d.float32Slow()
			}
			value = (value << 3) + (value << 1) + uint64(ind)
		}
	}
	return d.float32Slow()
}

// Float64 read float64
func (d *Decoder) Float64() (float64, error) {
	c, err := d.more()
	if err != nil {
		return 0, err
	}
	if c != '-' {
		d.unread()
	}
	v, err := d.positiveFloat64()
	if err != nil {
		return 0, err
	}
	if c == '-' {
		v *= -1
	}
	return v, nil
}

func (d *Decoder) positiveFloat64() (float64, error) {
	i := d.head
	// First char.
	if i == d.tail {
		return d.float64Slow()
	}
	c := d.buf[i]
	i++
	ind := floatDigits[c]
	switch ind {
	case invalidCharForNumber, endOfNumber:
		return 0, badToken(c, d.offset())
	case dotInNumber, plusInNumber, expInNumber:
		err := badToken(c, d.offset())
		return 0, errors.Wrapf(err, "leading %q", c)
	case minusInNumber: // minus handled by caller
		err := badToken(c, d.offset())
		return 0, errors.Wrap(err, "double minus")
	case 0:
		if i == d.tail {
			return d.float64Slow()
		}
		c = d.buf[i]
		if floatDigits[c] >= 0 {
			err := badToken(c, d.offset()+1)
			return 0, errors.Wrap(err, "leading zero")
		}
	}
	value := uint64(ind)
	// Chars before dot.
NonDecimal:
	for ; i < d.tail; i++ {
		c = d.buf[i]
		ind := floatDigits[c]
		switch ind {
		case invalidCharForNumber:
			return 0, badToken(c, d.offset()+i)
		case endOfNumber:
			d.head = i
			return float64(value), nil
		case dotInNumber, expInNumber:
			break NonDecimal
		}
		if value > uint64SafeToMultiple10 {
			return d.float64Slow()
		}
		value = (value << 3) + (value << 1) + uint64(ind) // value = value * 10 + ind;
	}
	// chars after dot
	if c == '.' {
		i++
		decimalPlaces := 0
		if i == d.tail {
			return d.float64Slow()
		}
		for ; i < d.tail; i++ {
			c = d.buf[i]
			ind := floatDigits[c]
			switch ind {
			case endOfNumber:
				if decimalPlaces > 0 && decimalPlaces < len(pow10) {
					d.head = i
					return float64(value) / float64(pow10[decimalPlaces]), nil
				}
				// too many decimal places
				return d.float64Slow()
			case dotInNumber, expInNumber, plusInNumber, minusInNumber:
				return d.float64Slow()
			case invalidCharForNumber:
				return 0, badToken(c, d.offset()+i)
			}
			decimalPlaces++
			// Not checking for uint64SafeToMultiple10 here because
			// if condition is positive value multiplied by 10 is
			// guaranteed to be bigger than maxFloat64.
			value = (value << 3) + (value << 1) + uint64(ind)
			if value > maxFloat64 {
				return d.float64Slow()
			}
		}
	}
	return d.float64Slow()
}

func (d *Decoder) float32Slow() (float32, error) {
	v, err := d.floatSlow(32)
	if err != nil {
		return 0, err
	}
	return float32(v), err
}

func (d *Decoder) float64Slow() (float64, error) { return d.floatSlow(64) }

func (d *Decoder) floatSlow(size int) (float64, error) {
	var (
		buf    [32]byte
		offset = d.offset()
	)

	str, err := d.numberAppend(buf[:0])
	if err != nil {
		return 0, errors.Wrap(err, "number")
	}

	if err := validateFloat(str, offset); err != nil {
		return 0, err
	}

	val, err := strconv.ParseFloat(string(str), size)
	if err != nil {
		return 0, err
	}

	return val, nil
}

func validateFloat(str []byte, offset int) error {
	// strconv.ParseFloat is not validating `1.` or `1.e1`
	if len(str) == 0 {
		// FIXME(tdakkota): use io.ErrUnexpectedEOF?
		return errors.New("empty")
	}

	switch c := str[0]; floatDigits[c] {
	case dotInNumber, plusInNumber, expInNumber:
		err := badToken(c, offset)
		return errors.Wrapf(err, "leading %q", c)
	case minusInNumber: // minus handled by caller
		err := badToken(c, offset)
		return errors.Wrap(err, "double minus")
	case 0:
		if len(str) >= 2 {
			switch str[1] {
			case 'e', 'E', '.':
			default:
				err := badToken(str[1], offset+1)
				return errors.Wrap(err, "leading zero")
			}
		}
	}

	dotPos := bytes.IndexByte(str, '.')
	if dotPos != -1 {
		if dotPos == len(str)-1 {
			// FIXME(tdakkota): use io.ErrUnexpectedEOF?
			return errors.New("dot as last char")
		}
		switch c := str[dotPos+1]; c {
		case '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
		default:
			err := badToken(c, offset+dotPos+1)
			return errors.Wrap(err, "no digit after dot")
		}
	}
	return nil
}
