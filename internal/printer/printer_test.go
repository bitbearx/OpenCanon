package printer

import "testing"

// TestNormalizeRanges 验证范围边界、合并和数字内空白。
func TestNormalizeRanges(t *testing.T) {
	tests := []struct {
		in, want string
		count    int
		bad      bool
	}{{"1-3,5", "1-3,5", 4, false}, {"1,1-3", "1-3", 3, false}, {" 1 - 3 ， 5 ", "1-3,5", 4, false}, {"0", "", 0, true}, {"6", "", 0, true}, {"3-1", "", 0, true}, {"1,,2", "", 0, true}, {"1 2", "", 0, true}}
	for _, tt := range tests {
		got, n, err := NormalizeRanges(tt.in, 5)
		if (err != nil) != tt.bad || (!tt.bad && (got != tt.want || n != tt.count)) {
			t.Errorf("%q: %q %d %v", tt.in, got, n, err)
		}
	}
}

// TestProfileConstraints 验证禁止组合和选项冲突。
func TestProfileConstraints(t *testing.T) {
	p := Profile{Queue: Queue, Media: []Media{{ID: "A4", Label: "A4", WidthMM: 210, HeightMM: 297, Options: map[string]string{"PageSize": "A4"}}}, ColorModes: []Mode{{ID: "color", Label: "彩色", Options: map[string]string{"Mode": "color"}}}, DuplexModes: []Mode{{ID: "single", Label: "单面"}, {ID: "long", Label: "双面", Options: map[string]string{"Mode": "other"}}}, Constraints: []Constraint{{MediaID: "A4", ColorMode: "color", Duplex: "single"}}}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Options("A4", "color", "single"); err == nil {
		t.Fatal("禁止组合被接受")
	}
	if _, err := p.Options("A4", "color", "long"); err == nil {
		t.Fatal("冲突选项被接受")
	}
}

// TestSheets 验证双面奇数页逐份计算。
func TestSheets(t *testing.T) {
	if got := Sheets(5, 3, "long"); got != 9 {
		t.Fatalf("got %d", got)
	}
	if got := Sheets(5, 3, "single"); got != 15 {
		t.Fatalf("got %d", got)
	}
}
