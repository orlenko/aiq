package daemon

import (
	"context"
	"testing"
)

func TestIdentityContextExactHMAC(t *testing.T) {
	for _, row := range []struct{ id, want string }{
		{"", ""},
		{"invented-machine-id", "bdb4aa74e7e1acdee56fd4d5366981fb5700560ddd6cf2225919a0e138d4fed4"},
		{"00000000-0000-4000-8000-000000000001", "469a1fbf0f1391930e8f1741e361300198bff025efdb4f59cf4694d64d93caa7"},
	} {
		if identityValue(row.id) != row.want {
			t.Fatal("existing identity HMAC changed")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if value, err := IdentityContext(ctx); err == nil || value != "" {
		t.Fatal("canceled public identity did not refuse before source access")
	}
}
