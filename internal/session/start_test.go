package session

import (
	"errors"
	"strings"
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

// The server's rejection text reaches the user's terminal through the error,
// so control characters in it are escaped and its length is bounded.
func TestStartRejectedTextIsQuotedAndBounded(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tr := newFakeTransport()
		tr.in <- initialResponse(t, "bad\x1b]0;pwned\x07"+strings.Repeat("x", 2*maxRejectText))
		err := Start(t.Context(), tr, Options{})
		if !errors.Is(err, ErrStartRejected) {
			t.Fatalf("Start = %v, want ErrStartRejected", err)
		}
		msg := err.Error()
		if strings.ContainsAny(msg, "\x1b\x07") {
			t.Fatalf("error text carries raw control characters: %q", msg)
		}
		if !strings.Contains(msg, `bad\x1b`) {
			t.Fatalf("error text %q does not show the escaped server text", msg)
		}
		if len(msg) > len(ErrStartRejected.Error())+maxRejectText+32 {
			t.Fatalf("error text is %d bytes, want it bounded near maxRejectText (%d)", len(msg), maxRejectText)
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
