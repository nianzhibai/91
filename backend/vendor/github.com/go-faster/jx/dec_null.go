package jx

// Null reads a json object as null and
// returns whether it's a null or not.
func (d *Decoder) Null() error {
	if err := d.skipSpace(); err != nil {
		return err
	}

	var (
		offset = d.offset()
		buf    [4]byte
	)
	if err := d.readExact4(&buf); err != nil {
		return err
	}

	if string(buf[:]) != "null" {
		const encodedNull = 'n' | 'u'<<8 | 'l'<<16 | 'l'<<24
		return findInvalidToken4(buf, encodedNull, offset)
	}
	return nil
}
