// Copyright 2018 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package errors

import (
	"fmt"
	"strings"
)

// A Formatter formats error messages.
type Formatter interface {
	error

	// FormatError prints the receiver's first error and returns the next error in
	// the error chain, if any.
	FormatError(p Printer) (next error)
}

// A Printer formats error messages.
//
// The most common implementation of Printer is the one provided by package fmt
// during Printf (as of Go 1.13). Localization packages such as golang.org/x/text/message
// typically provide their own implementations.
type Printer interface {
	// Print appends args to the message output.
	Print(args ...interface{})

	// Printf writes a formatted string.
	Printf(format string, args ...interface{})

	// Detail reports whether error detail is requested.
	// After the first call to Detail, all text written to the Printer
	// is formatted as additional detail, or ignored when
	// detail has not been requested.
	// If Detail returns false, the caller can avoid printing the detail at all.
	Detail() bool
}

// Errorf creates new error with format.
//
// If format contains the %w directive, Errorf falls back to fmt.Errorf
// and does not capture a caller frame.
func Errorf(format string, a ...interface{}) error {
	if !Trace() || strings.Contains(format, "%w") {
		return fmt.Errorf(format, a...)
	}
	return &errorString{sprintf(format, a), Caller(1)}
}

// sprintf is deliberately non-variadic: forwarding (format, a...) straight
// to fmt.Sprintf from Errorf makes vet's printf analyzer classify Errorf as
// a plain printf wrapper, rejecting %w in callers' format strings.
func sprintf(format string, a []interface{}) string {
	return fmt.Sprintf(format, a...)
}
