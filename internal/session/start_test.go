package session

import (
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/tphakala/et-go/internal/protocol"
)

func initialResponse(t *testing.T, errText string) protocol.Packet {
	t.Helper()
	resp := &protocol.InitialResponse{}
	if errText != "" {
		resp.SetError(errText)
	}
	b, err := proto.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	return protocol.Packet{Header: protocol.HeaderInitialResponse, Payload: b}
}

func TestStartSendsPayloadAndAcceptsResponse(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tr := newFakeTransport()
		tr.in <- initialResponse(t, "")
		if err := Start(t.Context(), tr, Options{}); err != nil {
			t.Fatalf("Start: %v", err)
		}
		sent := tr.sentWith(protocol.HeaderInitialPayload)
		if len(sent) != 1 {
			t.Fatalf("sent %d INITIAL_PAYLOAD packets, want 1", len(sent))
		}
		payload := &protocol.InitialPayload{}
		if err := proto.Unmarshal(sent[0].Payload, payload); err != nil {
			t.Fatal(err)
		}
		if !payload.GetSupportsExitStatus() {
			t.Error("supports_exit_status not set")
		}
		if payload.GetJumphost() {
			t.Error("jumphost set, want false")
		}
	})
}

func TestStartRejected(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tr := newFakeTransport()
		tr.in <- initialResponse(t, "no such terminal")
		err := Start(t.Context(), tr, Options{})
		if !errors.Is(err, ErrStartRejected) {
			t.Fatalf("Start = %v, want ErrStartRejected", err)
		}
	})
}

func TestStartWrongPacket(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tr := newFakeTransport()
		tr.in <- serverOutput(t, "hello")
		err := Start(t.Context(), tr, Options{})
		if err == nil || errors.Is(err, ErrStartRejected) || errors.Is(err, ErrStartTimeout) {
			t.Fatalf("Start = %v, want a protocol error", err)
		}
	})
}

func TestStartTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tr := newFakeTransport()
		begin := time.Now()
		err := Start(t.Context(), tr, Options{})
		if !errors.Is(err, ErrStartTimeout) {
			t.Fatalf("Start = %v, want ErrStartTimeout", err)
		}
		if got := time.Since(begin); got != startTimeout {
			t.Fatalf("timed out after %v, want %v", got, startTimeout)
		}
	})
}
