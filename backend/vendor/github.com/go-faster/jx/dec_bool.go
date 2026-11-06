package jx

// Bool reads a json object as Bool
func (d *Decoder) Bool() (bool, error) {
	if err := d.skipSpace(); err != nil {
		return false, err
	}

	var (
		offset = d.offset()
		buf    [4]byte
	)
	if err := d.readExact4(&buf); err != nil {
		return false, err
	}

	switch string(buf[:]) {
	case "true":
		return true, nil
	case "fals":
		c, err := d.byte()
		if err != nil {
			return false, err
		}
		if c != 'e' {
			return false, badToken(c, offset+4)
		}
		return false, nil
	default:
		switch c := buf[0]; c {
		case 't':
			const encodedTrue = 't' | 'r'<<8 | 'u'<<16 | 'e'<<24
			return false, findInvalidToken4(buf, encodedTrue, offset)
		case 'f':
			const encodedFals = 'f' | 'a'<<8 | 'l'<<16 | 's'<<24
			return false, findInvalidToken4(buf, encodedFals, offset)
		default:
			return false, badToken(c, offset)
		}
	}
}
