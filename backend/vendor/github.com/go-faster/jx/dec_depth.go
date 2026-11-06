package jx

import "github.com/go-faster/errors"

// limit maximum depth of nesting, as allowed by https://tools.ietf.org/html/rfc7159#section-9
const maxDepth = 10000

var errMaxDepth = errors.New("depth: maximum")

func (d *Decoder) incDepth() error {
	d.depth++
	if d.depth > maxDepth {
		return errMaxDepth
	}
	return nil
}

var errNegativeDepth = errors.New("depth: negative")

func (d *Decoder) decDepth() error {
	d.depth--
	if d.depth < 0 {
		return errNegativeDepth
	}
	return nil
}
