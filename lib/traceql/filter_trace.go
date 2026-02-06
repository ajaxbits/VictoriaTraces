package traceql

// TraceFilter is a filter for span, e.g. `{...}`
type filterTrace struct {
	andFilter filter
}

func (f *filterTrace) String() string {
	return "{" + f.andFilter.String() + "}"
}
