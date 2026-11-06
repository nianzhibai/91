package yaml

import (
	"fmt"
	"reflect"

	"github.com/go-faster/errors"
)

var _ = []interface {
	error
}{
	(*SyntaxError)(nil),
	(*UnmarshalError)(nil),
}

// SyntaxError is an error that occurs during parsing.
type SyntaxError struct {
	Offset int
	Line   int
	Column int
	Msg    string
}

func syntaxErr(offset, line, column int, msgf string, args ...any) error {
	return &SyntaxError{
		Offset: offset,
		Line:   line,
		Column: column,
		Msg:    fmt.Sprintf(msgf, args...),
	}
}

// Error returns the error message.
func (s *SyntaxError) Error() string {
	if s.Line == 0 {
		if s.Offset == 0 {
			return fmt.Sprintf("yaml: %s", s.Msg)
		}
		return fmt.Sprintf("yaml: offset %d: %s", s.Offset, s.Msg)
	}
	if s.Column == 0 {
		return fmt.Sprintf("yaml: line %d: %s", s.Line, s.Msg)
	}
	return fmt.Sprintf("yaml: line %d:%d: %s", s.Line, s.Column, s.Msg)
}

// UnknownFieldError reports an unknown field.
type UnknownFieldError struct {
	Field string
	Type  reflect.Type
}

// Error returns the error message.
func (d *UnknownFieldError) Error() string {
	return fmt.Sprintf("field %q not found in type %s", d.Field, d.Type)
}

func unknownFieldErr(field string, f *Node, typ reflect.Type) error {
	return &UnmarshalError{
		Node: f,
		Type: typ,
		Err:  &UnknownFieldError{Field: field, Type: typ},
	}
}

// DuplicateKeyError reports a duplicate key.
type DuplicateKeyError struct {
	First, Second *Node
}

func duplicateKeyErr(f, s *Node, typ reflect.Type) error {
	return &UnmarshalError{
		Node: f,
		Type: typ,
		Err:  &DuplicateKeyError{First: f, Second: s},
	}
}

// Error returns the error message.
func (d *DuplicateKeyError) Error() string {
	f, s := d.First, d.Second
	if s == nil {
		return fmt.Sprintf("duplicate key: %q", f.Value)
	}
	switch s.Kind {
	case SequenceNode, MappingNode:
		return fmt.Sprintf("mapping key already defined at line %d", s.Line)
	default:
		return fmt.Sprintf("mapping key %q already defined at line %d", s.Value, s.Line)
	}
}

// UnmarshalError is an error that occurs during unmarshaling.
type UnmarshalError struct {
	Node *Node
	Type reflect.Type
	Err  error
}

func unmarshalErrf(n *Node, typ reflect.Type, msgf string, args ...any) error {
	return &UnmarshalError{
		Node: n,
		Type: typ,
		Err:  errors.Errorf(msgf, args...),
	}
}

// Unwrap returns the underlying error.
func (s *UnmarshalError) Unwrap() error {
	return s.Err
}

// Error returns the error message.
func (s *UnmarshalError) Error() string {
	n := s.Node
	if n == nil || n.Line == 0 {
		return fmt.Sprintf("yaml: %s", s.Err)
	}
	return fmt.Sprintf("yaml: line %d: %s", n.Line, s.Err)
}

// MarshalError is an error that occurs during marshaling.
type MarshalError struct {
	Msg string
}

// Error returns the error message.
func (s *MarshalError) Error() string {
	return fmt.Sprintf("yaml: %s", s.Msg)
}

// A TypeError is returned by Unmarshal when one or more fields in
// the YAML document cannot be properly decoded into the requested
// types. When this error is returned, the value is still
// unmarshaled partially.
//
// Group is a multi-error which contains all errors that occurred.
// Use multierr.Errors to get a list of all errors.
type TypeError struct {
	Group error
}

// Unwrap returns the underlying error.
func (e *TypeError) Unwrap() error {
	return e.Group
}

// Error returns the error message.
func (e *TypeError) Error() string {
	return fmt.Sprintf("yaml: unmarshal errors:\n  %s", e.Group)
}

func handleErr(err *error) {
	if v := recover(); v != nil {
		if e, ok := v.(yamlError); ok {
			*err = e.err
		} else {
			panic(v)
		}
	}
}

type yamlError struct {
	err error
}

func fail(err error) {
	panic(yamlError{err})
}
