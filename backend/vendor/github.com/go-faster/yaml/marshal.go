package yaml

import (
	"io"
	"reflect"
)

// The Marshaler interface may be implemented by types to customize their
// behavior when being marshaled into a YAML document. The returned value
// is marshaled in place of the original value implementing Marshaler.
//
// If an error is returned by MarshalYAML, the marshaling procedure stops
// and returns with the provided error.
type Marshaler interface {
	MarshalYAML() (any, error)
}

// Marshal serializes the value provided into a YAML document. The structure
// of the generated document will reflect the structure of the value itself.
// Maps and pointers (to struct, string, int, etc) are accepted as the in value.
//
// Struct fields are only marshaled if they are exported (have an upper case
// first letter), and are marshaled using the field name lowercased as the
// default key. Custom keys may be defined via the "yaml" name in the field
// tag: the content preceding the first comma is used as the key, and the
// following comma-separated options are used to tweak the marshaling process.
// Conflicting names result in a runtime error.
//
// The field tag format accepted is:
//
//	`(...) yaml:"[<key>][,<flag1>[,<flag2>]]" (...)`
//
// The following flags are currently supported:
//
//	omitempty    Only include the field if it's not set to the zero
//	             value for the type or to empty slices or maps.
//	             Zero valued structs will be omitted if all their public
//	             fields are zero, unless they implement an IsZero
//	             method (see the IsZeroer interface type), in which
//	             case the field will be excluded if IsZero returns true.
//
//	flow         Marshal using a flow style (useful for structs,
//	             sequences and maps).
//
//	inline       Inline the field, which must be a struct or a map,
//	             causing all of its fields or keys to be processed as if
//	             they were part of the outer struct. For maps, keys must
//	             not conflict with the yaml keys of other struct fields.
//
// In addition, if the key is "-", the field is ignored.
//
// For example:
//
//	type T struct {
//	    F int `yaml:"a,omitempty"`
//	    B int
//	}
//	yaml.Marshal(&T{B: 2}) // Returns "b: 2\n"
//	yaml.Marshal(&T{F: 1}} // Returns "a: 1\nb: 0\n"
func Marshal(in any) (out []byte, err error) {
	defer handleErr(&err)
	e := newEncoder()
	defer e.destroy()
	e.marshalDoc("", reflect.ValueOf(in))
	e.finish()
	out = e.out
	return
}

// An Encoder writes YAML values to an output stream.
type Encoder struct {
	encoder *encoder
}

// NewEncoder returns a new encoder that writes to w.
// The Encoder should be closed after use to flush all data
// to w.
func NewEncoder(w io.Writer) *Encoder {
	return &Encoder{
		encoder: newEncoderWithWriter(w),
	}
}

// Encode writes the YAML encoding of v to the stream.
// If multiple items are encoded to the stream, the
// second and subsequent document will be preceded
// with a "---" document separator, but the first will not.
//
// See the documentation for Marshal for details about the conversion of Go
// values to YAML.
func (e *Encoder) Encode(v any) (err error) {
	defer handleErr(&err)
	e.encoder.marshalDoc("", reflect.ValueOf(v))
	return nil
}

// Encode encodes value v and stores its representation in n.
//
// See the documentation for Marshal for details about the
// conversion of Go values into YAML.
func (n *Node) Encode(v any) (err error) {
	defer handleErr(&err)
	e := newEncoder()
	defer e.destroy()
	e.marshalDoc("", reflect.ValueOf(v))
	e.finish()
	p := newParser(e.out)
	p.textless = true
	defer p.destroy()
	doc := p.parse()
	*n = *doc.Content[0]
	return nil
}

// SetIndent changes the used indentation used when encoding.
func (e *Encoder) SetIndent(spaces int) {
	if spaces < 0 {
		panic("yaml: cannot indent to a negative number of spaces")
	}
	e.encoder.indent = spaces
}

// Close closes the encoder by writing any remaining data.
// It does not write a stream terminating string "...".
func (e *Encoder) Close() (err error) {
	defer handleErr(&err)
	e.encoder.finish()
	return nil
}
