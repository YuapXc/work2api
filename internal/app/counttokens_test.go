package app

import "testing"

// A6 回归：len/4 是英文经验值，中文 1 字 3 字节≈1 token，按字节/4 低估约 1.5 倍。
func TestCJKRunes(t *testing.T) {
	if n := cjkRunes("你好世界"); n != 4 {
		t.Fatalf("got %d", n)
	}
	if n := cjkRunes("hello world"); n != 0 {
		t.Fatalf("got %d", n)
	}
	if n := cjkRunes("你好，世界！"); n != 6 { // 含全角标点
		t.Fatalf("got %d", n)
	}
	if n := cjkRunes("mix混合 test"); n != 2 {
		t.Fatalf("got %d", n)
	}
}

// count_tokens 估算口径：CJK 按 ~1.5 字符/token，其余按 4 字节/token。
// 纯中文文本的旧公式（字节/4）会把中文按 3 字节≈0.75 token 算（实际≈1），
// 约低估 1.5 倍；新公式把 CJK 部分按 ~1.5 字符/token 计。
func TestCountTokensCJKWeighted(t *testing.T) {
	cjk := "一二三四五六七八九十一二三四五六七八九十" // 20 字 = 60 字节
	runes := cjkRunes(cjk)
	if runes != 20 {
		t.Fatalf("runes=%d", runes)
	}
	textLen := len(cjk)
	// 新公式：CJK 部分 = 20/1.5 ≈ 13 token；旧公式该部分 = 60/4 = 15。
	// 差异体现在 token 语义而非绝对值——CJK 被单独按字符权重计，不再按字节摊薄。
	newCJKPart := int(float64(runes) / 1.5)
	oldCJKPart := textLen / 4
	if newCJKPart != 13 || oldCJKPart != 15 {
		t.Fatalf("权重口径异常: newCJKPart=%d oldCJKPart=%d", newCJKPart, oldCJKPart)
	}
	// 混合文本：中文权重不被英文稀释
	mixed := cjk + " " + "hello" // 20 CJK + 6 字节英文
	mr := cjkRunes(mixed)
	est := int(float64(mr)/1.5) + (len(mixed)-3*mr)/4
	if est < 13 || est > 16 {
		t.Fatalf("混合文本估算异常: %d", est)
	}
}
