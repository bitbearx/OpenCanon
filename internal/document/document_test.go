package document

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// TestHEIFBrands 验证完整品牌表和动画品牌处理。
func TestHEIFBrands(t *testing.T) {
	tests := []struct {
		name, major string
		compatible  []string
		want        bool
	}{
		{"long-static", "xxxx", []string{"aaaa", "bbbb", "cccc", "dddd", "eeee", "heic"}, true},
		{"static-multiple", "mif1", []string{"heic"}, true},
		{"sequence", "heic", []string{"msf1"}, false},
		{"hevc-sequence", "hevc", []string{"heic"}, false},
		{"avif-sequence", "mif1", []string{"avis"}, false},
		{"unknown", "xxxx", []string{"aaaa"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := make([]byte, 16+4*len(tt.compatible))
			binary.BigEndian.PutUint32(data, uint32(len(data)))
			copy(data[4:8], "ftyp")
			copy(data[8:12], tt.major)
			for i, b := range tt.compatible {
				copy(data[16+i*4:], b)
			}
			path := filepath.Join(t.TempDir(), "image")
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			kind, err := detect(path)
			if (err == nil) != tt.want {
				t.Fatalf("kind=%q err=%v", kind, err)
			}
		})
	}
}
