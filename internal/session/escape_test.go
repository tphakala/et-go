package session

import "testing"

func TestEscapeFilter(t *testing.T) {
	tests := []struct {
		name   string
		chunks []string // fed to one filter in order
		want   string
		detach bool
	}{
		{name: "plain text passes", chunks: []string{"ls -l\r"}, want: "ls -l\r"},
		{name: "detach at start of input", chunks: []string{"~."}, want: "", detach: true},
		{name: "detach after CR", chunks: []string{"ls\r~."}, want: "ls\r", detach: true},
		{name: "detach after LF", chunks: []string{"ls\n~.junk"}, want: "ls\n", detach: true},
		{name: "tilde mid line is literal", chunks: []string{"a~."}, want: "a~."},
		{name: "double tilde sends one", chunks: []string{"~~"}, want: "~"},
		{name: "double tilde then dot is literal", chunks: []string{"~~."}, want: "~."},
		{name: "tilde then other byte sends both", chunks: []string{"~x"}, want: "~x"},
		{name: "tilde then CR keeps line start", chunks: []string{"~\r~."}, want: "~\r", detach: true},
		{name: "escape split across reads", chunks: []string{"ls\r~", "."}, want: "ls\r", detach: true},
		{name: "held tilde released by next read", chunks: []string{"~", "a"}, want: "~a"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var f escapeFilter
			var got []byte
			var detach bool
			for _, c := range tt.chunks {
				got, detach = f.filter(got, []byte(c))
				if detach {
					break
				}
			}
			if string(got) != tt.want || detach != tt.detach {
				t.Fatalf("filter(%q) = %q, detach %v; want %q, detach %v",
					tt.chunks, got, detach, tt.want, tt.detach)
			}
		})
	}
}
