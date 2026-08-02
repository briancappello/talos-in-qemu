package cluster

import (
	"strings"
	"testing"
)

func TestNodeVersionRefusesAnEmptyEndpoint(t *testing.T) {
	_, err := NodeVersion(t.Context(), "")
	if err == nil {
		t.Fatal("NodeVersion accepted an empty endpoint\n" +
			"  reason: an empty address spends the caller's whole timeout proving " +
			"that \"\" is not an address")
	}

	if !strings.Contains(err.Error(), "host:port") {
		t.Errorf("error does not say what shape an endpoint has: %s", err)
	}
}
