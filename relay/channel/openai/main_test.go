package openai

import (
	"os"
	"testing"

	"github.com/QuantumNous/new-api/service"
)

// TestMain initializes the token encoders that the stream handlers fall back to
// when an upstream reports no usage. Production initializes them at startup;
// without this the estimate path dereferences a nil codec. The vocabularies are
// compiled into the tokenizer package, so this stays offline.
func TestMain(m *testing.M) {
	service.InitTokenEncoders()
	os.Exit(m.Run())
}
