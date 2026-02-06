package traceql

// filterNoop does nothing
type filterNoop struct {
}

func (fn *filterNoop) String() string {
	return "*"
}
