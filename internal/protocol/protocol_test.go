package protocol

import (
	"testing"

	"google.golang.org/protobuf/proto"
)

// TestHeaderMatchesGeneratedEnums pins every hand-written Header constant to
// the upstream enum value it mirrors, so a change in the .proto files cannot
// drift silently.
func TestHeaderMatchesGeneratedEnums(t *testing.T) {
	tests := []struct {
		name string
		got  Header
		want int32
	}{
		{"KeepAlive", HeaderKeepAlive, int32(TerminalPacketType_KEEP_ALIVE)},
		{"TerminalBuffer", HeaderTerminalBuffer, int32(TerminalPacketType_TERMINAL_BUFFER)},
		{"TerminalInfo", HeaderTerminalInfo, int32(TerminalPacketType_TERMINAL_INFO)},
		{"TerminalClose", HeaderTerminalClose, int32(TerminalPacketType_TERMINAL_CLOSE)},
		{"TerminalExitStatus", HeaderTerminalExitStatus, int32(TerminalPacketType_TERMINAL_EXIT_STATUS)},
		{"InitialResponse", HeaderInitialResponse, int32(EtPacketType_INITIAL_RESPONSE)},
		{"InitialPayload", HeaderInitialPayload, int32(EtPacketType_INITIAL_PAYLOAD)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if int32(tt.got) != tt.want {
				t.Errorf("Header%s = %d, generated enum = %d", tt.name, tt.got, tt.want)
			}
		})
	}
}

func TestHeaderString(t *testing.T) {
	tests := []struct {
		h    Header
		want string
	}{
		{HeaderKeepAlive, "KeepAlive"},
		{HeaderTerminalBuffer, "TerminalBuffer"},
		{HeaderTerminalInfo, "TerminalInfo"},
		{HeaderTerminalClose, "TerminalClose"},
		{HeaderTerminalExitStatus, "TerminalExitStatus"},
		{HeaderInitialPayload, "InitialPayload"},
		{Header(99), "Header(99)"},
	}
	for _, tt := range tests {
		if got := tt.h.String(); got != tt.want {
			t.Errorf("Header(%d).String() = %q, want %q", uint8(tt.h), got, tt.want)
		}
	}
}

// TestConnectRequestRoundTrip checks the opaque-API setters and getters on a
// message the client sends first on every connection.
func TestConnectRequestRoundTrip(t *testing.T) {
	req := &ConnectRequest{}
	req.SetClientId("XXXabcdefghijklm")
	req.SetVersion(Version)

	b, err := proto.Marshal(req)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var got ConnectRequest
	if err := proto.Unmarshal(b, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.GetClientId() != "XXXabcdefghijklm" || got.GetVersion() != Version {
		t.Errorf("round trip = (%q, %d), want (%q, %d)", got.GetClientId(), got.GetVersion(), "XXXabcdefghijklm", Version)
	}
}
