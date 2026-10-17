package gateway

import (
	"context"
	"strings"
	"testing"
)

func TestPlaintextBackupRepoFailsClosed(t *testing.T) {
	s := &Server{BackupRepo: "rest:http://spade:pw@10.0.0.1:8000"}
	_, err := s.injectSecrets(context.Background(), []byte(`{"secret_ref":{"kind":"backup","service_id":"x"}}`))
	if err == nil || !strings.Contains(err.Error(), "plain HTTP") {
		t.Fatalf("credentials over http must be refused, got %v", err)
	}
}
