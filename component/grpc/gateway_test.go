package memql

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDecodeQueryRequestParsesQuery(t *testing.T) {
	t.Parallel()

	body := `{"query":"concept==v1:demo","variables":{"foo":"bar"}}`
	req := httptest.NewRequest(http.MethodPost, "/memql/query", strings.NewReader(body))

	decoded, err := decodeQueryRequest(req)
	require.NoError(t, err)
	require.NotNil(t, decoded)
	require.Equal(t, "concept==v1:demo", decoded.GetQuery())
	require.Equal(t, "bar", decoded.GetVariables().GetFields()["foo"].GetStringValue())
}

func TestDecodeQueryRequestDefaultsVariables(t *testing.T) {
	t.Parallel()

	body := `{"query":"concept==v1:demo"}`
	req := httptest.NewRequest(http.MethodPost, "/memql/query", strings.NewReader(body))

	decoded, err := decodeQueryRequest(req)
	require.NoError(t, err)
	require.NotNil(t, decoded)
	require.NotNil(t, decoded.GetVariables())
}
