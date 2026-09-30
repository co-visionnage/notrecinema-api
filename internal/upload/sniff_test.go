package upload

import "testing"

func TestSniffImageType(t *testing.T) {
	tests := []struct {
		name string
		data []byte
		want string
		ok   bool
	}{
		{"png", []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, 0, 0}, "image/png", true},
		{"jpeg", []byte{0xFF, 0xD8, 0xFF, 0, 0}, "image/jpeg", true},
		{"gif87", []byte("GIF87a12345"), "image/gif", true},
		{"gif89", []byte("GIF89a12345"), "image/gif", true},
		{"webp", append([]byte("RIFF\x00\x00\x00\x00"), []byte("WEBP")...), "image/webp", true},
		{"svg rejected", []byte("<svg xmlns=..."), "", false},
		{"plain text rejected", []byte("not an image"), "", false},
		{"empty rejected", []byte{}, "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := SniffImageType(tt.data)
			if ok != tt.ok {
				t.Fatalf("ok = %v, want %v", ok, tt.ok)
			}
			if ok && got.MimeType != tt.want {
				t.Errorf("MimeType = %q, want %q", got.MimeType, tt.want)
			}
		})
	}
}
