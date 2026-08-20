package inprocess_test

import (
	"testing"

	agent "github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/interaction"
	"github.com/LyleLiu666/agentSlot/interaction/inprocess"
)

func TestEntrypointExposesOnlyTheAttachedGateway(t *testing.T) {
	entrypoint := inprocess.New()
	if _, err := entrypoint.Access(); !agent.IsCode(err, agent.CodeApplicationNotStarted) {
		t.Fatalf("Access before Attach error = %v, code=%q", err, agent.CodeOf(err))
	}
	gateway := &gatewayProbe{}
	if err := entrypoint.Attach(gateway); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	access, err := entrypoint.Access()
	if err != nil {
		t.Fatalf("Access: %v", err)
	}
	if access != gateway {
		t.Fatalf("Access returned %T, want original GatewayAccess", access)
	}
	if err := entrypoint.Attach(&gatewayProbe{}); err == nil {
		t.Fatal("second Attach succeeded")
	}
}

func TestEntrypointRejectsNilGateway(t *testing.T) {
	entrypoint := inprocess.New()
	if err := entrypoint.Attach(nil); err == nil {
		t.Fatal("nil GatewayAccess was accepted")
	}
	var typedNil *gatewayProbe
	if err := entrypoint.Attach(typedNil); err == nil {
		t.Fatal("typed-nil GatewayAccess was accepted")
	}
}

// Embedding the public interface keeps this probe focused on attachment. It
// also proves the Entrypoint does not require a private Runtime capability.
type gatewayProbe struct{ interaction.GatewayAccess }

var _ interaction.Entrypoint = inprocess.New()
