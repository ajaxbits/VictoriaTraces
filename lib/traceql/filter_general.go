package traceql

type filterCommon struct {
	fieldName string
	op        string
	value     string
}

func (fp *filterCommon) String() string {
	return quoteFieldNameIfNeeded(fp.fieldName) + fp.op + quoteTokenIfNeeded(fp.value)
}

func quoteFieldNameIfNeeded(s string) string {
	return quoteTokenIfNeeded(s)
}
