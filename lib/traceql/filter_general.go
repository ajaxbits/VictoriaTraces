package traceql

import (
	otelpb "github.com/VictoriaMetrics/VictoriaTraces/lib/protoparser/opentelemetry/pb"
	"strconv"
	"strings"
)

type filterCommon struct {
	fieldName string
	op        string
	value     string
}

func (fp *filterCommon) String() string {
	v := fp.value
	if duration, ok := tryParseDuration(v); ok {
		v = strconv.FormatInt(duration, 10)
	}
	return quoteFieldNameIfNeeded(fp.tagToVTField()) + ":" + fp.op + quoteTokenIfNeeded(v)
}

func (fp *filterCommon) tagToVTField() string {
	if strings.HasPrefix(fp.fieldName, "resource.") {
		return otelpb.ResourceAttrPrefix + fp.fieldName[len("resource."):]
	} else if strings.HasPrefix(fp.fieldName, "span.") {
		return otelpb.SpanAttrPrefixField + fp.fieldName[len("span."):]
	} else if strings.HasPrefix(fp.fieldName, "event.") {
		return otelpb.EventPrefix + otelpb.EventAttrPrefix + fp.fieldName[len("event."):]
	} else if strings.HasPrefix(fp.fieldName, "link.") {
		return otelpb.LinkPrefix + otelpb.LinkAttrPrefix + fp.fieldName[len("link."):]
	} else if strings.HasPrefix(fp.fieldName, "instrumentation.") {
		return otelpb.InstrumentationScopeAttrPrefix + fp.fieldName[len("instrumentation."):]
	} else if fp.fieldName == "status" {
		return otelpb.StatusCodeField
	}
	return fp.fieldName
}

func quoteFieldNameIfNeeded(s string) string {
	return quoteTokenIfNeeded(s)
}
