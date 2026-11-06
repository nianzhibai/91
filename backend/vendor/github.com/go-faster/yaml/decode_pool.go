package yaml

import "sync"

var parserPool = &sync.Pool{
	New: func() any {
		p := &parser{}
		if !yaml_parser_initialize(&p.parser) {
			panic("failed to initialize YAML emitter")
		}
		return p
	},
}

func getParser() *parser {
	p := parserPool.Get().(*parser)
	p.reset()
	return p
}

func putParser(p *parser) {
	parserPool.Put(p)
}

func (p *parser) reset() {
	p.parser = yaml_parser_t{
		raw_buffer:     p.parser.raw_buffer[:0],
		buffer:         p.parser.buffer[:0],
		tokens:         p.parser.tokens[:0],
		simple_keys:    p.parser.simple_keys[:0],
		states:         p.parser.states[:0],
		marks:          p.parser.marks[:0],
		tag_directives: p.parser.tag_directives[:0],
	}
	p.event = yaml_event_t{}
	p.doc = nil
	p.anchors = nil
	p.doneInit = false
	p.textless = false
}
