package jx

import (
	"io"

	"github.com/go-faster/errors"
)

// Skip skips a json object and positions to relatively the next json object.
func (d *Decoder) Skip() error {
	c, err := d.next()
	if err != nil {
		return err
	}
	switch c {
	case '"':
		if err := d.skipStr(); err != nil {
			return errors.Wrap(err, "str")
		}
		return nil
	case 'n':
		d.unread()
		return d.Null()
	case 't', 'f':
		d.unread()
		_, err := d.Bool()
		return err
	case '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
		d.unread()
		return d.skipNumber()
	case '[':
		if err := d.skipArr(); err != nil {
			return errors.Wrap(err, "array")
		}
		return nil
	case '{':
		if err := d.skipObj(); err != nil {
			return errors.Wrap(err, "object")
		}
		return nil
	default:
		return badToken(c, d.offset()-1)
	}
}

var skipNumberSet = [256]byte{
	'0': 1,
	'1': 1,
	'2': 1,
	'3': 1,
	'4': 1,
	'5': 1,
	'6': 1,
	'7': 1,
	'8': 1,
	'9': 1,

	',':  2,
	']':  2,
	'}':  2,
	' ':  2,
	'\t': 2,
	'\n': 2,
	'\r': 2,
}

// skipNumber reads one JSON number.
//
// Assumes d.buf is not empty.
func (d *Decoder) skipNumber() error {
	const (
		digitTag  byte = 1
		closerTag byte = 2
	)
	c := d.buf[d.head]
	d.head++
	switch c {
	case '-':
		c, err := d.byte()
		if err != nil {
			return err
		}
		// Character after '-' must be a digit.
		if skipNumberSet[c] != digitTag {
			return badToken(c, d.offset()-1)
		}
		if c != '0' {
			break
		}
		fallthrough
	case '0':
		// If buffer is empty, try to read more.
		if d.head == d.tail {
			err := d.read()
			if err != nil {
				// There is no data anymore.
				if err == io.EOF {
					return nil
				}
				return err
			}
		}

		c = d.buf[d.head]
		if skipNumberSet[c] == closerTag {
			return nil
		}
		switch c {
		case '.':
			goto stateDot
		case 'e', 'E':
			goto stateExp
		default:
			return badToken(c, d.offset())
		}
	}
	for {
		for i, c := range d.buf[d.head:d.tail] {
			switch skipNumberSet[c] {
			case closerTag:
				d.head += i
				return nil
			case digitTag:
				continue
			}

			switch c {
			case '.':
				d.head += i
				goto stateDot
			case 'e', 'E':
				d.head += i
				goto stateExp
			default:
				return badToken(c, d.offset()+i)
			}
		}

		if err := d.read(); err != nil {
			// There is no data anymore.
			if err == io.EOF {
				d.head = d.tail
				return nil
			}
			return err
		}
	}

stateDot:
	d.head++
	{
		var last byte = '.'
		for {
			for i, c := range d.buf[d.head:d.tail] {
				switch skipNumberSet[c] {
				case closerTag:
					d.head += i
					// Check that dot is not last character.
					if last == '.' {
						return io.ErrUnexpectedEOF
					}
					return nil
				case digitTag:
					last = c
					continue
				}

				switch c {
				case 'e', 'E':
					if last == '.' {
						return badToken(c, d.offset()+i)
					}
					d.head += i
					goto stateExp
				default:
					return badToken(c, d.offset()+i)
				}
			}

			if err := d.read(); err != nil {
				// There is no data anymore.
				if err == io.EOF {
					d.head = d.tail
					// Check that dot is not last character.
					if last == '.' {
						return io.ErrUnexpectedEOF
					}
					return nil
				}
				return err
			}
		}
	}
stateExp:
	d.head++
	// There must be a number or sign after e.
	{
		numOrSign, err := d.byte()
		if err != nil {
			return err
		}
		if skipNumberSet[numOrSign] != digitTag { // If next character is not a digit, check for sign.
			if numOrSign == '-' || numOrSign == '+' {
				num, err := d.byte()
				if err != nil {
					return err
				}
				// There must be a number after sign.
				if skipNumberSet[num] != digitTag {
					return badToken(num, d.offset()-1)
				}
			} else {
				return badToken(numOrSign, d.offset()-1)
			}
		}
	}
	for {
		for i, c := range d.buf[d.head:d.tail] {
			if skipNumberSet[c] == closerTag {
				d.head += i
				return nil
			}
			if skipNumberSet[c] == 0 {
				return badToken(c, d.offset()+i)
			}
		}

		if err := d.read(); err != nil {
			// There is no data anymore.
			if err == io.EOF {
				d.head = d.tail
				return nil
			}
			return err
		}
	}
}

var (
	escapedStrSet = [256]byte{
		'"':  '"',
		'\\': '\\',
		'/':  '/',
		'b':  '\b',
		'f':  '\f',
		'n':  '\n',
		'r':  '\r',
		't':  '\t',
		'u':  'u',
	}
	hexSet = [256]byte{
		'0': 0x0 + 1, '1': 0x1 + 1, '2': 0x2 + 1, '3': 0x3 + 1,
		'4': 0x4 + 1, '5': 0x5 + 1, '6': 0x6 + 1, '7': 0x7 + 1,
		'8': 0x8 + 1, '9': 0x9 + 1,

		'A': 0xA + 1, 'B': 0xB + 1, 'C': 0xC + 1, 'D': 0xD + 1,
		'E': 0xE + 1, 'F': 0xF + 1,

		'a': 0xa + 1, 'b': 0xb + 1, 'c': 0xc + 1, 'd': 0xd + 1,
		'e': 0xe + 1, 'f': 0xf + 1,
	}
)

// skipStr reads one JSON string.
//
// Assumes first quote was consumed.
func (d *Decoder) skipStr() error {
	var (
		c byte
		i int
	)
readStr:
	for {
		i = 0
		buf := d.buf[d.head:d.tail]
		for len(buf) >= 8 {
			c = buf[0]
			if safeSet[c] != 0 {
				goto readTok
			}
			i++

			c = buf[1]
			if safeSet[c] != 0 {
				goto readTok
			}
			i++

			c = buf[2]
			if safeSet[c] != 0 {
				goto readTok
			}
			i++

			c = buf[3]
			if safeSet[c] != 0 {
				goto readTok
			}
			i++

			c = buf[4]
			if safeSet[c] != 0 {
				goto readTok
			}
			i++

			c = buf[5]
			if safeSet[c] != 0 {
				goto readTok
			}
			i++

			c = buf[6]
			if safeSet[c] != 0 {
				goto readTok
			}
			i++

			c = buf[7]
			if safeSet[c] != 0 {
				goto readTok
			}
			i++

			buf = buf[8:]
		}
		var n int
		for n, c = range buf {
			if safeSet[c] != 0 {
				i += n
				goto readTok
			}
		}

		if err := d.read(); err != nil {
			if err == io.EOF {
				err = io.ErrUnexpectedEOF
			}
			return err
		}
	}

readTok:
	; // Bug in cover tool, see https://github.com/golang/go/issues/28319.
	switch {
	case c == '"':
		d.head += i + 1
		return nil
	case c == '\\':
		d.head += i + 1
		v, err := d.byte()
		if err != nil {
			return err
		}
		switch escapedStrSet[v] {
		case 'u':
			for range [4]struct{}{} {
				h, err := d.byte()
				if err != nil {
					return err
				}
				if hexSet[h] == 0 {
					return badToken(h, d.offset()-1)
				}
			}
		case 0:
			return badToken(v, d.offset()-1)
		}
	case c < ' ':
		return badToken(c, d.offset()+i)
	}
	goto readStr
}

// skipObj reads JSON object.
//
// Assumes first bracket was consumed.
func (d *Decoder) skipObj() error {
	if err := d.incDepth(); err != nil {
		return errors.Wrap(err, "inc")
	}

	c, err := d.more()
	if err != nil {
		return errors.Wrap(err, `'"' or "}" expected`)
	}
	switch c {
	case '}':
		return d.decDepth()
	case '"':
		d.unread()
	default:
		return badToken(c, d.offset()-1)
	}

	for {
		if err := d.consume('"'); err != nil {
			return errors.Wrap(err, `'"' expected`)
		}
		if err := d.skipStr(); err != nil {
			return errors.Wrap(err, "read field name")
		}
		if err := d.consume(':'); err != nil {
			return errors.Wrap(err, `":" expected`)
		}
		if err := d.Skip(); err != nil {
			return err
		}
		c, err := d.more()
		if err != nil {
			return errors.Wrap(err, `"," or "}" expected`)
		}
		switch c {
		case ',':
			continue
		case '}':
			return d.decDepth()
		default:
			return badToken(c, d.offset()-1)
		}
	}
}

// skipArr reads JSON array.
//
// Assumes first bracket was consumed.
func (d *Decoder) skipArr() error {
	if err := d.incDepth(); err != nil {
		return errors.Wrap(err, "inc")
	}

	c, err := d.more()
	if err != nil {
		return errors.Wrap(err, `value or "]" expected`)
	}
	if c == ']' {
		return d.decDepth()
	}
	d.unread()

	for {
		if err := d.Skip(); err != nil {
			return err
		}
		c, err := d.more()
		if err != nil {
			return errors.Wrap(err, `"," or "]" expected`)
		}
		switch c {
		case ',':
			continue
		case ']':
			return d.decDepth()
		default:
			return badToken(c, d.offset()-1)
		}
	}
}

// skipSpace skips space characters.
//
// Returns io.ErrUnexpectedEOF if got io.EOF.
func (d *Decoder) skipSpace() error {
	// Skip space.
	if _, err := d.more(); err != nil {
		return err
	}
	d.unread()
	return nil
}
