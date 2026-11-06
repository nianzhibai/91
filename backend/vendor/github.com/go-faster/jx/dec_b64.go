package jx

import (
	"github.com/segmentio/asm/base64"

	"github.com/go-faster/errors"
)

// Base64 decodes base64 encoded data from string.
//
// Same as encoding/json, base64.StdEncoding or RFC 4648.
func (d *Decoder) Base64() ([]byte, error) {
	if d.Next() == Null {
		if err := d.Null(); err != nil {
			return nil, errors.Wrap(err, "read null")
		}
		return nil, nil
	}
	return d.Base64Append([]byte{})
}

// Base64Append appends base64 encoded data from string.
//
// Same as encoding/json, base64.StdEncoding or RFC 4648.
func (d *Decoder) Base64Append(b []byte) ([]byte, error) {
	if d.Next() == Null {
		if err := d.Null(); err != nil {
			return nil, errors.Wrap(err, "read null")
		}
		return b, nil
	}
	buf, err := d.StrBytes()
	if err != nil {
		return nil, errors.Wrap(err, "bytes")
	}

	decodedLen := base64.StdEncoding.DecodedLen(len(buf))
	start := len(b)
	b = append(b, make([]byte, decodedLen)...)

	n, err := base64.StdEncoding.Decode(b[start:], buf)
	if err != nil {
		return nil, errors.Wrap(err, "decode")
	}

	return b[:start+n], nil
}
