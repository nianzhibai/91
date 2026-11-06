package jx

import (
	"strconv"
)

func (d *Decoder) int(size int) (int, error) {
	switch size {
	case 8:
		v, err := d.Int8()
		return int(v), err
	case 16:
		v, err := d.Int16()
		return int(v), err
	case 32:
		v, err := d.Int32()
		return int(v), err
	default:
		v, err := d.Int64()
		return int(v), err
	}
}

// Int reads int.
func (d *Decoder) Int() (int, error) {
	return d.int(strconv.IntSize)
}

func (d *Decoder) uint(size int) (uint, error) {
	switch size {
	case 8:
		v, err := d.UInt8()
		return uint(v), err
	case 16:
		v, err := d.UInt16()
		return uint(v), err
	case 32:
		v, err := d.UInt32()
		return uint(v), err
	default:
		v, err := d.UInt64()
		return uint(v), err
	}
}

// UInt reads uint.
func (d *Decoder) UInt() (uint, error) {
	return d.uint(strconv.IntSize)
}
