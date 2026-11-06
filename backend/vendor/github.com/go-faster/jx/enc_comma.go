package jx

// begin should be called before new Array or Object.
func (e *Encoder) begin() {
	e.first = append(e.first, true)
}

// end should be called after Array or Object.
func (e *Encoder) end() {
	if len(e.first) == 0 {
		return
	}
	e.first = e.first[:e.current()]
}

func (e *Encoder) current() int { return len(e.first) - 1 }

// comma should be called before any new value.
func (e *Encoder) comma() bool {
	// Writing commas.
	// 1. Before every field expect first.
	// 2. Before every array element except first.
	if len(e.first) == 0 {
		return false
	}
	current := e.current()
	_ = e.first[current]
	if e.first[current] {
		e.first[current] = false
		return false
	}
	return e.byte(',') ||
		e.writeIndent()
}
