package yaml

import (
	"io"
	"reflect"

	"go.uber.org/multierr"
)

// The Unmarshaler interface may be implemented by types to customize their
// behavior when being unmarshaled from a YAML document.
type Unmarshaler interface {
	UnmarshalYAML(value *Node) error
}

type obsoleteUnmarshaler interface {
	UnmarshalYAML(unmarshal func(any) error) error
}

// Unmarshal decodes the first document found within the in byte slice
// and assigns decoded values into the out value.
//
// Maps and pointers (to a struct, string, int, etc) are accepted as out
// values. If an internal pointer within a struct is not initialized,
// the yaml package will initialize it if necessary for unmarshaling
// the provided data. The out parameter must not be nil.
//
// The type of the decoded values should be compatible with the respective
// values in out. If one or more values cannot be decoded due to a type
// mismatches, decoding continues partially until the end of the YAML
// content, and a *yaml.TypeError is returned with details for all
// missed values.
//
// Struct fields are only unmarshaled if they are exported (have an
// upper case first letter), and are unmarshaled using the field name
// lowercased as the default key. Custom keys may be defined via the
// "yaml" name in the field tag: the content preceding the first comma
// is used as the key, and the following comma-separated options are
// used to tweak the marshaling process (see Marshal).
// Conflicting names result in a runtime error.
//
// For example:
//
//	type T struct {
//	    F int `yaml:"a,omitempty"`
//	    B int
//	}
//	var t T
//	yaml.Unmarshal([]byte("a: 1\nb: 2"), &t)
//
// See the documentation of Marshal for the format of tags and a list of
// supported tag options.
func Unmarshal(in []byte, out any) (err error) {
	return unmarshal(in, out)
}

// A Decoder reads and decodes YAML values from an input stream.
type Decoder struct {
	parser      *parser
	knownFields bool
}

// NewDecoder returns a new decoder that reads from r.
//
// The decoder introduces its own buffering and may read
// data from r beyond the YAML values requested.
func NewDecoder(r io.Reader) *Decoder {
	return &Decoder{
		parser: newParserFromReader(r),
	}
}

// KnownFields ensures that the keys in decoded mappings to
// exist as fields in the struct being decoded into.
func (dec *Decoder) KnownFields(enable bool) {
	dec.knownFields = enable
}

// Decode reads the next YAML-encoded value from its input
// and stores it in the value pointed to by v.
//
// See the documentation for Unmarshal for details about the
// conversion of YAML into a Go value.
func (dec *Decoder) Decode(v any) (err error) {
	d := newDecoder()
	d.knownFields = dec.knownFields
	defer handleErr(&err)
	node := dec.parser.parse()
	if node == nil {
		return io.EOF
	}
	out := reflect.ValueOf(v)
	if out.Kind() == reflect.Ptr && !out.IsNil() {
		out = out.Elem()
	}
	d.unmarshal(node, out)
	if len(d.terrors) > 0 {
		return &TypeError{
			Group: multierr.Combine(d.terrors...),
		}
	}
	return nil
}

// Decode decodes the node and stores its data into the value pointed to by v.
//
// See the documentation for Unmarshal for details about the
// conversion of YAML into a Go value.
func (n *Node) Decode(v any) (err error) {
	d := newDecoder()
	defer handleErr(&err)
	out := reflect.ValueOf(v)
	if out.Kind() == reflect.Ptr && !out.IsNil() {
		out = out.Elem()
	}
	d.unmarshal(n, out)
	if len(d.terrors) > 0 {
		return &TypeError{
			Group: multierr.Combine(d.terrors...),
		}
	}
	return nil
}

func unmarshal(in []byte, out any) (err error) {
	defer handleErr(&err)
	d := newDecoder()
	p := newParser(in)
	defer p.destroy()
	node := p.parse()
	if node != nil {
		v := reflect.ValueOf(out)
		if v.Kind() == reflect.Ptr && !v.IsNil() {
			v = v.Elem()
		}
		d.unmarshal(node, v)
	}
	if len(d.terrors) > 0 {
		return &TypeError{
			Group: multierr.Combine(d.terrors...),
		}
	}
	return nil
}
