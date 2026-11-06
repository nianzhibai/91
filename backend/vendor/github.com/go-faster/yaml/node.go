package yaml

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// Kind defines the kind of node.
type Kind uint32

const (
	DocumentNode Kind = 1 << iota
	SequenceNode
	MappingNode
	ScalarNode
	AliasNode
)

// String implements fmt.Stringer.
func (s Kind) String() string {
	switch s {
	case DocumentNode:
		return "Document"
	case SequenceNode:
		return "Sequence"
	case MappingNode:
		return "Mapping"
	case ScalarNode:
		return "Scalar"
	case AliasNode:
		return "Alias"
	default:
		return fmt.Sprintf("Kind(%d)", s)
	}
}

// Style describes the style of a node.
type Style uint32

const (
	TaggedStyle Style = 1 << iota
	DoubleQuotedStyle
	SingleQuotedStyle
	LiteralStyle
	FoldedStyle
	FlowStyle
)

// String implements fmt.Stringer.
func (s Style) String() string {
	switch s {
	case 0:
		return "Undefined"
	case TaggedStyle:
		return "Tagged"
	case DoubleQuotedStyle:
		return "DoubleQuoted"
	case SingleQuotedStyle:
		return "SingleQuoted"
	case LiteralStyle:
		return "Literal"
	case FoldedStyle:
		return "Folded"
	case FlowStyle:
		return "Flow"
	default:
		return fmt.Sprintf("Style(%d)", s)
	}
}

// Node represents an element in the YAML document hierarchy. While documents
// are typically encoded and decoded into higher level types, such as structs
// and maps, Node is an intermediate representation that allows detailed
// control over the content being decoded or encoded.
//
// It's worth noting that although Node offers access into details such as
// line numbers, colums, and comments, the content when re-encoded will not
// have its original textual representation preserved. An effort is made to
// render the data plesantly, and to preserve comments near the data they
// describe, though.
//
// Values that make use of the Node type interact with the yaml package in the
// same way any other type would do, by encoding and decoding yaml data
// directly or indirectly into them.
//
// For example:
//
//	var person struct {
//	        Name    string
//	        Address yaml.Node
//	}
//	err := yaml.Unmarshal(data, &person)
//
// Or by itself:
//
//	var person Node
//	err := yaml.Unmarshal(data, &person)
type Node struct {
	// Kind defines whether the node is a document, a mapping, a sequence,
	// a scalar value, or an alias to another node. The specific data type of
	// scalar nodes may be obtained via the ShortTag and LongTag methods.
	Kind Kind

	// Style allows customizing the appearance of the node in the tree.
	Style Style

	// Tag holds the YAML tag defining the data type for the value.
	// When decoding, this field will always be set to the resolved tag,
	// even when it wasn't explicitly provided in the YAML content.
	// When encoding, if this field is unset the value type will be
	// implied from the node properties, and if it is set, it will only
	// be serialized into the representation if TaggedStyle is used or
	// the implicit tag diverges from the provided one.
	Tag string

	// Value holds the unescaped and unquoted representation of the value.
	Value string

	// Anchor holds the anchor name for this node, which allows aliases to point to it.
	Anchor string

	// Alias holds the node that this alias points to. Only valid when Kind is AliasNode.
	Alias *Node

	// Content holds contained nodes for documents, mappings, and sequences.
	Content []*Node

	// HeadComment holds any comments in the lines preceding the node and
	// not separated by an empty line.
	HeadComment string

	// LineComment holds any comments at the end of the line where the node is in.
	LineComment string

	// FootComment holds any comments following the node and before empty lines.
	FootComment string

	// Line and Column hold the node position in the decoded YAML text.
	// These fields are not respected when encoding the node.
	Line   int
	Column int
}

// IsZero returns whether the node has all of its fields unset.
func (n *Node) IsZero() bool {
	return n.Kind == 0 && n.Style == 0 && n.Tag == "" && n.Value == "" && n.Anchor == "" && n.Alias == nil && n.Content == nil &&
		n.HeadComment == "" && n.LineComment == "" && n.FootComment == "" && n.Line == 0 && n.Column == 0
}

// LongTag returns the long form of the tag that indicates the data type for
// the node. If the Tag field isn't explicitly defined, one will be computed
// based on the node properties.
func (n *Node) LongTag() string {
	return longTag(n.ShortTag())
}

// ShortTag returns the short form of the YAML tag that indicates data type for
// the node. If the Tag field isn't explicitly defined, one will be computed
// based on the node properties.
func (n *Node) ShortTag() string {
	if n.indicatedString() {
		return strTag
	}
	if n.Tag == "" || n.Tag == "!" {
		switch n.Kind {
		case MappingNode:
			return mapTag
		case SequenceNode:
			return seqTag
		case AliasNode:
			if n.Alias != nil {
				return n.Alias.ShortTag()
			}
		case ScalarNode:
			tag, _ := resolve("", n.Value)
			return tag
		case 0:
			// Special case to make the zero value convenient.
			if n.IsZero() {
				return nullTag
			}
		}
		return ""
	}
	return shortTag(n.Tag)
}

func (n *Node) indicatedString() bool {
	return n.Kind == ScalarNode &&
		(shortTag(n.Tag) == strTag ||
			(n.Tag == "" || n.Tag == "!") && n.Style&(SingleQuotedStyle|DoubleQuotedStyle|LiteralStyle|FoldedStyle) != 0)
}

// SetString is a convenience function that sets the node to a string value
// and defines its style in a pleasant way depending on its content.
func (n *Node) SetString(s string) {
	n.Kind = ScalarNode
	if utf8.ValidString(s) {
		n.Value = s
		n.Tag = strTag
	} else {
		n.Value = encodeBase64(s)
		n.Tag = binaryTag
	}
	if strings.Contains(n.Value, "\n") {
		n.Style = LiteralStyle
	}
}
