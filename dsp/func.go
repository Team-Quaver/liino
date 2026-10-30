package dsp

// ProcessorFunc adapts a function to the Processor interface.
type ProcessorFunc func(data []float32, channels int)

func (f ProcessorFunc) Process(data []float32, channels int) { f(data, channels) }
func (f ProcessorFunc) Reset()                               {}
