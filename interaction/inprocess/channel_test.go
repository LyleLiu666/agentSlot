package inprocess_test

import (
	"testing"

	agent "github.com/LyleLiu666/agentSlot/agent"
	"github.com/LyleLiu666/agentSlot/interaction"
	"github.com/LyleLiu666/agentSlot/interaction/inprocess"
)

func TestChannelExposesOnlyTheBoundGateway(t *testing.T) {
	channel := inprocess.New()
	if _, err := channel.Access(); !agent.IsCode(err, agent.CodeApplicationNotStarted) {
		t.Fatalf("Access before Bind error = %v, code=%q", err, agent.CodeOf(err))
	}
	gateway := &gatewayProbe{}
	if err := channel.Bind(gateway); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	access, err := channel.Access()
	if err != nil {
		t.Fatalf("Access: %v", err)
	}
	if access != gateway {
		t.Fatalf("Access returned %T, want original GatewayAccess", access)
	}
	if err := channel.Bind(&gatewayProbe{}); err == nil {
		t.Fatal("second Bind succeeded")
	}
}

func TestChannelRejectsNilGateway(t *testing.T) {
	channel := inprocess.New()
	if err := channel.Bind(nil); err == nil {
		t.Fatal("nil GatewayAccess was accepted")
	}
	var typedNil *gatewayProbe
	if err := channel.Bind(typedNil); err == nil {
		t.Fatal("typed-nil GatewayAccess was accepted")
	}
}

// Embedding the public interface keeps this probe focused on attachment. It
// also proves the Channel does not require a private Runtime capability.
type gatewayProbe struct{ interaction.GatewayAccess }

var _ interaction.GatewayChannel = inprocess.New()
