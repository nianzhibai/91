package yaml

import (
	"time"

	"github.com/go-faster/errors"
	"github.com/go-faster/jx"
)

func writeJSONSequence(e *jx.Encoder, n *Node) error {
	e.ArrStart()
	for _, n := range n.Content {
		if err := writeJSON(e, n); err != nil {
			return err
		}
	}
	e.ArrEnd()
	return nil
}

func writeJSONMapping(e *jx.Encoder, n *Node) error {
	var resolveKey func(k *Node) string
	resolveKey = func(k *Node) string {
		switch k.Kind {
		case ScalarNode:
			return k.Value
		case AliasNode:
			return resolveKey(k.Alias)
		default:
			fail(errors.New("unexpected node kind"))
			return "" // unreachable
		}
	}

	e.ObjStart()
	for i := 0; i < len(n.Content); i += 2 {
		k, v := n.Content[i], n.Content[i+1]

		// TODO(tdakkota): probably, we should just convert key to string.
		if tag := k.ShortTag(); tag != strTag {
			return errors.Errorf("can't use tag %q as a key", tag)
		}

		e.FieldStart(resolveKey(k))
		if err := writeJSON(e, v); err != nil {
			return err
		}
	}
	e.ObjEnd()
	return nil
}

func writeJSONScalar(e *jx.Encoder, n *Node) error {
	switch tag := n.ShortTag(); tag {
	case boolTag:
		e.Bool(n.Value == "true")
		return nil
	case nullTag:
		e.Null()
		return nil
	case "", intTag, floatTag:
		rtag, out := resolve(n.Tag, n.Value)
		switch out := out.(type) {
		case time.Time:
			e.Str(out.Format(time.RFC3339Nano))
			return nil
		case int64:
			e.Int64(out)
			return nil
		case uint64:
			e.UInt64(out)
			return nil
		case int:
			e.Int(out)
			return nil
		case uint:
			e.UInt(out)
			return nil
		case float32:
			e.Float32(out)
			return nil
		case float64:
			e.Float64(out)
			return nil
		}
		return errors.Errorf("unable to encode %q (rtag: %q)", n.Value, rtag)
	case strTag, binaryTag, timestampTag:
		// Timestamp is already in RFC3339Nano format.
		// Binary data is already base64-encoded.
		e.Str(n.Value)
		return nil
	default:
		// Fallback to string.
		e.Str(n.Value)
		return nil
	}
}

func writeJSON(e *jx.Encoder, n *Node) (rerr error) {
	defer handleErr(&rerr)

	switch n.Kind {
	case DocumentNode:
		switch len(n.Content) {
		case 0:
			return errors.New("empty document")
		case 1:
			return writeJSON(e, n.Content[0])
		default:
			return errors.New("multiple document nodes")
		}
	case SequenceNode:
		return writeJSONSequence(e, n)
	case MappingNode:
		return writeJSONMapping(e, n)
	case ScalarNode:
		return writeJSONScalar(e, n)
	case AliasNode:
		return writeJSON(e, n.Alias)
	default:
		return errors.Errorf("unknown node kind %v", n.Kind)
	}
}

// EncodeJSON writes the JSON representation of the node to given encoder.
func (n *Node) EncodeJSON(e *jx.Encoder) error {
	return writeJSON(e, n)
}
