package metrics

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLabelValuesUseTextFormatEscapes(t *testing.T) {
	r := New()
	r.Inc("x_total", "source", "a\tb\"c\\d\ne")
	var b strings.Builder
	require.NoError(t, r.Write(&b, nil))
	assert.Contains(t, b.String(), "x_total{source=\"a\tb\\\"c\\\\d\\ne\"} 1\n")
}
