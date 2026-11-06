package jx

import (
	"io"
	"math/bits"
)

// Next gets Type of relatively next json element
func (d *Decoder) Next() Type {
	v, err := d.next()
	if err == nil {
		d.unread()
	}
	return types[v]
}

var spaceSet = [256]byte{
	' ': 1, '\n': 1, '\t': 1, '\r': 1,
}

func (d *Decoder) consume(c byte) (err error) {
	for {
		buf := d.buf[d.head:d.tail]
		for i, got := range buf {
			switch spaceSet[got] {
			default:
				if c != got {
					return badToken(got, d.offset()+i)
				}
				d.head += i + 1
				return nil
			case 1:
				continue
			}
		}
		if err = d.read(); err != nil {
			if err == io.EOF {
				return io.ErrUnexpectedEOF
			}
			return err
		}
	}
}

// more is next but io.EOF is unexpected.
func (d *Decoder) more() (byte, error) {
	c, err := d.next()
	if err == io.EOF {
		err = io.ErrUnexpectedEOF
	}
	return c, err
}

// next reads next non-whitespace token or error.
func (d *Decoder) next() (byte, error) {
	for {
		buf := d.buf[d.head:d.tail]
		for i, c := range buf {
			switch spaceSet[c] {
			default:
				d.head += i + 1
				return c, nil
			case 1:
				continue
			}
		}
		if err := d.read(); err != nil {
			return 0, err
		}
	}
}

// peek returns next byte without advancing.
func (d *Decoder) peek() (byte, error) {
	if d.head == d.tail {
		if err := d.read(); err != nil {
			return 0, err
		}
	}
	c := d.buf[d.head]
	return c, nil
}

func (d *Decoder) byte() (byte, error) {
	if d.head == d.tail {
		err := d.read()
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		if err != nil {
			return 0, err
		}
	}
	c := d.buf[d.head]
	d.head++
	return c, nil
}

func (d *Decoder) read() error {
	if d.reader == nil {
		d.head = d.tail
		return io.EOF
	}

	n, err := d.reader.Read(d.buf)
	switch err {
	case nil:
	case io.EOF:
		if n > 0 {
			break
		}
		fallthrough
	default:
		return err
	}

	d.streamOffset += d.tail
	d.head = 0
	d.tail = n
	return nil
}

func (d *Decoder) readAtLeast(n int) error {
	if d.reader == nil {
		d.head = d.tail
		return io.ErrUnexpectedEOF
	}

	if need := n - len(d.buf); need > 0 {
		d.buf = append(d.buf, make([]byte, need)...)
	}
	n, err := io.ReadAtLeast(d.reader, d.buf, n)
	if err != nil {
		if err == io.EOF && n == 0 {
			return io.ErrUnexpectedEOF
		}
		return err
	}

	d.streamOffset += d.tail
	d.head = 0
	d.tail = n
	return nil
}

func (d *Decoder) unread() { d.head-- }

func (d *Decoder) readExact4(b *[4]byte) error {
	if buf := d.buf[d.head:d.tail]; len(buf) >= len(b) {
		d.head += copy(b[:], buf[:4])
		return nil
	}

	n := copy(b[:], d.buf[d.head:d.tail])
	if err := d.readAtLeast(len(b) - n); err != nil {
		return err
	}
	d.head += copy(b[n:], d.buf[d.head:d.tail])
	return nil
}

func findInvalidToken4(buf [4]byte, mask uint32, offset int) error {
	c := uint32(buf[0]) | uint32(buf[1])<<8 | uint32(buf[2])<<16 | uint32(buf[3])<<24
	idx := bits.TrailingZeros32(c^mask) / 8
	return badToken(buf[idx], offset+idx)
}
