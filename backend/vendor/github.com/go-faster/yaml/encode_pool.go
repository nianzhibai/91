package yaml

import "sync"

var encoderPool = &sync.Pool{
	New: func() any {
		e := &encoder{}
		yaml_emitter_initialize(&e.emitter)
		return e
	},
}

func getEncoder() *encoder {
	e := encoderPool.Get().(*encoder)
	e.reset()
	return e
}

func putEncoder(e *encoder) {
	encoderPool.Put(e)
}

func (e *encoder) reset() {
	e.emitter = yaml_emitter_t{
		// Zero and re-size the buffer.
		buffer:     append(e.emitter.buffer[:0], make([]byte, output_buffer_size)...),
		states:     e.emitter.states[:0],
		events:     e.emitter.events[:0],
		best_width: -1,
	}
	yaml_emitter_set_unicode(&e.emitter, true)
	e.event = yaml_event_t{}
	e.out = nil
	e.flow = false
	e.indent = 0
	e.doneInit = false
}
